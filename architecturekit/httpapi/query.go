package httpapi

import (
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
var ErrNotFound = errors.New("httpapi: not found")

// ToQuery turns request data and the user into a query. It is the read
// side's counterpart to ToCommand, and it is a function rather than a method,
// because a query reads from the URL instead of from a body.
type ToQuery[TUser any, TQuery any] func(r *http.Request, user TUser) (TQuery, error)

// Answer answers a query. It sees neither the request nor HTTP, which is the
// whole point of splitting it from ToQuery.
type Answer[TQuery any, TResult any] func(ctx context.Context, query TQuery) (TResult, error)

// Ask determines the caller, builds the query and answers it, without writing
// anything to the response. Use it to answer in a format of your own.
func Ask[TUser any, TQuery any, TResult any](
	r *http.Request,
	api *API[TUser],
	toQuery ToQuery[TUser, TQuery],
	answer Answer[TQuery, TResult],
) (TResult, error) {
	var empty TResult

	user, err := UserOf(r, api)
	if err != nil {
		return empty, err
	}

	query, err := toQuery(r, user)
	if err != nil {
		return empty, categorise(err)
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
// 304 when nothing has changed, and tag the answer with the revision of the
// view it served.
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
//     that nothing has changed when it has.
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
// A nil view, a negative wait, or giving Revisioned twice, is a programming
// error, so it panics.
func Revisioned(view architecturekit.Revisioned, wait time.Duration) QueryOption {
	if view == nil {
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
// which is the result itself. With Revisioned, it reads its own writes and
// answers 304 when nothing has changed; with Varying in addition, its tag
// changes with what the answer takes from elsewhere.
func Query[TUser any, TQuery any, TResult any](
	api *API[TUser],
	mux *http.ServeMux,
	pattern string,
	toQuery ToQuery[TUser, TQuery],
	answer Answer[TQuery, TResult],
	options ...QueryOption,
) {
	var settings querySettings
	for _, option := range options {
		option(&settings)
	}

	if settings.view == nil {
		if settings.varies != nil {
			panic("architecturekit/httpapi: Varying needs Revisioned, since only a revisioned query has a tag")
		}

		mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			result, err := Ask(r, api, toQuery, answer)
			respondResult(w, result, err, api.logFailure(r))
		}))

		return
	}

	mux.Handle(pattern, answerRevisioned(api, toQuery, answer, settings))
}

// RespondResult writes a query result, or maps the error the way Respond does
// for commands, logging an internal failure through the logger of the API,
// with the route of the request (see WithLogger).
func RespondResult[TUser any, TResult any](
	w http.ResponseWriter,
	r *http.Request,
	api *API[TUser],
	result TResult,
	err error,
) {
	respondResult(w, result, err, api.logFailure(r))
}

// respondResult writes a query result, and logs an internal failure with
// logFailure.
func respondResult[TResult any](
	w http.ResponseWriter,
	result TResult,
	err error,
	logFailure func(status int, err error),
) {
	w.Header().Set("Content-Type", "application/json")

	if err == nil {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(listOf(result))
		return
	}

	status := StatusFor(err)
	message := err.Error()
	if status >= http.StatusInternalServerError {
		// Internal failures are not explained to the caller, but logged.
		message = "internal server error"
		logFailure(status, err)
	}

	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}

// listOf turns a nil slice into an empty one, so that a query that finds
// nothing answers with [] rather than null. slices.Collect, which the kit
// suggests for turning items into a slice, returns nil when there are none.
func listOf(result any) any {
	value := reflect.ValueOf(result)
	if value.Kind() == reflect.Slice && value.IsNil() {
		return []any{}
	}

	return result
}
