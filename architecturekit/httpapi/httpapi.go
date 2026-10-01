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
	"log/slog"
	"mime"
	"net/http"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/query"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// MaxRequestBody is the largest request body that is read. The body is read
// into memory in full before it is decoded, so it needs an upper bound.
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

	// ErrMalformed means the body could not be decoded, or not be turned into
	// a command.
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

	// logger is nil unless the API was created with WithLogger, in which case
	// the default logger of log/slog is used.
	logger *slog.Logger
}

// APIOption configures an API.
type APIOption func(*apiSettings)

type apiSettings struct {
	logger *slog.Logger
}

// WithLogger has everything that answers through the API log every failure it
// does not explain to the caller through the given logger, once, with the
// method and the route of the request: the routes the API wires up, and
// Respond, RespondResult and RespondResultAt in a handler of your own. Without
// it, they log through the default logger of log/slog.
//
// A nil logger is a programming error, so WithLogger panics.
func WithLogger(logger *slog.Logger) APIOption {
	if logger == nil {
		panic("architecturekit/httpapi: WithLogger needs a logger, not nil")
	}

	return func(settings *apiSettings) { settings.logger = logger }
}

// NewAPI creates an API that determines the user with userFrom. A request
// whose user cannot be determined is answered with 401 and never reaches a
// command or a query.
func NewAPI[TUser any](
	store *architecturekit.Store,
	userFrom func(*http.Request) (TUser, error),
	options ...APIOption,
) *API[TUser] {
	var settings apiSettings
	for _, option := range options {
		option(&settings)
	}

	return &API[TUser]{store: store, userFrom: userFrom, logger: settings.logger}
}

// logFailure logs a failure the caller is not told about, through the logger
// of the API, and names the request it happened on.
//
// Answering without an API is a programming error, so it panics, and does so
// on every answer rather than only on a failure, so that a test finds it.
func (api *API[TUser]) logFailure(r *http.Request) func(status int, err error) {
	if api == nil {
		panic("architecturekit/httpapi: answering needs the API, not nil")
	}

	return func(status int, err error) {
		logger := api.logger
		if logger == nil {
			logger = slog.Default()
		}

		logger.Error("httpapi: internal failure",
			"method", r.Method, "route", r.Pattern, "status", status, "error", err)
	}
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
func NewPublicAPI(store *architecturekit.Store, options ...APIOption) *API[NoUser] {
	return NewAPI(store, func(*http.Request) (NoUser, error) {
		return NoUser{}, nil
	}, options...)
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

	request, err := BodyOf[TRequest](r)
	if err != nil {
		return handled, err
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

// RouteOption configures a route that Route wires up.
type RouteOption[TCommand any] func(*routeSettings[TCommand])

type routeSettings[TCommand any] struct {
	fields func(Handled[TCommand]) any
}

// Adding has a route answer with further fields next to the revision, such as
// the ID of an aggregate the command created. The function receives what the
// command did, and returns a value that encodes to a JSON object, usually a
// struct with json tags. It is only called after the command has succeeded.
//
// The kit adds the revision itself, so the fields must not contain one. A
// value that holds a revision, or that does not encode to a JSON object, is a
// programming error: the route then answers 500, and logs why, although the
// events have been written.
//
// A nil function, or giving Adding twice, is a programming error, so it
// panics.
func Adding[TCommand any](fields func(Handled[TCommand]) any) RouteOption[TCommand] {
	if fields == nil {
		panic("architecturekit/httpapi: Adding needs a function, not nil")
	}

	return func(settings *routeSettings[TCommand]) {
		if settings.fields != nil {
			panic("architecturekit/httpapi: Adding is given twice")
		}

		settings.fields = fields
	}
}

// Route wires a request DTO to a decider, adds it to the mux and answers in
// the kit's default format, which is the revision the command wrote (see
// Respond), plus the fields of Adding, if given. Only TRequest has to be
// given: TUser comes from the API, TCommand and TState come from the decider.
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
	options ...RouteOption[TCommand],
) {
	var settings routeSettings[TCommand]
	for _, option := range options {
		option(&settings)
	}

	mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handled, err := Handle[TRequest](r, api, decider)

		var fields any
		if err == nil && settings.fields != nil {
			fields = settings.fields(handled)
		}

		respond(w, handled.Events, fields, err, api.logFailure(r))
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

// Respond writes the kit's default answer to a command. On success, that is
// the revision the command wrote, the ID of the last event, which a caller
// hands to a query to read its own writes (see QueryRevisioned):
//
//	{"revision": "42"}
//
// The revision is empty if the command wrote nothing. On failure, it is a
// message, which explains a failure the caller can fix, and only says
// "internal server error" otherwise, while the failure is logged through the
// logger of the API, with the route of the request (see WithLogger).
//
// Replace it with your own writer if you need a different shape; StatusFor
// stays usable either way.
func Respond[TUser any](
	w http.ResponseWriter,
	r *http.Request,
	api *API[TUser],
	written []eventsourcingdb.Event,
	err error,
) {
	respond(w, written, nil, err, api.logFailure(r))
}

// respond writes the answer to a command, with the given fields next to the
// revision, and logs an internal failure with logFailure.
func respond(
	w http.ResponseWriter,
	written []eventsourcingdb.Event,
	fields any,
	err error,
	logFailure func(status int, err error),
) {
	var body map[string]any
	if err == nil {
		body, err = answerOf(written, fields)
	}

	status := StatusFor(err)
	switch {
	case err == nil:
	case status < http.StatusInternalServerError:
		body = map[string]any{"message": err.Error()}
	default:
		// Internal failures are not explained to the caller, but logged.
		body = map[string]any{"message": "internal server error"}
		logFailure(status, err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// answerOf combines the revision with the fields of Adding. The fields are
// decoded with numbers kept as they are, so that a large integer does not lose
// digits on its way through a float.
func answerOf(written []eventsourcingdb.Event, fields any) (map[string]any, error) {
	answer := map[string]any{}

	if fields != nil {
		encoded, err := json.Marshal(fields)
		if err != nil {
			return nil, fmt.Errorf("httpapi: encoding the fields of the answer: %w", err)
		}

		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()

		if err := decoder.Decode(&answer); err != nil || answer == nil {
			return nil, fmt.Errorf("httpapi: the fields of the answer must encode to a JSON object, not %s", encoded)
		}
		if _, hasRevision := answer["revision"]; hasRevision {
			return nil, errors.New("httpapi: the fields of the answer must not contain a revision, which the kit adds itself")
		}
	}

	answer["revision"] = architecturekit.RevisionOf(written)

	return answer, nil
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

// BodyOf decodes the JSON body of a request by the rules that Route applies to
// a command, for a handler of your own or a query whose input does not fit
// into the query string. The Content-Type has to be application/json, or it is
// ErrUnsupportedMediaType. The body may hold at most MaxRequestBody bytes, or
// it is ErrTooLarge. JSON that does not fit TBody, including a field that TBody
// does not have, is ErrMalformed.
func BodyOf[TBody any](r *http.Request) (TBody, error) {
	var value TBody

	if err := requireJSON(r); err != nil {
		return value, err
	}

	body, err := readBody(r)
	if err != nil {
		return value, err
	}

	// Unknown fields are rejected rather than dropped, so that a misspelled
	// field cannot silently turn into a zero value.
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&value); err != nil {
		var zero TBody
		return zero, fmt.Errorf("%w: %v", ErrMalformed, err)
	}

	return value, nil
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
