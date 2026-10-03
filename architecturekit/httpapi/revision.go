package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

// A caller that has just written something knows the revision its write
// produced, and can ask to see at least that much before it reads again. That
// turns "wait a moment and hope" into a condition the server can check.
//
// The request names the revision it needs in the Wait-For-Revision header; the
// response says in the Revision header which revision it actually shows, and
// builds its ETag from it. The ETag means something other than the revision,
// though, which is why the standard If-None-Match is not used for the waiting:
// an ETag is opaque and compares only for equality, while a revision is
// ordered. A server that is behind would answer "not equal, here you go" and
// hand out stale data.
const (
	// HeaderWaitFor is what a caller sends to name the revision it needs.
	HeaderWaitFor = "Wait-For-Revision"

	// HeaderRevision repeats the served revision, next to the ETag, so that a
	// caller can read it without treating the ETag as anything but opaque. It
	// has no X- prefix, which RFC 6648 advises against for new headers.
	HeaderRevision = "Revision"
)

// DefaultWait is how long a query waits for its revision before answering with
// what it has.
const DefaultWait = 5 * time.Second

// Await waits for the revision the request asked for, for at most the given
// time, and within the context of the request. Use it in a handler of your own
// that reads its own writes.
//
// It returns nil when the view reached the revision and when the request asked
// for none. Running out of time is not an error either: the caller answers
// with what the view has. Neither is the end of the context of the request,
// for example because the caller went away: Await stops waiting then, and
// returns nil as well. Only a revision that cannot be read as one is refused,
// because that is a mistake in the request rather than a slow projection. The
// error is ErrMalformed, and wraps architecturekit.ErrNotARevision. Any other
// error of the view is returned as it is.
func Await(
	r *http.Request,
	view architecturekit.Revisioned,
	wait time.Duration,
) error {
	wanted := r.Header.Get(HeaderWaitFor)
	if wanted == "" {
		return nil
	}

	// A revision that is not one would otherwise wait for the full timeout and
	// then answer as if nothing were wrong.
	if _, err := architecturekit.CompareRevisions(wanted, "0"); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformed, err)
	}

	waiting, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()

	if err := view.WaitFor(waiting, wanted); err != nil && waiting.Err() == nil {
		return err
	}

	return nil
}

// Volatile says what an answer depends on besides the revision and the query,
// as a string that changes when the answer would.
//
// A revision describes the read model and nothing else. That is enough while
// the answer follows from the stored events alone, and it stops being
// enough the moment anything outside them takes part. "Everything due today"
// is the plain case: the same events mean something different after
// midnight, without a single event being written. A tag built from the
// revision alone would then claim that nothing had changed, and the caller
// would keep yesterday's answer for as long as nothing else happened.
//
// Returning the current day is usually all it takes.
type Volatile func(*http.Request) string

// methodQuery is QUERY, a method that asks with a body and changes nothing,
// like GET (draft-ietf-httpbis-safe-method-w-body). net/http has no name for
// it yet.
const methodQuery = "QUERY"

// serveUnchanged answers 304 when the caller already holds the answer with
// the given tag, or 412 for a method other than GET, HEAD and QUERY, and
// reports whether it did.
//
// HTTP has 304 for GET and HEAD (RFC 9110, 13.1.2), and for QUERY, which it
// treats like GET, and 412 for every other method, such as POST, which a
// query whose input does not fit into the query string is sent with. Both
// carry the tag and the revision of the current answer, and only 412 has a
// body, which is a message, as with any other answer that is not a success.
func serveUnchanged(w http.ResponseWriter, r *http.Request, revision, tag string) bool {
	if tag == "" || !holdsTag(r, tag) {
		return false
	}

	writeRevision(w, revision, tag)

	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == methodQuery {
		w.WriteHeader(http.StatusNotModified)
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message": "precondition failed: the current answer matches If-None-Match",
		})
	}

	return true
}

