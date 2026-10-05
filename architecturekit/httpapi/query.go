package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

// ErrNotFound means the query asked for something that does not exist.
var ErrNotFound = errors.New("not found")

// ToQuery turns request data and the user into a query. It is the read
// side's counterpart to ToCommand, without a decoded body, since a query
// usually reads from the URL. One whose input does not fit there reads the
// body itself, with BodyOf.
//
// Its errors are treated as those of ToCommand: one that StatusFor maps to a
// status of its own keeps it, and so does one of the category
// architecturekit.ErrPermanent, while any other error comes back wrapped with
// ErrMalformed, and is answered with 400.
type ToQuery[TUser any, TQuery any] func(r *http.Request, user TUser) (TQuery, error)

// Answer answers a query. It sees neither the request nor HTTP, which is the
// whole point of splitting it from ToQuery.
type Answer[TQuery any, TResult any] func(ctx context.Context, query TQuery) (TResult, error)

// Ask determines the caller, builds the query and answers it, without writing
// anything to the response. Use it to answer in a format of your own.
//
// A panic on the way comes back as an error that StatusFor maps to 500, and
// that RespondResult logs with the value and the stack of the panic. Only
// http.ErrAbortHandler panics on, since net/http expects it to abort the
// response.
//
// A nil API, toQuery, or answer is a programming error, so Ask panics, and
// does so first, on every request, also one whose caller is unknown. Like any
// other panic, that comes back as an error, which names the mistake rather
// than a nil pointer.
func Ask[TUser any, TQuery any, TResult any](
	r *http.Request,
	api *API[TUser],
	toQuery ToQuery[TUser, TQuery],
	answer Answer[TQuery, TResult],
) (result TResult, err error) {
	defer recoverInto(&err)

	if api == nil {
		panic("architecturekit/httpapi: Ask needs the API, not nil")
	}
	if toQuery == nil {
		panic("architecturekit/httpapi: Ask needs a function that turns the request into a query, not nil")
	}
	if answer == nil {
		panic("architecturekit/httpapi: Ask needs a function that answers the query, not nil")
	}

	user, err := UserOf(r, api)
	if err != nil {
		return result, err
	}

	query, err := toQuery(r, user)
	if err != nil {
		return result, categorise(err)
	}

	return answer(r.Context(), query)
}

// QueryOption configures a query that Query wires up.
type QueryOption func(*querySettings)

type querySettings struct {
	view   architecturekit.Revisioned
	wait   time.Duration
	varies Volatile
}

// Revisioned has a query wait for the revision a caller asks for, for at most
// the given time (DefaultWait, unless there is a reason for another), answer
// 304 when nothing has changed, or 412 for a method other than GET, HEAD and
// QUERY, and tag the answer with the revision of the view it served.
//
// Whether nothing has changed, it tells from If-None-Match, which it reads
// the way HTTP has it: as a list of tags, or *, compared weakly, so that a tag
// that a proxy marked as weak while compressing the answer still matches.
//
// HTTP has 304 for GET and HEAD (RFC 9110, 13.1.2), and for QUERY, a method
// that asks with a body and changes nothing, which it treats like GET
// (draft-ietf-httpbis-safe-method-w-body), and 412 for every other method. So
// a query that is sent as POST, since its input does not fit into the query
// string, is answered with 412 when nothing has changed. It carries the tag
// and the revision, as 304 does, and a message.
//
// The tag holds the query, so two callers get the same tag only if they ask
// the same: a query that holds the user, or anything else that tells callers
// apart, gets a tag of its own for each of them. That is enough as long as the
// answer depends on nothing but the query and the view, which is why answer
// sees neither the request nor the user. Three things get past it, and each
// has to be dealt with where it comes in:
//
//   - The clock: an answer that depends on the time, such as everything due
//     today, puts the time into the query or adds Varying, or callers are told
//     that nothing has changed when it has. Either holds the time only as
//     precisely as the answer depends on it, such as the day: an instant
//     makes every tag unique, so that nothing is ever answered with 304.
//   - Another view: the revision is that of the view handed over, so an answer
//     that also reads from another view does not notice when that one
//     changes.
//   - The context: a value that a middleware put into the context, such as the
//     user, never shows up in the tag. Put it into the query instead.
//
// The query is built before anything waits or is answered, since building it
// determines the caller and checks what they may ask: nobody can make the
// server wait, or learn that an answer is unchanged, without being allowed to
// ask.
//
// An answer that carries a revision, a success or one that says that nothing
// has changed, says Cache-Control: private, no-cache: a cache asks again
// before it hands it out, and private keeps shared caches, such as proxies,
// from keeping it at all. Any other answer, such as a failure, or a success of
// a view that has seen nothing, says no-store, as every other answer of the
// kit does.
//
// A nil view, a negative wait, or giving Revisioned twice, is a programming
// error, so it panics. A nil pointer counts as a nil view, such as a view that
// was declared but never created.
func Revisioned(view architecturekit.Revisioned, wait time.Duration) QueryOption {
	if isNil(view) {
		panic("architecturekit/httpapi: Revisioned needs a view, not nil")
	}
	if wait < 0 {
		panic(fmt.Sprintf("architecturekit/httpapi: Revisioned needs a wait that is not negative, not %s", wait))
	}

	return func(settings *querySettings) {
		if settings.view != nil {
			panic("architecturekit/httpapi: Revisioned is given twice")
		}

		settings.view = view
		settings.wait = wait
	}
}

