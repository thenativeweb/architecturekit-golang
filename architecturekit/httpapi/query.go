package httpapi

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

// ErrNotFound means the query asked for something that does not exist.
var ErrNotFound = errors.New("not found")

// MethodQuery is QUERY, the method of RFC 10008 that a query is asked with
// (see Query). It is named like http.MethodGet, since net/http does not
// define it yet.
const MethodQuery = "QUERY"

// ToQuery turns a request, its body, and the user into a query. It is the
// read side's counterpart to ToCommand, and gets the same: the body comes
// decoded into TRequest, by the rules of BodyOf, while the request holds what
// the body does not, such as a value of the path, which r.PathValue returns,
// a header, or the context of the request.
//
// TRequest only describes the body, as for a command, so the query itself
// needs no json tags. For a query without input, it is NoBody.
//
// Its errors are treated as those of ToCommand: one that StatusFor maps to a
// status of its own keeps it, and so do one of the category
// architecturekit.ErrPermanent, one of architecturekit.ErrOutcomeUnknown, and
// one of architecturekit.ErrNotCaughtUp, while any other error comes back
// wrapped with ErrMalformed, and is answered with 400.
type ToQuery[TUser any, TRequest any, TQuery any] func(r *http.Request, request TRequest, user TUser) (TQuery, error)

// Answer answers a query. It sees neither the request nor HTTP, which is the
// whole point of splitting it from ToQuery.
type Answer[TQuery any, TResult any] func(ctx context.Context, query TQuery) (TResult, error)

// Ask determines the caller, decodes the body of the request into TRequest
// (see BodyOf), turns both into a query with toQuery, and answers it, without
// writing anything to the response. Use it to answer in a format of your own.
//
// It answers any method, such as GET for a download. A request with GET
// carries no body, so its request type is NoBody, which lets GET pass from
// any origin, since it must not change anything (see NoBody).
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
func Ask[TUser any, TRequest any, TQuery any, TResult any](
	r *http.Request,
	api *API[TUser],
	toQuery ToQuery[TUser, TRequest, TQuery],
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

	query, err := build(r, api, toQuery)
	if err != nil {
		return result, err
	}

	return answer(r.Context(), query)
}

// QueryOption configures a query that Query wires up.
type QueryOption func(*querySettings)

type querySettings struct {
	view     architecturekit.Revisioned
	wait     time.Duration
	isTagged bool
	varies   Volatile
}

// givenTogether is what Revisioned and Awaiting panic with, in either order,
// since both wait.
const givenTogether = "architecturekit/httpapi: Awaiting is given along with Revisioned, which waits as well"

// Revisioned has a query wait for the revision a caller asks for, for at most
// the given time (DefaultWait, unless there is a reason for another), answer
// 304 when nothing has changed, and tag the answer with the revision of the
// view it served.
//
// Whether nothing has changed, it tells from If-None-Match, which it reads the
// way HTTP has it: as a list of tags, or *, compared weakly, so that a tag
// that a proxy marked as weak while compressing the answer still matches. HTTP
// has 304 for GET and HEAD (RFC 9110, 13.1.2), and for QUERY, which it treats
// like GET (RFC 10008), also when the query asks with a body. It carries the
// tag and the revision, and no body.
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
// An answer for which none of that works, such as one that depends on the
// instant, or on another view, waits with Awaiting instead, which tags
// nothing.
//
// The query is built before anything waits or is answered, since building it
// determines the caller, decodes the body, and checks what they may ask:
// nobody can make the server wait, or learn that an answer is unchanged,
// without being allowed to ask. Once it has waited, it reads the revision of
// the view, and a view that fails to read it fails the query with its error,
// which is answered as every other error is (see StatusFor).
//
// An answer that carries a revision, a success or one that says that nothing
// has changed, says Cache-Control: private, no-cache: a cache asks again
// before it hands it out, and private keeps shared caches, such as proxies,
// from keeping it at all. Any other answer, such as a failure, or a success of
// a view that has seen nothing, says no-store, as every other answer of the
// kit does.
//
// A nil view, a negative wait, giving Revisioned twice, or along with
// Awaiting, is a programming error, so it panics. A nil pointer counts as a
// nil view, such as a view that was declared but never created.
func Revisioned(view architecturekit.Revisioned, wait time.Duration) QueryOption {
	if isNil(view) {
		panic("architecturekit/httpapi: Revisioned needs a view, not nil")
	}
	if wait < 0 {
		panic(fmt.Sprintf("architecturekit/httpapi: Revisioned needs a wait that is not negative, not %s", wait))
	}

	return func(settings *querySettings) {
		switch {
		case settings.view != nil && settings.isTagged:
			panic("architecturekit/httpapi: Revisioned is given twice")
		case settings.view != nil:
			panic(givenTogether)
		}

		settings.view = view
		settings.wait = wait
		settings.isTagged = true
	}
}

