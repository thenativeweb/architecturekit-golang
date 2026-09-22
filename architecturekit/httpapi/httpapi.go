// Package httpapi exposes commands over HTTP. It is optional: the kit's core
// knows nothing about transports, and everything here can be replaced by a
// handler of your own.
package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/query"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// MaxRequestBody is the largest request body that is read. It matches the
// limit of the EventSourcingDB itself.
const MaxRequestBody = 1 << 20

var (
	// ErrUnauthorized means the caller could not be determined.
	ErrUnauthorized = errors.New("httpapi: unauthorized")

	// ErrUnsupportedMediaType means the request did not claim to be JSON.
	ErrUnsupportedMediaType = errors.New("httpapi: unsupported media type")

	// ErrTooLarge means the request body exceeded MaxRequestBody.
	ErrTooLarge = errors.New("httpapi: request body too large")

	// ErrForbidden means the user is known but not allowed to do this.
	ErrForbidden = errors.New("httpapi: forbidden")

	// ErrMalformed means the body could not be turned into a command.
	ErrMalformed = errors.New("httpapi: malformed request")
)

// ToCommand is implemented by the request DTO. It is the single place where
// transport data and the user turn into a command, which keeps the command
// itself free of JSON tags and of HTTP.
type ToCommand[TUser any, TCommand any] interface {
	ToCommand(user TUser) (TCommand, error)
}

// API bundles what all routes share.
type API[TUser any] struct {
	store    *architecturekit.Store
	userFrom func(*http.Request) (TUser, error)
}

// NewAPI creates an API that determines the user with userFrom. A request
// whose user cannot be determined is answered with 401 and never reaches a
// command or a query.
func NewAPI[TUser any](
	store *architecturekit.Store,
	userFrom func(*http.Request) (TUser, error),
) *API[TUser] {
	return &API[TUser]{store: store, userFrom: userFrom}
}

// UserOf determines who is asking, the same way Handle and Ask do.
//
// Use it when you write a handler of your own -- a stream, a download, a page
// -- so that it treats callers exactly like the routes the kit wires up. An
// unknown caller comes back as ErrUnauthorized, which StatusFor maps to 401.
func UserOf[TUser any](r *http.Request, api *API[TUser]) (TUser, error) {
	user, err := api.userFrom(r)
	if err != nil {
		var none TUser
		return none, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}

	return user, nil
}

// NoUser is the user type of an application that has no users, which is not
// the same as one whose users are unknown.
type NoUser struct{}

// NewPublicAPI creates an API for an application without authentication.
// Every request is served, and commands and queries receive NoUser.
func NewPublicAPI(store *architecturekit.Store) *API[NoUser] {
	return NewAPI(store, func(*http.Request) (NoUser, error) {
		return NoUser{}, nil
	})
}

// Handled is what a command did. It carries the command itself, so that a
// handler can answer with something it generated on the way, such as an
// aggregate ID it made up before executing.
type Handled[TCommand any] struct {
	Command TCommand
	Events  []eventsourcingdb.Event
}

// Handle turns a request into a command and executes it, without writing
// anything to the response. Use it to answer in a format of your own.
func Handle[
	TRequest ToCommand[TUser, TCommand],
	TUser any,
	TCommand architecturekit.Command,
	TState any,
](
	r *http.Request,
	api *API[TUser],
	decider architecturekit.Decider[TCommand, TState],
) (Handled[TCommand], error) {
	var handled Handled[TCommand]

	user, err := UserOf(r, api)
	if err != nil {
		return handled, err
	}

	if err := requireJSON(r); err != nil {
		return handled, err
	}

	body, err := readBody(r)
	if err != nil {
		return handled, err
	}

	// Unknown fields are rejected rather than dropped, so that a misspelled
	// field cannot silently turn into a zero value.
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()

	var request TRequest
	if err := decoder.Decode(&request); err != nil {
		return handled, fmt.Errorf("%w: %v", ErrMalformed, err)
	}

	cmd, err := request.ToCommand(user)
	if err != nil {
		return handled, categorise(err)
	}

	// The command is handed back even when executing it fails, because a
	// handler may want to name what it tried to do.
	handled.Command = cmd
	handled.Events, err = architecturekit.Execute(r.Context(), api.store, decider, cmd)

	return handled, err
}

