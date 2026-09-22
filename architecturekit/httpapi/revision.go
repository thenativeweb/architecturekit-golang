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

// Volatile says what an answer depends on besides the revision and the
// resource, as a string that changes when the answer would.
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
func ServeUnchanged(
	w http.ResponseWriter,
	r *http.Request,
	revision string,
	varies Volatile,
) bool {
	if revision == "" || r.Header.Get("If-None-Match") != etagOf(r, revision, varies) {
		return false
	}

	writeRevision(w, r, revision, varies)
	w.WriteHeader(http.StatusNotModified)

	return true
}

// RespondResultAt writes a query result and says which revision it shows. It
// maps errors the way RespondResult does.
func RespondResultAt[TResult any](
	w http.ResponseWriter,
	r *http.Request,
	revision string,
	result TResult,
	err error,
	varies Volatile,
) {
	if err == nil {
		writeRevision(w, r, revision, varies)
	}

	RespondResult(w, result, err)
}

// QueryRevisioned wires a query that can be asked for a revision. It waits for
// what the caller asked for, answers 304 when nothing changed, and tags the
// answer with the revision it served.
//
// It assumes the answer follows from the read model alone. When it does not --
// when the clock or anything else outside the events takes part -- use
// QueryVarying and say so, or callers will be told that nothing has changed
// when it has.
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
		// The caller is determined before anything waits, so that nobody can
		// bind waiting time on the server without being allowed in.
		if _, err := UserOf(r, api); err != nil {
			RespondResult(w, struct{}{}, err)
			return
		}

		if err := Await(r.Context(), r, view, wait); err != nil {
			RespondResult(w, struct{}{}, err)
			return
		}

		// The revision is read once, after waiting, so that the answer and its
		// tag describe the same state even if the projection moves on.
		revision := view.Revision()

		if ServeUnchanged(w, r, revision, varies) {
			return
		}

		result, err := Ask(r, api, toQuery, answer)
		RespondResultAt(w, r, revision, result, err, varies)
	}))
}

func writeRevision(w http.ResponseWriter, r *http.Request, revision string, varies Volatile) {
	if revision == "" {
		return
	}

	// Without this a browser is free to decide for itself how long the answer
	// stays good, and it will not ask again until it has. The tag still saves
	// the body when nothing has changed; this only insists that it asks.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etagOf(r, revision, varies))
	w.Header().Set(HeaderRevision, revision)
}

// etagOf ties the revision to the resource it describes. Every query over the
// same view shares a revision, so a tag that held nothing else would match
// across resources -- and a caller that sent one query's tag to another would
// be told, wrongly, that nothing had changed.
func etagOf(r *http.Request, revision string, varies Volatile) string {
	resource := fnv.New64a()
	_, _ = io.WriteString(resource, r.URL.Path)
	_, _ = io.WriteString(resource, "?")
	_, _ = io.WriteString(resource, r.URL.RawQuery)

	if varies != nil {
		_, _ = io.WriteString(resource, "\x00")
		_, _ = io.WriteString(resource, varies(r))
	}

	return fmt.Sprintf(`"%s-%x"`, revision, resource.Sum64())
}
