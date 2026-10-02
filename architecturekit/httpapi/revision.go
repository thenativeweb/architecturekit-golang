package httpapi

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"time"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

// A caller that has just written something knows the revision its write
// produced, and can ask to see at least that much before it reads again. That
// turns "wait a moment and hope" into a condition the server can check.
//
// The request names the revision it needs in the Wait-For-Revision header; the
// response says in its ETag which revision it actually shows. Both headers carry the
// same kind of value, but they mean different things, which is why the
// standard If-None-Match is not used for the waiting: an ETag is opaque and
// compares only for equality, while a revision is ordered. A server that is
// behind would answer "not equal, here you go" and hand out stale data.
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
// with what the view has, and the ETag says which revision that is. Only a
// revision that cannot be read as one is refused, because that is a mistake in
// the request rather than a slow projection.
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
		return fmt.Errorf("%w: %v", ErrMalformed, err)
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
// the answer follows from the stored events alone -- and it stops being
// enough the moment anything outside them takes part. "Everything due today"
// is the plain case: the same events mean something different after
// midnight, without a single event being written. A tag built from the
// revision alone would then claim that nothing had changed, and the caller
// would keep yesterday's answer for as long as nothing else happened.
//
// Returning the current day is usually all it takes.
type Volatile func(*http.Request) string

// serveUnchanged answers 304 when the caller already holds the answer with
// the given tag, and reports whether it did.
func serveUnchanged(w http.ResponseWriter, r *http.Request, revision, tag string) bool {
	if tag == "" || r.Header.Get("If-None-Match") != tag {
		return false
	}

	writeRevision(w, revision, tag)
	w.WriteHeader(http.StatusNotModified)

	return true
}

// respondResultAt writes a query result with the revision it shows, and logs
// an internal failure with logFailure.
func respondResultAt[TResult any](
	w http.ResponseWriter,
	revision string,
	tag string,
	result TResult,
	err error,
	logFailure func(status int, err error),
) {
	if err == nil {
		writeRevision(w, revision, tag)
	}

	respondResult(w, result, err, logFailure)
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
		logFailure := api.logFailure(r)

		user, err := UserOf(r, api)
		if err != nil {
			respondResult(w, struct{}{}, err, logFailure)
			return
		}

		query, err := toQuery(r, user)
		if err != nil {
			respondResult(w, struct{}{}, categorise(err), logFailure)
			return
		}

		if err := Await(r, settings.view, settings.wait); err != nil {
			respondResult(w, struct{}{}, err, logFailure)
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
		respondResultAt(w, revision, tag, result, err, logFailure)
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
// held nothing else would match across resources and across callers -- and a
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
