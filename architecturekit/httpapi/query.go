package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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

// Query wires a query to the mux and answers in the kit's default format,
// which is the result itself.
func Query[TUser any, TQuery any, TResult any](
	api *API[TUser],
	mux *http.ServeMux,
	pattern string,
	toQuery ToQuery[TUser, TQuery],
	answer Answer[TQuery, TResult],
) {
	mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result, err := Ask(r, api, toQuery, answer)
		RespondResult(w, result, err)
	}))
}

// RespondResult writes a query result, or maps the error the way Respond does
// for commands.
func RespondResult[TResult any](w http.ResponseWriter, result TResult, err error) {
	w.Header().Set("Content-Type", "application/json")

	if err == nil {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(result)
		return
	}

	status := StatusFor(err)
	message := err.Error()
	if status >= http.StatusInternalServerError {
		// Internal failures are not explained to the caller.
		message = "internal server error"
	}

	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}