// Route wires a request DTO to a decider, adds it to the mux and answers in
// the kit's default format. Only TRequest has to be given: TUser comes
// from the API, TCommand and TState come from the decider.
func Route[
	TRequest ToCommand[TUser, TCommand],
	TUser any,
	TCommand architecturekit.Command,
	TState any,
](
	api *API[TUser],
	mux *http.ServeMux,
	pattern string,
	decider architecturekit.Decider[TCommand, TState],
) {
	mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handled, err := Handle[TRequest](r, api, decider)
		Respond(w, handled.Events, err)
	}))
}

// StatusFor maps an error to an HTTP status. It asks for error categories
// rather than concrete errors, so new failures do not need a new case here.
func StatusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrUnsupportedMediaType):
		return http.StatusUnsupportedMediaType
	case errors.Is(err, ErrMalformed):
		return http.StatusBadRequest
	// A query that expected one item and found none is a plain 404, so that a
	// single-item lookup does not have to translate it by hand.
	case errors.Is(err, ErrNotFound), errors.Is(err, query.ErrNoItems):
		return http.StatusNotFound
	case errors.Is(err, architecturekit.ErrDomain):
		return http.StatusUnprocessableEntity
	// A conflict is transient, but 409 says more than 503, so it comes first.
	case errors.Is(err, architecturekit.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, architecturekit.ErrTransient):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// Respond writes the kit's default answer. Replace it with your own writer if
// you need a different shape; StatusFor stays usable either way.
func Respond(w http.ResponseWriter, written []eventsourcingdb.Event, err error) {
	status := StatusFor(err)

	body := map[string]any{"message": "ok"}
	switch {
	case err == nil:
		body["eventIds"] = idsOf(written)
	case status < http.StatusInternalServerError:
		body["message"] = err.Error()
	default:
		// Internal failures are not explained to the caller.
		body["message"] = "internal server error"
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func idsOf(events []eventsourcingdb.Event) []string {
	ids := make([]string, len(events))
	for i, event := range events {
		ids[i] = event.ID
	}
	return ids
}

// categorise leaves an error that already says what kind it is alone, and
// marks everything else as a malformed request.
//
// Without this, an application that returns ErrForbidden from ToCommand would
// see its 403 turned into a 400.
func categorise(err error) error {
	for _, known := range []error{
		ErrUnauthorized,
		ErrForbidden,
		ErrNotFound,
		ErrTooLarge,
		ErrUnsupportedMediaType,
		ErrMalformed,
		architecturekit.ErrDomain,
	} {
		if errors.Is(err, known) {
			return err
		}
	}

	return fmt.Errorf("%w: %v", ErrMalformed, err)
}

// requireJSON insists on application/json. That is not pedantry: a browser
// form can only send urlencoded, multipart or text/plain, and only those count
// as simple requests without a CORS preflight. Demanding JSON therefore shuts
// out cross-origin writes, whatever the application uses to authenticate.
//
// The media type is parsed rather than matched as a substring, because
// "text/plain; application/json" would pass a substring test.
func requireJSON(r *http.Request) error {
	value := r.Header.Get("Content-Type")
	if value == "" {
		return fmt.Errorf("%w: Content-Type is required", ErrUnsupportedMediaType)
	}

	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return fmt.Errorf("%w: %q could not be parsed", ErrUnsupportedMediaType, value)
	}
	if mediaType != "application/json" {
		return fmt.Errorf("%w: %q is not application/json", ErrUnsupportedMediaType, mediaType)
	}

	return nil
}

// readBody reads at most MaxRequestBody bytes and tells a body that is too
// large apart from one that could not be read.
func readBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBody+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading the body: %v", ErrMalformed, err)
	}

	if len(body) > MaxRequestBody {
		return nil, fmt.Errorf("%w: at most %d bytes are read", ErrTooLarge, MaxRequestBody)
	}

	return body, nil
}