// Awaiting has a query wait for the revision a caller asks for, for at most
// the given time (DefaultWait, unless there is a reason for another), as
// Revisioned does, but tags nothing: the answer carries neither a revision
// nor an ETag, is never 304, and says Cache-Control: no-store, as every answer
// without a revision does.
//
// Use it for an answer that depends on more than the query and the view, such
// as on the instant, on the configuration, or on another view, for which a
// tag could tell a caller, wrongly, that nothing has changed (see Revisioned).
// The caller still reads its own writes.
//
// It waits as Revisioned does (see Await): a revision that is not one is
// answered with 400, and once the time has run out, the query is answered with
// what the view holds. The query is built before anything waits, so a body
// that does not fit is refused at once, and nobody can make the server wait
// without being allowed to ask.
//
// A nil view, a negative wait, giving Awaiting twice, or along with
// Revisioned, is a programming error, so it panics. A nil pointer counts as a
// nil view, such as a view that was declared but never created.
func Awaiting(view architecturekit.Revisioned, wait time.Duration) QueryOption {
	if isNil(view) {
		panic("architecturekit/httpapi: Awaiting needs a view, not nil")
	}
	if wait < 0 {
		panic(fmt.Sprintf("architecturekit/httpapi: Awaiting needs a wait that is not negative, not %s", wait))
	}

	return func(settings *querySettings) {
		switch {
		case settings.view != nil && !settings.isTagged:
			panic("architecturekit/httpapi: Awaiting is given twice")
		case settings.view != nil:
			panic(givenTogether)
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
// A nil function, giving Varying twice, or Varying without Revisioned, also
// with Awaiting, is a programming error, so it panics; the last one when Query
// wires the query.
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
// which is the result itself, encoded as RespondResult encodes it, with
// Cache-Control: no-store, so that no cache keeps it. With Revisioned, it
// reads its own writes and answers 304 when nothing has changed; with Varying
// in addition, its tag changes with what the answer takes from elsewhere.
// With Awaiting, it reads its own writes, but tags nothing.
//
// A query is asked with QUERY, the method that HTTP defines in RFC 10008.
// Like GET, it is safe, so it changes nothing, but like POST, it carries a
// body. So the pattern names it, as in QUERY /api/books. A pattern without a
// method, which accepts every method, or with another method than QUERY, is
// a programming error, so Query panics, as Route does for a method that must
// not change anything. A handler of your own that asks with Ask may use any
// method, such as GET for a download.
//
// The body holds the input of the query, which is decoded into TRequest as
// for a command, after the caller is determined, and before toQuery gets it
// (see BodyOf). So a body that does not claim to be JSON, that is too large,
// or that is not valid JSON or does not fit TRequest is answered with 415,
// 413, or 400, as for a command. A query without input has the request type
// NoBody, which accepts a body that is empty, or {}, and refuses a request
// that a browser sends from another origin (see NoBody).
//
// A panic while it handles a request is answered with 500, like any other
// internal failure, and logged with its value and its stack, as with Route.
// So is a result that can not be encoded, such as one that holds NaN, which
// JSON has no number for.
//
// A nil API, toQuery, answer, or option is a programming error, so Query
// panics, as Route does, rather than failing every request, with 500, or for a
// nil API, with no answer at all.
func Query[TUser any, TRequest any, TQuery any, TResult any](
	api *API[TUser],
	mux *http.ServeMux,
	pattern string,
	toQuery ToQuery[TUser, TRequest, TQuery],
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

	switch method := methodOf(pattern); method {
	case MethodQuery:
	case "":
		panic(fmt.Sprintf("architecturekit/httpapi: Query needs a pattern that names the method QUERY, not %q, "+
			"which accepts every method", pattern))
	default:
		panic(fmt.Sprintf("architecturekit/httpapi: Query needs a pattern whose method is QUERY, not %q, "+
			"since a query is asked with QUERY rather than %s", pattern, method))
	}

	var settings querySettings
	applyOptions("Query", &settings, options)

	if settings.varies != nil && !settings.isTagged {
		panic("architecturekit/httpapi: Varying needs Revisioned, since only a revisioned query has a tag")
	}

	if settings.view == nil {
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
// query that found no item, a write whose outcome is unknown, a view that did
// not catch up with a write, and 500 and above, while the error is logged through the logger of the API, with the
// route of the request (see WithLogger), and the error itself otherwise. A
// result that can not be encoded, such as one that holds NaN, is answered with
// 500 as well, and so is a panic while it is encoded, such as one in a
// MarshalJSON function, which is logged with its value and its stack, as a
// route logs it. Like Respond, it says Cache-Control: no-store, so that no
// cache keeps the answer.
//
// The result is encoded as encoding/json encodes it, except that a nil slice
// is [] and a nil map is {}, at every depth, rather than null, so that a
// caller gets a list or an object, whether it holds anything or not. A nil
// pointer is still null, and a nil slice of bytes is "", since a slice of
// bytes is a string in base64.
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
// MarshalJSON function, which is recovered here (see encodeResult), so that a
// handler of your own answers it as well, rather than net/http closing the
// connection.
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

	var body []byte
	if err == nil {
		body, err = encodeResult(result)
	}

	if err == nil {
		writeRevision(w, revision, tag)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}

	status := StatusFor(err)
	message := explain(status, err)

	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}

// encodeResult encodes a result as an answer holds it (see answerJSON), with a
// newline at the end, as what an Encoder of encoding/json writes has.
//
// A result that can not be encoded is an error, which is wrapped with %v
// rather than %w, since it is a mistake in the code, which has to be answered
// with 500, whatever category the error of a MarshalJSON function has. A
// panic while it is encoded, such as one in a MarshalJSON function, is an
// error as well, which carries its value and its stack (see recoverInto), as
// a panic in a route does.
func encodeResult[TResult any](result TResult) (body []byte, err error) {
	defer recoverInto(&err)

	encoded, err := jsonv2.Marshal(result, answerJSON)
	if err != nil {
		return nil, fmt.Errorf("httpapi: encoding the result: %v", err)
	}

	return append(encoded, '\n'), nil
}

// answerJSON are the rules that results and the fields of Adding are encoded
// by: those of encoding/json, except that a nil slice is written as [], and a
// nil map as {}, at every depth, rather than as null, so that a caller gets a
// list or an object, whether it holds anything or not, such as for the nil
// slice that query.Collect returns when there are no items.
//
// A nil pointer and a nil interface are still null, and so is a nil
// json.RawMessage, which holds JSON text. A nil slice of bytes is "", since a
// slice of bytes is written as a string in base64.
var answerJSON = jsonv2.JoinOptions(
	json.DefaultOptionsV1(),
	jsonv2.FormatNilSliceAsNull(false),
	jsonv2.FormatNilMapAsNull(false),
)