// isNil reports whether a value that is handed over as an interface is nil,
// also when it is a nil pointer, map, slice, function, or channel of a
// concrete type, such as a view that was declared but never created. Such a
// value is not equal to nil, since the interface knows its type, but it fails
// as soon as it is used. It uses reflection, so call it only while wiring.
//
// An interface never shows up as the kind, since reflect.ValueOf unpacks it.
// The package architecturekit has the same function, which it does not export,
// so that it does not become part of what the kit offers.
func isNil(value any) bool {
	if value == nil {
		return true
	}

	switch reflected := reflect.ValueOf(value); reflected.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return reflected.IsNil()
	default:
		return false
	}
}

// Varying adds to the tag of a revisioned query what its answer depends on
// besides the view and the query, such as the current day, which the answer
// takes from the clock itself (see Volatile).
//
// A nil function, giving Varying twice, or Varying without Revisioned, is a
// programming error, so it panics; the last one when Query wires the query.
func Varying(varies Volatile) QueryOption {
	if varies == nil {
		panic("architecturekit/httpapi: Varying needs a function, not nil")
	}

	return func(settings *querySettings) {
		if settings.varies != nil {
			panic("architecturekit/httpapi: Varying is given twice")
		}

		settings.varies = varies
	}
}

// Query wires a query to the mux and answers in the kit's default format,
// which is the result itself, with Cache-Control: no-store, so that no cache
// keeps it. With Revisioned, it reads its own writes and answers 304 when
// nothing has changed, or 412 for a method other than GET, HEAD and QUERY;
// with Varying in addition, its tag changes with what the answer takes from
// elsewhere.
//
// A panic while it handles a request is answered with 500, like any other
// internal failure, and logged with its value and its stack, as with Route.
// So is a result that can not be encoded, such as one that holds NaN, which
// JSON has no number for.
//
// A nil API, toQuery, or answer is a programming error, so Query panics, as
// Route does, rather than failing every request, with 500, or for a nil API,
// with no answer at all.
func Query[TUser any, TQuery any, TResult any](
	api *API[TUser],
	mux *http.ServeMux,
	pattern string,
	toQuery ToQuery[TUser, TQuery],
	answer Answer[TQuery, TResult],
	options ...QueryOption,
) {
	if api == nil {
		panic("architecturekit/httpapi: Query needs the API, not nil")
	}
	if toQuery == nil {
		panic("architecturekit/httpapi: Query needs a function that turns the request into a query, not nil")
	}
	if answer == nil {
		panic("architecturekit/httpapi: Query needs a function that answers the query, not nil")
	}

	var settings querySettings
	for _, option := range options {
		option(&settings)
	}

	if settings.view == nil {
		if settings.varies != nil {
			panic("architecturekit/httpapi: Varying needs Revisioned, since only a revisioned query has a tag")
		}

		mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer api.answerPanic(w, r)

			result, err := Ask(r, api, toQuery, answer)
			respondResult(w, result, err, api.explain(r))
		}))

		return
	}

	mux.Handle(pattern, answerRevisioned(api, toQuery, answer, settings))
}

// RespondResult writes a query result, or answers the error the way Respond
// does for commands, with the same messages: a fixed one for 401, 409, a
// query that found no item, and 500 and above, while the error is logged
// through the logger of the API, with the route of the request (see
// WithLogger), and the error itself otherwise. A result that can not be
// encoded, such as one that holds NaN, is answered with 500 as well. Like
// Respond, it says Cache-Control: no-store, so that no cache keeps the answer.
//
// As with Respond, an error without a status of its own is answered with 500,
// so wrap a mistake in the request that a handler of your own has found with
// ErrMalformed, to answer it with 400.
func RespondResult[TUser any, TResult any](
	w http.ResponseWriter,
	r *http.Request,
	api *API[TUser],
	result TResult,
	err error,
) {
	respondResult(w, result, err, api.explain(r))
}

// respondResult writes a query result, and explains an error with explain.
func respondResult[TResult any](
	w http.ResponseWriter,
	result TResult,
	err error,
	explain func(status int, err error) string,
) {
	respondResultAt(w, "", "", result, err, explain)
}

// respondResultAt writes a query result with the revision it shows, if there
// is one (see writeRevision), and explains an error with explain.
//
// An answer without a revision has no tag that a cache could ask about, and
// it may be the outcome of a command, or hold what only its caller may see.
// Without a word on caching, HTTP lets a cache keep it for a while it picks
// itself, and hand it out again, so it says that no cache may keep it.
//
// The result is encoded before anything is written, so that a result that can
// not be encoded is still answered with 500, and without the revision of an
// answer that never came. That holds for an error while it is encoded, such as
// for NaN, which JSON has no number for, and for a panic, such as one in a
// MarshalJSON function.
func respondResultAt[TResult any](
	w http.ResponseWriter,
	revision string,
	tag string,
	result TResult,
	err error,
	explain func(status int, err error) string,
) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	var body bytes.Buffer
	if err == nil {
		// The error is wrapped with %v rather than %w, since a result that can not
		// be encoded is a mistake in the code, which has to be answered with 500,
		// whatever category the error of a MarshalJSON function has.
		if failure := json.NewEncoder(&body).Encode(listOf(result)); failure != nil {
			err = fmt.Errorf("httpapi: encoding the result: %v", failure)
		}
	}

	if err == nil {
		writeRevision(w, revision, tag)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body.Bytes())
		return
	}

	status := StatusFor(err)
	message := explain(status, err)

	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}

// listOf turns a nil slice into an empty one, so that a query that finds
// nothing answers with [] rather than null. query.Collect, which the kit
// suggests for turning items into a slice, returns nil when there are none.
func listOf(result any) any {
	value := reflect.ValueOf(result)
	if value.Kind() == reflect.Slice && value.IsNil() {
		return []any{}
	}

	return result
}
