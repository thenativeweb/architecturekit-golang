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
// The request names the revision it needs in WaitForRevision; the response
// says in its ETag which revision it actually shows. Both headers carry the
// same kind of value, but they mean different things, which is why the
// standard If-None-Match is not used for the waiting: an ETag is opaque and
// compares only for equality, while a revision is ordered. A server that is
// behind would answer "not equal, here you go" and hand out stale data.
const (
	// HeaderWaitFor is what a caller sends to name the revision it needs.
	HeaderWaitFor = "Wait-For-Revision"

	// HeaderRevision repeats the served revision, next to the ETag, so that a
	// caller can read it without treating the ETag as anything but opaque.
	HeaderRevision = "X-Revision"
)

// DefaultWait is how long a query waits for its revision before answering with
// what it has.
const DefaultWait = 5 * time.Second

// Await waits for the revision the request asked for.
//
// It returns nil when the view reached the revision and when the request asked
// for none. Running out of time is not an error either: the caller answers
// with what the view has, and the ETag says which revision that is. Only a
// revision that cannot be read as one is refused, because that is a mistake in
// the request rather than a slow projection.
func Await(
	ctx context.Context,
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

	waiting, cancel := context.WithTimeout(ctx, wait)
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

// ServeUnchanged answers 304 when the caller already holds this revision of
// this resource, and reports whether it did. Use it after Await, because
// waiting is what changes the answer.
//
// Its tag describes the resource, the revision and what varies, but not the
// query, which it never sees. Use it only for an answer that is the same for
// every caller, or make varies return whatever tells callers apart; otherwise
// one caller is told that nothing has changed and keeps the answer of another.
// QueryRevisioned and QueryVarying put the query into the tag themselves.
func ServeUnchanged(
	w http.ResponseWriter,
	r *http.Request,
	revision string,
	varies Volatile,
) bool {
	return serveUnchanged(w, r, revision, etagOf(r, revision, nil, varies))
}

func serveUnchanged(w http.ResponseWriter, r *http.Request, revision, tag string) bool {
	if tag == "" || r.Header.Get("If-None-Match") != tag {
		return false
	}

	writeRevision(w, revision, tag)
	w.WriteHeader(http.StatusNotModified)

	return true
}

// RespondResultAt writes a query result and says which revision it shows. It
// maps errors, and logs them, the way RespondResult does. Its tag is the one
// ServeUnchanged checks, with the same limits.
func RespondResultAt[TUser any, TResult any](
	w http.ResponseWriter,
	r *http.Request,
	api *API[TUser],
	revision string,
	result TResult,
	err error,
	varies Volatile,
) {
	respondResultAt(w, revision, etagOf(r, revision, nil, varies), result, err, api.logFailure(r))
}

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

// QueryRevisioned wires a query that can be asked for a revision. It waits for
// what the caller asked for, answers 304 when nothing changed, and tags the
// answer with the revision it served.
//
// The tag holds the query, so two callers get the same tag only if they ask the
// same: a query that holds the user, or anything else that tells callers
// apart, gets a tag of its own for each of them. That is enough as long as the
// answer depends on nothing but the query and the view, which is why answer
// sees neither the request nor the user. Three things get past it, and each
// has to be dealt with where it comes in:
//
//   - The clock: an answer that depends on the time, such as everything due
//     today, uses QueryVarying and says so, or callers are told that nothing
//     has changed when it has.
//   - Another view: the revision is that of the view handed over, so an answer
//     that also reads from another view does not notice when that one
//     changes.
//   - The context: a value that a middleware put into the context, such as the
//     user, never shows up in the tag. Put it into the query instead.
//
// The query is built before anything waits or is answered, since building it
// determines the caller and checks what they may ask: nobody can make the
// server wait, or learn that an answer is unchanged, without being allowed to
// ask. Answers are marked private, so that a shared cache does not keep them.
//
// Use Await, ServeUnchanged and RespondResultAt directly when you need a
// different shape.
func QueryRevisioned[TUser any, TQuery any, TResult any](
	api *API[TUser],
	mux *http.ServeMux,
	pattern string,
	view architecturekit.Revisioned,
	toQuery ToQuery[TUser, TQuery],
	answer Answer[TQuery, TResult],
	wait time.Duration,
) {
	QueryVarying(api, mux, pattern, view, toQuery, answer, wait, nil)
}

// QueryVarying is QueryRevisioned for an answer that depends on more than the
// read model. See Volatile.
func QueryVarying[TUser any, TQuery any, TResult any](
	api *API[TUser],
	mux *http.ServeMux,
	pattern string,
	view architecturekit.Revisioned,
	toQuery ToQuery[TUser, TQuery],
	answer Answer[TQuery, TResult],
	wait time.Duration,
	varies Volatile,
) {
	mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		if err := Await(r.Context(), r, view, wait); err != nil {
			respondResult(w, struct{}{}, err, logFailure)
			return
		}

		// The revision is read once, after waiting, so that the answer and its
		// tag describe the same state even if the projection moves on.
		revision := view.Revision()
		tag := etagOf(r, revision, func(w io.Writer) bool { return spellOut(w, query) }, varies)

		if serveUnchanged(w, r, revision, tag) {
			return
		}

		result, err := answer(r.Context(), query)
		respondResultAt(w, revision, tag, result, err, logFailure)
	}))
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

// etagOf ties the revision to the resource it describes and, when it is given
// one, to the query that was asked. Every query over the same view shares a
// revision, so a tag that held nothing else would match across resources and
// across callers -- and a caller that sent a tag it got elsewhere would be
// told, wrongly, that nothing had changed.
//
// It returns no tag for a view that has seen nothing, and for a query that can
// not be spelled out (see spellOut).
func etagOf(r *http.Request, revision string, asked func(io.Writer) bool, varies Volatile) string {
	if revision == "" {
		return ""
	}

	resource := fnv.New64a()
	_, _ = io.WriteString(resource, r.URL.Path)
	_, _ = io.WriteString(resource, "?")
	_, _ = io.WriteString(resource, r.URL.RawQuery)

	if asked != nil {
		_, _ = io.WriteString(resource, "\x00")

		if !asked(resource) {
			return ""
		}
	}

	if varies != nil {
		_, _ = io.WriteString(resource, "\x00")
		_, _ = io.WriteString(resource, varies(r))
	}

	return fmt.Sprintf(`"%s-%x"`, revision, resource.Sum64())
}