// holdsTag reports whether the If-None-Match header of a request names the
// tag, compared the way HTTP has it for this header (RFC 9110, 13.1.2).
//
// The header holds a list of tags, separated by commas, which may be spread
// over several lines, or *, which names every tag. The comparison is weak, so
// W/"a" names "a" as well: a proxy that compresses an answer marks its tag as
// weak, since the bytes are no longer the same, and the caller then sends it
// back that way. A tag can contain a comma, but no quote, so the list is read
// from quote to quote rather than split at the commas.
func holdsTag(r *http.Request, tag string) bool {
	list := strings.Join(r.Header.Values("If-None-Match"), ",")

	for {
		list = strings.TrimLeft(list, " \t,")
		if strings.HasPrefix(list, "*") {
			return true
		}

		quoted, isQuoted := strings.CutPrefix(strings.TrimPrefix(list, "W/"), `"`)
		opaque, rest, isClosed := strings.Cut(quoted, `"`)
		if !isQuoted || !isClosed {
			return false
		}
		if `"`+opaque+`"` == tag {
			return true
		}

		list = rest
	}
}

// answerRevisioned answers a query that can be asked for a revision (see
// Revisioned).
func answerRevisioned[TUser any, TQuery any, TResult any](
	api *API[TUser],
	toQuery ToQuery[TUser, TQuery],
	answer Answer[TQuery, TResult],
	settings querySettings,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer api.answerPanic(w, r)

		explain := api.explain(r)

		user, err := UserOf(r, api)
		if err != nil {
			respondResult(w, struct{}{}, err, explain)
			return
		}

		query, err := toQuery(r, user)
		if err != nil {
			respondResult(w, struct{}{}, categorise(err), explain)
			return
		}

		if err := Await(r, settings.view, settings.wait); err != nil {
			respondResult(w, struct{}{}, err, explain)
			return
		}

		// The revision is read once, after waiting, so that the answer and its
		// tag describe the same state even if the projection moves on.
		revision := settings.view.Revision()
		tag := etagOf(r, revision, query, settings.varies)

		if serveUnchanged(w, r, revision, tag) {
			return
		}

		result, err := answer(r.Context(), query)
		respondResultAt(w, revision, tag, result, err, explain)
	})
}

func writeRevision(w http.ResponseWriter, revision, tag string) {
	if revision == "" {
		return
	}

	// Without no-cache a browser is free to decide for itself how long the
	// answer stays good, and it will not ask again until it has. The tag still
	// saves the body when nothing has changed; this only insists that it asks.
	//
	// Private keeps shared caches, such as proxies, from keeping the answer at
	// all. Whether an answer is the same for everybody is something only the
	// application knows, and even a public one may differ between anonymous
	// callers, so the kit does not guess.
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set(HeaderRevision, revision)

	if tag != "" {
		w.Header().Set("ETag", tag)
	}
}

// etagOf ties the revision to the resource it describes and to the query that
// was asked. Every query over the same view shares a revision, so a tag that
// held nothing else would match across resources and across callers, and a
// caller that sent a tag it got elsewhere would be told, wrongly, that nothing
// had changed.
//
// It returns no tag for a view that has seen nothing, and for a query that can
// not be spelled out (see spellOut).
func etagOf(r *http.Request, revision string, query any, varies Volatile) string {
	if revision == "" {
		return ""
	}

	resource := fnv.New64a()
	_, _ = io.WriteString(resource, r.URL.Path)
	_, _ = io.WriteString(resource, "?")
	_, _ = io.WriteString(resource, r.URL.RawQuery)
	_, _ = io.WriteString(resource, "\x00")

	if !spellOut(resource, query) {
		return ""
	}

	if varies != nil {
		_, _ = io.WriteString(resource, "\x00")
		_, _ = io.WriteString(resource, varies(r))
	}

	return fmt.Sprintf(`"%s-%x"`, revision, resource.Sum64())
}
