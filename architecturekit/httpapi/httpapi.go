// Package httpapi exposes commands and queries over HTTP, lets a caller read
// its own writes (see Revisioned, Awaiting and Await), and answers the health
// checks of an orchestrator (see Readiness and Liveness). It is optional: the
// kit's core knows nothing about transports, and everything here can be
// replaced by a handler of your own.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"reflect"
	"runtime/debug"
	"strings"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/query"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// MaxRequestBody is the largest request body that is read. The body is read
// into memory in full before it is decoded, so it needs an upper bound.
const MaxRequestBody = 1 << 20

// These errors sort the failures of a request. An error that is written for
// the caller is answered with its text (see Respond), so their texts name what
// is wrong, without the name of the package.
var (
	// ErrUnauthorized means the caller could not be determined.
	ErrUnauthorized = errors.New("unauthorized")

	// ErrUnsupportedMediaType means the request did not claim to be JSON.
	ErrUnsupportedMediaType = errors.New("unsupported media type")

	// ErrTooLarge means the request body exceeded MaxRequestBody, or a lower
	// limit that a middleware set with http.MaxBytesReader.
	ErrTooLarge = errors.New("request body too large")

	// ErrForbidden means the user is known but not allowed to do this, or that
	// a request without a body comes from another origin (see NoBody).
	ErrForbidden = errors.New("forbidden")

	// ErrMalformed means the body could not be read or decoded, or is not
	// empty for a route that takes none, that the request could not be turned
	// into a command by ToCommand or into a query by ToQuery, or that the
	// revision it waits for, in the Wait-For-Revision header, is not one.
	ErrMalformed = errors.New("malformed request")
)

// errNoStore means a command reached an API that was created without a store.
// That is a mistake in the wiring, which the caller is not told.
var errNoStore = errors.New("httpapi: the API has no store, so it can not execute commands")

// ToCommand turns a request, its body, and the user into a command. The body
// comes decoded into TRequest, by the rules of BodyOf, while the request holds
// what the body does not, such as a value of the path, which r.PathValue
// returns, a header, or the context of the request.
//
// It is the single place where transport data and the user turn into a
// command, which keeps the command itself free of JSON tags and of HTTP.
// TRequest only describes the body, so it may come from another package than
// the function, such as one that is shared with a client. For a command that
// takes no body, it is NoBody.
//
// An error that StatusFor maps to a status of its own keeps it, such as
// ErrForbidden, an error of the category architecturekit.ErrDomain, or one of
// architecturekit.ErrTransient, if a service that ToCommand asks is down. So
// do an error of the category architecturekit.ErrPermanent and one of a write
// whose outcome is unknown, architecturekit.ErrOutcomeUnknown, which are
// answered with 500. Any other error means that the request can not be turned
// into a command, and comes back wrapped with ErrMalformed, which is answered
// with 400 and the error as the message.
type ToCommand[TUser any, TRequest any, TCommand any] func(r *http.Request, request TRequest, user TUser) (TCommand, error)

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

// WithLogger has everything that answers through the API log every error it
// does not explain to the caller in full through the given logger, once, with
// the method and the route of the request: the routes the API wires up, and
// Respond, RespondResult, and RespondError in a handler of your own. A failure
// of the server is logged as an error, which for a panic includes its value
// and its stack, and a refusal whose details the caller is not told, such as
// one with 401 or 409 (see Respond), as information. The same goes for an
// answer that Adding could not complete, which is logged as an error. Only a
// request that was canceled is not logged, since nothing failed. Without it,
// they log through the default logger of log/slog.
//
// A nil logger is a programming error, so WithLogger panics.
func WithLogger(logger *slog.Logger) APIOption {
	if logger == nil {
		panic("architecturekit/httpapi: WithLogger needs a logger, not nil")
	}

	return func(settings *apiSettings) { settings.logger = logger }
}

// NewAPI creates an API that determines the user with userFrom. A request
// for which userFrom fails never reaches a command or a query. It is answered
// with 401, unless the error has a status of its own, which it keeps (see
// UserOf).
//
// Queries do not need the store, so an API without one, with nil, answers
// them, for example in a test of the queries alone. A command on such an API
// is answered with 500, and the failure says that there is no store.
//
// A nil userFrom, on the other hand, is a programming error, so NewAPI panics,
// rather than failing every request with 500, and so does a nil option. For an
// application without authentication, use NewPublicAPI.
func NewAPI[TUser any](
	store *architecturekit.Store,
	userFrom func(*http.Request) (TUser, error),
	options ...APIOption,
) *API[TUser] {
	if userFrom == nil {
		panic("architecturekit/httpapi: NewAPI needs a function that determines the user, not nil")
	}

	return newAPI("NewAPI", store, userFrom, options)
}

// newAPI creates the API of NewAPI and NewPublicAPI, which it names if one of
// the options is nil.
func newAPI[TUser any](
	function string,
	store *architecturekit.Store,
	userFrom func(*http.Request) (TUser, error),
	options []APIOption,
) *API[TUser] {
	var settings apiSettings
	applyOptions(function, &settings, options)

	return &API[TUser]{store: store, userFrom: userFrom, logger: settings.logger}
}

// applyOptions applies the options to the settings of the function of httpapi
// with the given name, in their order. A nil option, such as one that was
// declared but never set, is a programming error, so applyOptions panics,
// naming the function, rather than with a nil dereference.
func applyOptions[TSettings any, TOption ~func(*TSettings)](function string, settings *TSettings, options []TOption) {
	for _, option := range options {
		if option == nil {
			panic(fmt.Sprintf("architecturekit/httpapi: %s got a nil option", function))
		}

		option(settings)
	}
}

// explain returns the function that turns an error into the message the
// caller is told, and logs what the message leaves out through the logger of
// the API, naming the request it happened on.
//
// Answering without an API is a programming error, so it panics, and does so
// on every answer rather than only on a failure, so that a test finds it.
func (api *API[TUser]) explain(r *http.Request) func(status int, err error) string {
	if api == nil {
		panic("architecturekit/httpapi: answering needs the API, not nil")
	}

	return func(status int, err error) string {
		switch {
		// A write whose outcome is unknown is an internal failure, and logged as
		// one, but the caller has to learn that the events may have been
		// written, so that it does not simply try again. Its error may name
		// internals, such as the subject, so it gets a fixed text.
		case errors.Is(err, architecturekit.ErrOutcomeUnknown):
			api.logFailure(r, status, err)

			return "outcome unknown: the request may have succeeded"

		// Internal failures are not explained to the caller, but logged.
		case status >= http.StatusInternalServerError:
			api.logFailure(r, status, err)

			return "internal server error"

		// A refusal with 401 or 409 tells the caller what to do, sign in or read
		// again, but its error may name internals, such as the key a token failed
		// to verify with, or the subject a precondition guarded. So it gets a
		// fixed text, and since the server did not fail, the error is logged as
		// information.
		case status == http.StatusUnauthorized:
			api.logRefusal(r, status, err)

			return "unauthorized"

		case status == http.StatusConflict:
			api.logRefusal(r, status, err)

			return "conflict: the data has changed since it was read"

		// A request that was canceled, usually because the caller went away,
		// failed for no fault of the server. Its error may name internals, such
		// as the subject that was read, and the caller may still be there, if
		// the application canceled the request itself. So it gets a fixed text,
		// and since nothing failed, it is not logged.
		case status == statusClientClosedRequest:
			return "request canceled"

		// A query that expects one item and finds none fails with
		// query.ErrNoItems, whose text does not say what was not found, in the
		// words of a package that knows nothing about HTTP. So unless the
		// application says itself what was not found, with ErrNotFound, the
		// caller is told what the status says, and the error is logged as
		// information, as above.
		case status == http.StatusNotFound && !errors.Is(err, ErrNotFound):
			api.logRefusal(r, status, err)

			return "not found"

		// Every other error is written for the caller, such as the business rule
		// a command broke, or what is wrong with a request.
		default:
			return err.Error()
		}
	}
}

// logFailure logs an internal failure. For a panic, that includes its value
// and the stack it happened on, which the error alone does not show.
func (api *API[TUser]) logFailure(r *http.Request, status int, err error) {
	attributes := []any{"method", r.Method, "route", r.Pattern, "status", status, "error", err}

	if failure, isPanic := errors.AsType[*panicError](err); isPanic {
		attributes = append(attributes, "panic", failure.value, "stack", string(failure.stack))
	}

	api.loggerOrDefault().Error("httpapi: internal failure", attributes...)
}

// logRefusal logs the error of a refusal whose details the caller is not told.
func (api *API[TUser]) logRefusal(r *http.Request, status int, err error) {
	api.loggerOrDefault().Info("httpapi: request refused",
		"method", r.Method, "route", r.Pattern, "status", status, "error", err)
}

// logIncomplete logs that a command has succeeded, but that the fields its
// answer should hold could not be completed (see Adding), and why the fields
// that came along with the error were dropped, if they were. The caller gets
// the revision all the same, so it is no internal failure, but it is worth
// knowing.
func (api *API[TUser]) logIncomplete(r *http.Request, err, dropped error) {
	attributes := []any{"method", r.Method, "route", r.Pattern, "error", err}
	if dropped != nil {
		attributes = append(attributes, "dropped", dropped)
	}

	api.loggerOrDefault().Error("httpapi: incomplete answer", attributes...)
}

// loggerOrDefault is the logger of the API, or the default one of slog if it
// has none (see WithLogger).
func (api *API[TUser]) loggerOrDefault() *slog.Logger {
	if api.logger == nil {
		return slog.Default()
	}

	return api.logger
}

// UserOf determines who is asking, the same way Handle and Ask do.
//
// Use it when you write a handler of your own, such as a stream, a download,
// or a page, so that it treats callers exactly like the routes the kit wires
// up. An unknown caller comes back as ErrUnauthorized, which StatusFor maps to
// 401.
//
// An error of userFrom that StatusFor maps to a status of its own comes back
// as it is, and so do one of the category architecturekit.ErrPermanent and
// one of architecturekit.ErrOutcomeUnknown, so that they keep their status.
// If the session store is down, for example, userFrom says so with
// architecturekit.ErrTransient, which is answered with 503 and logged, rather
// than sending the caller off to sign in again. Only an error without such a
// status comes back as ErrUnauthorized, which wraps it, so that errors.Is and
// errors.As still find it. To have an error with a status of its own answered
// with 401 all the same, userFrom wraps it with ErrUnauthorized itself.
func UserOf[TUser any](r *http.Request, api *API[TUser]) (TUser, error) {
	user, err := api.userFrom(r)
	if err != nil {
		var none TUser

		if hasCategory(err) {
			return none, err
		}

		return none, fmt.Errorf("%w: %w", ErrUnauthorized, err)
	}

	return user, nil
}

// NoUser is the user type of an application that has no users, which is not
// the same as one whose users are unknown.
type NoUser struct{}

// NoBody is the request type of a command or a query that takes no body,
// since everything it needs comes from the path, a header, or the user, such
// as POST /api/books/{id}/return, or QUERY /api/books/{id}. Its route requires
// no Content-Type, and accepts a body that is empty, or {}, which a caller may
// send out of habit. Any other body is ErrMalformed (see BodyOf). Unlike
// http.NoBody, which is an empty body to send, it is a type to hand to Route,
// Handle, Query, Ask, and BodyOf.
//
// Requiring JSON is what keeps a browser from sending a command from another
// site without asking the server first, since a form can not send JSON. A
// route that takes no body can not rely on that, so it checks first where the
// request comes from, with http.CrossOriginProtection. A request that a
// browser sends from another origin, which Sec-Fetch-Site says, or without
// it, an Origin whose host differs from Host, is ErrForbidden. A request from
// the same origin passes, and so does one without these headers, such as one
// of curl or of another server. So a browser frontend on another origin than
// the API can not call such a route for now.
//
// The check lets a request with GET, HEAD, or OPTIONS pass from any origin,
// since these methods must not change anything. That is why Route refuses a
// pattern with one of them, or without a method, which accepts every method
// (see Route). Wire a handler of your own that executes a command to a method
// that may change something as well, such as POST. A handler of your own that
// answers a query with GET, such as a download, passes from any origin, which
// is fine, since it changes nothing.
type NoBody struct{}

// NewPublicAPI creates an API for an application without authentication.
// Every request is served, and commands and queries receive NoUser.
//
// A nil option is a programming error, so NewPublicAPI panics, as NewAPI does.
func NewPublicAPI(store *architecturekit.Store, options ...APIOption) *API[NoUser] {
	return newAPI("NewPublicAPI", store, func(*http.Request) (NoUser, error) {
		return NoUser{}, nil
	}, options)
}

// Handled is what a command did. It carries the command itself, so that a
// handler can answer with something that ToCommand generated on the way, such
// as an aggregate ID it made up before executing.
type Handled[TCommand any] struct {
	Command TCommand
	Events  []eventsourcingdb.Event
}

// Handle determines the caller, decodes the body of the request into TRequest
// (see BodyOf), turns both into a command with toCommand, and executes it,
// without writing anything to the response. Use it to answer in a format of
// your own.
//
// A panic on the way, such as one of SubjectScheme.Build in the Subject
// function of the command, comes back as an error that StatusFor maps to 500,
// and that Respond logs with the value and the stack of the panic. Only
// http.ErrAbortHandler panics on, since net/http expects it to abort the
// response.
//
// A nil API, a nil toCommand, or the zero Decider, one that was not made with
// NewDecider, is a programming error, so Handle panics, and does so first, on
// every request, also one whose caller is unknown. Like any other panic, that
// comes back as an error, which names the mistake rather than a nil pointer.
func Handle[
	TUser any,
	TRequest any,
	TCommand architecturekit.Command,
	TState any,
](
	r *http.Request,
	api *API[TUser],
	toCommand ToCommand[TUser, TRequest, TCommand],
	decider architecturekit.Decider[TCommand, TState],
) (handled Handled[TCommand], err error) {
	defer recoverInto(&err)

	if api == nil {
		panic("architecturekit/httpapi: Handle needs the API, not nil")
	}
	if toCommand == nil {
		panic("architecturekit/httpapi: Handle needs a function that turns the request into a command, not nil")
	}
	if decider.State() == nil {
		panic("architecturekit/httpapi: Handle needs a decider made with NewDecider, not the zero Decider")
	}

	cmd, err := build(r, api, toCommand)
	if err != nil {
		return handled, err
	}

	// The command is handed back even when executing it fails, because a
	// handler may want to name what it tried to do.
	handled.Command = cmd

	if api.store == nil {
		return handled, errNoStore
	}

	handled.Events, err = architecturekit.Execute(r.Context(), api.store, decider, cmd)

	return handled, err
}

// build turns a request into a command or a query, the same way for Handle,
// Ask, and Query: it determines the caller (see UserOf), decodes the body into
// TRequest (see BodyOf), and hands both to to. So an unknown caller is refused
// before the body is read, and to only gets a body that fits. An error of to
// is categorised (see categorise).
func build[TUser any, TRequest any, TBuilt any](
	r *http.Request,
	api *API[TUser],
	to func(r *http.Request, request TRequest, user TUser) (TBuilt, error),
) (TBuilt, error) {
	var none TBuilt

	user, err := UserOf(r, api)
	if err != nil {
		return none, err
	}

	request, err := BodyOf[TRequest](r)
	if err != nil {
		return none, err
	}

	built, err := to(r, request, user)
	if err != nil {
		return none, categorise(err)
	}

	return built, nil
}

// RouteOption configures a route that Route wires up.
type RouteOption[TCommand any] func(*routeSettings[TCommand])

type routeSettings[TCommand any] struct {
	fields func(Handled[TCommand]) (any, error)
}

// Adding has a route answer with further fields next to the revision, such as
// the ID of an aggregate the command created. The function receives what the
// command did, and returns a value that encodes to a JSON object, usually a
// struct with json tags. It is only called after the command has succeeded.
//
// A value that encodes to null adds no fields. That is nil, and also a nil
// pointer of a concrete type, which an interface does not count as nil, such
// as the one that return lookup(handled) hands back if lookup returns a *T and
// an error, and fails. The value is encoded as a result is (see
// RespondResult), so a nil slice or map in it is [] or {}.
//
// If the function fails, the command has succeeded all the same, and its
// events are written. So the answer stays a success, with the revision, which
// the caller needs to read its own writes, and must not take for a reason to
// send the command again, whatever is wrong with the fields the function
// returned along with its error. The answer holds these fields if they can be
// used, and none otherwise. The error is logged through the logger of the
// API, with the route of the request (see WithLogger), and so is why the
// fields were dropped, if they were.
//
// The kit adds the revision itself, so the fields must not contain one. If
// the function did not fail, a value that holds a revision, that does not
// encode to a JSON object, or that can not be encoded at all, such as one
// that holds NaN, is a programming error: the route then answers 500, and
// logs why, although the events have been written.
//
// A nil function, or giving Adding twice, is a programming error, so it
// panics.
func Adding[TCommand any](fields func(Handled[TCommand]) (any, error)) RouteOption[TCommand] {
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

// Route wires a command to the mux. It handles a request as Handle does,
// turning it into the command with toCommand and executing that with the
// decider, and answers in the kit's default format, which is the revision the
// command wrote (see Respond), plus the fields of Adding, if given. No type
// has to be given: TUser comes from the API, TRequest and TCommand come from
// toCommand, and TState comes from the decider.
//
// A command that takes no body, such as one that needs nothing but a value of
// the path, has the request type NoBody. Its route then requires neither a
// Content-Type nor a body, but refuses a request that a browser sends from
// another origin (see NoBody).
//
// A panic while it handles a request is answered with 500, like any other
// internal failure, and logged with its value and its stack. Left to
// net/http, it would close the connection, and the caller would get no answer
// at all.
//
// A command changes something, so the pattern names a method that may do so,
// usually POST, as in POST /api/books/{id}/return. A pattern without a method
// accepts every method, GET included, and GET, HEAD, OPTIONS, and QUERY must
// not change anything. A browser sends GET and HEAD from another site without
// asking, such as for a link that the user follows, with the cookies of the
// user, and a route that takes no body lets GET, HEAD, and OPTIONS pass from
// another origin (see NoBody). QUERY may change nothing either, since HTTP
// defines it as safe and repeatable, so a client or a cache may send it again.
// So a pattern without a method, or with one of these, is a programming error,
// and Route panics, rather than executing the command for any site that links
// to it, or whenever a query is sent again.
//
// A nil API, a nil toCommand, the zero Decider, one that was not made with
// NewDecider, or a nil option, is a programming error, so Route panics, rather
// than failing every request, with 500, or for a nil API, with no answer at
// all.
func Route[
	TUser any,
	TRequest any,
	TCommand architecturekit.Command,
	TState any,
](
	api *API[TUser],
	mux *http.ServeMux,
	pattern string,
	toCommand ToCommand[TUser, TRequest, TCommand],
	decider architecturekit.Decider[TCommand, TState],
	options ...RouteOption[TCommand],
) {
	if api == nil {
		panic("architecturekit/httpapi: Route needs the API, not nil")
	}
	if toCommand == nil {
		panic("architecturekit/httpapi: Route needs a function that turns the request into a command, not nil")
	}
	if decider.State() == nil {
		panic("architecturekit/httpapi: Route needs a decider made with NewDecider, not the zero Decider")
	}

	switch method := methodOf(pattern); method {
	case "":
		panic(fmt.Sprintf("architecturekit/httpapi: Route needs a pattern that names a method, such as POST, not %q, "+
			"which accepts every method, GET included", pattern))
	case http.MethodGet, http.MethodHead, http.MethodOptions, MethodQuery:
		panic(fmt.Sprintf("architecturekit/httpapi: Route needs a pattern whose method may change something, such as POST, not %q, "+
			"since %s must not change anything", pattern, method))
	}

	var settings routeSettings[TCommand]
	applyOptions("Route", &settings, options)

	mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer api.answerPanic(w, r)

		handled, err := Handle(r, api, toCommand, decider)

		var fields map[string]any
		if err == nil && settings.fields != nil {
			fields, err = api.fieldsFor(r, handled, settings.fields)
		}

		respond(w, handled.Events, fields, err, api.explain(r))
	}))
}

// methodOf returns the method that a pattern of http.ServeMux names, or "" if
// it names none. It reads the pattern as the mux does, which takes what comes
// before the first space or tab for the method, if there is one.
func methodOf(pattern string) string {
	if i := strings.IndexAny(pattern, " \t"); i >= 0 {
		return pattern[:i]
	}

	return ""
}

// fieldsFor asks the function of Adding for the fields of the answer to a
// command that has succeeded (see fieldsOf).
//
// If the function fails, the command has succeeded all the same, so the
// answer stays a success, whatever is wrong with the fields the function
// returned along with its error. Fields that can not be used are dropped
// then, rather than answered with 500, and why is logged with the error.
func (api *API[TUser]) fieldsFor[TCommand any](
	r *http.Request,
	handled Handled[TCommand],
	adding func(Handled[TCommand]) (any, error),
) (map[string]any, error) {
	value, failure := adding(handled)
	fields, err := fieldsOf(value)

	if failure != nil {
		api.logIncomplete(r, failure, err)

		if err != nil {
			return nil, nil
		}
	}

	return fields, err
}

// statusClientClosedRequest is what a request gets whose caller went away
// before it was answered. HTTP has no status for it, so this is the one nginx
// introduced, and which logs and metrics commonly know. Nobody reads the answer,
// so it is there for them only.
const statusClientClosedRequest = 499

// StatusFor maps an error to an HTTP status. It asks for error categories
// rather than concrete errors, so new failures do not need a new case here.
//
// A query that expects exactly one item and finds none, such as one of
// query.Single, fails with query.ErrNoItems, which maps to 404 without having
// to be translated into ErrNotFound.
//
// An error that wraps architecturekit.ErrNotARevision maps to 400, since the
// value that is not a revision was handed over, such as a bound of
// architecturekit.Read, the event ID of architecturekit.OnEventID, or the
// revision a view is to wait for, which usually come from the request. An
// error of the category architecturekit.ErrPermanent maps to 500 even then,
// since an ID that the server stored or made itself is broken. An error that
// wraps architecturekit.ErrEmptyRange maps to 400 the same way, since bounds
// of architecturekit.Read that leave no room for an event usually come from
// the request as well.
//
// An error because the context ended belongs to no category. If the request
// was canceled, which happens when the caller goes away, it maps to 499, which
// is not logged, since nothing failed. If its deadline ran out, the server
// took too long, which maps to 503 and is logged.
//
// An error that wraps architecturekit.ErrOutcomeUnknown, of a write that may
// or may not have stored its events, belongs to no category either. It maps
// to 500, since trying again, which 409 and 503 invite, may store the events
// twice, and 499 is not logged. So it comes before ErrConflict, ErrTransient,
// and the end of the context, also if it wraps one of them as well.
//
// The status says nothing about what to tell the caller. Respond,
// RespondResult, and RespondError explain only an error that is written for
// the caller, and an answer in a format of your own should do the same: the
// error of a 401, a 409, a 499, or a status of 500 and above may name
// internals. For a write whose outcome is unknown, tell the caller that the
// request may have succeeded.
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
	// Trying again may store the events of a write whose outcome is unknown
	// twice, so it must not get a status that invites it.
	case errors.Is(err, architecturekit.ErrOutcomeUnknown):
		return http.StatusInternalServerError
	// A conflict is transient, but 409 says more than 503, so it comes first.
	case errors.Is(err, architecturekit.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, architecturekit.ErrTransient):
		return http.StatusServiceUnavailable
	case errors.Is(err, context.Canceled):
		return statusClientClosedRequest
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable
	// A permanent failure is the server's mistake, even if it says that an ID
	// is not a revision, or that a range is empty, so it comes before
	// ErrNotARevision and ErrEmptyRange. They all come last, so that an error
	// that has a status of its own above keeps it.
	case errors.Is(err, architecturekit.ErrPermanent):
		return http.StatusInternalServerError
	case errors.Is(err, architecturekit.ErrNotARevision), errors.Is(err, architecturekit.ErrEmptyRange):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// Respond writes the kit's default answer to a command. On success, that is
// the revision the command wrote, the ID of the last event, which a caller
// hands to a query to read its own writes (see Revisioned and Awaiting):
//
//	{"revision": "42"}
//
// The revision is empty if the command wrote nothing. On failure, it is a
// message, which explains a failure the caller can fix, such as a broken
// business rule or a malformed request. Other errors may name internals, so
// their message is fixed:
//
//   - 401 says "unauthorized", and 409 says "conflict: the data has changed
//     since it was read", while the error is logged at level Info, since the
//     server did not fail.
//   - 404 says "not found" for a query that found no item, whose error
//     query.ErrNoItems does not say what was not found, and it is logged the
//     same way. An error of ErrNotFound, on the other hand, is written for
//     the caller, and so it is the message.
//   - 499, for a request that was canceled, says "request canceled", and is
//     not logged, since nothing failed.
//   - 500 and above say "internal server error", while the failure is logged
//     at level Error. Only 500 for a write whose outcome is unknown,
//     architecturekit.ErrOutcomeUnknown, says "outcome unknown: the request
//     may have succeeded", so that the caller does not simply try again.
//
// Each of them is logged through the logger of the API, with the route of the
// request (see WithLogger).
//
// Every answer, a success as well as a failure, says Cache-Control: no-store,
// so that no cache keeps it.
//
// Unlike Route and Handle, Respond does not know where an error comes from, so
// an error without a status of its own is answered with 500, also one that a
// handler of your own has found in the request. Wrap such an error with
// ErrMalformed to answer it with 400.
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
	respond(w, written, nil, err, api.explain(r))
}

// RespondError answers an error without a result, exactly as Respond and
// RespondResult answer one: with the status that StatusFor maps it to, the
// same messages, Cache-Control: no-store, and the same logging (see
// WithLogger). Use it in a handler of your own that answers a success in a
// format of its own, such as a download, and a failure in the kit's, such as
// one of UserOf or Ask.
//
// As with Respond, an error without a status of its own is answered with 500,
// so wrap a mistake in the request that a handler of your own has found with
// ErrMalformed, to answer it with 400.
//
// A nil API is a programming error, so RespondError panics, as Respond and
// RespondResult do, and so is a nil error, since there is nothing to answer.
// The API is checked first.
func RespondError[TUser any](
	w http.ResponseWriter,
	r *http.Request,
	api *API[TUser],
	err error,
) {
	explain := api.explain(r)

	if err == nil {
		panic("architecturekit/httpapi: RespondError needs an error, not nil")
	}

	respondResult(w, struct{}{}, err, explain)
}

// respond writes the answer to a command, with the given fields next to the
// revision, and explains an error with explain. It writes the same way as a
// query result is written (see respondResultAt).
func respond(
	w http.ResponseWriter,
	written []eventsourcingdb.Event,
	fields map[string]any,
	err error,
	explain func(status int, err error) string,
) {
	var answer map[string]any
	if err == nil {
		answer = map[string]any{"revision": architecturekit.RevisionOf(written)}
		maps.Copy(answer, fields)
	}

	respondResult(w, answer, err, explain)
}

// fieldsOf turns what the function of Adding returned into the fields of the
// answer. A value that encodes to null holds no fields, such as nil, and also
// a nil pointer of a concrete type, which an interface does not count as nil.
// The value is encoded as a result is (see answerJSON), so a nil slice or map
// in it is [] or {}, and the fields are decoded with numbers kept as they are,
// so that a large integer does not lose digits on its way through a float.
//
// A value that can not be encoded, also because a MarshalJSON function
// panics, that does not encode to a JSON object, or that holds a revision,
// which the kit adds itself, is an error.
func fieldsOf(value any) (fields map[string]any, err error) {
	defer recoverInto(&err)

	// The error is wrapped with %v rather than %w, since fields that can not
	// be encoded are a mistake in the code, which has to be answered with 500,
	// whatever category the error of a MarshalJSON function has.
	encoded, err := jsonv2.Marshal(value, answerJSON)
	if err != nil {
		return nil, fmt.Errorf("httpapi: encoding the fields of the answer: %v", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()

	// null leaves the map nil, without an error, which holds no fields.
	if err := decoder.Decode(&fields); err != nil {
		return nil, fmt.Errorf("httpapi: the fields of the answer must encode to a JSON object, not %s", encoded)
	}
	if _, hasRevision := fields["revision"]; hasRevision {
		return nil, errors.New("httpapi: the fields of the answer must not contain a revision, which the kit adds itself")
	}

	return fields, nil
}

// categorise leaves an error of ToCommand or ToQuery that already says what
// kind it is alone (see hasCategory), and marks everything else as a
// malformed request, which it wraps, so that errors.Is and errors.As still
// find it.
//
// Without this, an application that returns ErrForbidden from ToCommand would
// see its 403 turned into a 400, and one whose session store is down would
// tell the caller that the request is to blame.
func categorise(err error) error {
	if hasCategory(err) {
		return err
	}

	return fmt.Errorf("%w: %w", ErrMalformed, err)
}

// hasCategory reports whether an error says what kind it is: whether
// StatusFor maps it to a status of its own, or it is of the category
// architecturekit.ErrPermanent, or wraps architecturekit.ErrOutcomeUnknown,
// both of which map to 500 like an error without any category, but on
// purpose.
func hasCategory(err error) bool {
	return StatusFor(err) != http.StatusInternalServerError ||
		errors.Is(err, architecturekit.ErrPermanent) || errors.Is(err, architecturekit.ErrOutcomeUnknown)
}

// panicError is a panic while a request was handled, which is answered like
// any other internal failure, and logged with its value and its stack (see
// logFailure).
//
// It does not unwrap to the value, even if that is an error, since a panic is
// a mistake in the code, whatever it panicked with, and so always maps to 500.
type panicError struct {
	value any
	stack []byte
}

func (failure *panicError) Error() string {
	return fmt.Sprintf("httpapi: panic while handling the request: %v", failure.value)
}

// panicked turns what recover returned into an error, which is nil if nothing
// panicked. A panic with http.ErrAbortHandler goes on, since net/http expects
// it to abort the response, and does not log it.
func panicked(value any) error {
	if value == nil {
		return nil
	}
	if value == http.ErrAbortHandler {
		panic(value)
	}

	return &panicError{value: value, stack: debug.Stack()}
}

// recoverInto turns a panic into the error that err points to (see
// panicked). It has to be deferred, so that recover sees the panic.
func recoverInto(err *error) {
	if failure := panicked(recover()); failure != nil {
		*err = failure
	}
}

// answerPanic answers a panic with 500, like any other internal failure (see
// panicked). It has to be deferred, so that recover sees the panic.
func (api *API[TUser]) answerPanic(w http.ResponseWriter, r *http.Request) {
	if failure := panicked(recover()); failure != nil {
		respondResult(w, struct{}{}, failure, api.explain(r))
	}
}

// BodyOf decodes the JSON body of a request by the rules that Route and Query
// apply to the body of a command or a query, for a handler of your own that
// reads the body itself. The Content-Type has to be application/json, or it is
// ErrUnsupportedMediaType. The body may hold at most MaxRequestBody bytes, or
// it is ErrTooLarge, and so is a body over a lower limit that a middleware set
// with http.MaxBytesReader. JSON that does not fit TBody, including a field
// that TBody does not have, is ErrMalformed.
//
// So is anything but whitespace after the JSON value, such as a second value,
// and an object in which a name occurs twice. Names match fields regardless of
// case, as with encoding/json, so two names that match the same field count
// as the same name, even if they differ in case.
//
// The error says what is wrong in words of its own, rather than in those of
// the reader, which name its package, or of the decoder, which name the types
// of Go. After the text of ErrMalformed, it says one of these:
//
//	the body could not be read
//	empty body
//	invalid JSON
//	data after the JSON value
//	unknown field "quantiy"
//	duplicate field "customerId"
//	"quantity" must be a number
//	"quantity" is out of range
//	"quantity" must be an integer
//	"cover" must be base64
//	"pair" must have 2 elements
//	"dueOn" must be a time such as "2026-10-05T12:00:00Z"
//	"quantity" must be a string that holds a number
//	the keys of "stock" must be numbers
//	"rating" can not be decoded
//
// It names a value by its path, as encoding/json does, such as
// "delivery.address" or "items.0.bookId", or the body as a whole, as in "the
// body must be an object". A value of the wrong kind names the kind it has to
// be, which is a string, a number, a boolean, an array, or an object. A number
// is out of range if it is too large or too small for its type, which a
// negative number is for an unsigned integer, and an integer has to be
// written as one, without a fraction or an exponent. A field with the option
// string takes its number, boolean, or string in a string, and the keys of a
// map whose keys are times have to be times.
//
// An array has to have as many elements as the Go array that it is decoded
// into, such as [2]int, which encoding/json would fill up with zeros, or cut
// off, without a word. A body of null is refused as well, since encoding/json
// takes it for no value at all, and would hand back the zero value of TBody.
// The error says which kind the body has to be, as in "the body must be an
// object". Only a type whose kind is not clear, such as one that decodes
// itself, gets null as any other value, and decides itself what it means,
// and an interface takes it for nil. null for a field still leaves the field
// as it is, as with encoding/json.
//
// The error of a type that decodes itself keeps its own text, as it is, also
// for a type of a library, such as netip.Addr, as long as it can tell its
// text, which it can not if its Error method panics, as that of a
// *json.UnmarshalTypeError without a type does. Such an error, and any other
// failure that there are no words for, such as a value of a type that JSON has
// no kind for, says that the value can not be decoded, or that the body can
// not be decoded, if it is not clear which value. Either way, the error wraps
// the error of decoding, so that errors.As finds it, such as a
// *json.UnmarshalTypeError that names the field.
//
// If TBody is NoBody, the request is read without a body instead. A request
// that a browser sends from another origin is ErrForbidden then, before the
// body is looked at (see NoBody). The Content-Type does not matter, since
// there is no body for it to describe, and the body has to be empty, or {},
// the object without fields, whatever the Content-Type says. Any other body
// is ErrMalformed, which says that the route takes no body, and one of more
// than MaxRequestBody bytes is still ErrTooLarge.
func BodyOf[TBody any](r *http.Request) (TBody, error) {
	var value TBody

	if _, takesNoBody := any(value).(NoBody); takesNoBody {
		return value, requireNoBody(r)
	}

	if err := requireJSON(r); err != nil {
		return value, err
	}

	body, err := readBody(r)
	if err != nil {
		return value, err
	}

	if err := refuseNull(reflect.TypeFor[TBody](), body); err != nil {
		return value, fmt.Errorf("%w: %w", ErrMalformed, err)
	}

	value, err = decodeStrictly[TBody](body)
	if err != nil {
		return value, fmt.Errorf("%w: %w", ErrMalformed, err)
	}

	return value, nil
}

// strictJSON are the rules of encoding/json, which match names regardless of
// case, except that unknown fields and names that occur twice are rejected
// (see decodeStrictly), and so is an array whose length differs from that of
// the Go array that it is decoded into, which encoding/json would fill up
// with zeros, or cut off, without a word.
var strictJSON = jsonv2.JoinOptions(
	json.DefaultOptionsV1(),
	jsonv2.RejectUnknownMembers(true),
	jsontext.AllowDuplicateNames(false),
	json.UnmarshalArrayFromAnyLength(false),
)

// decodeStrictly decodes a body that holds exactly one JSON value, with
// nothing but whitespace after it, and hands back the zero value if it fails,
// so that nothing half-decoded gets out. Its error says what is wrong in words
// of its own, as far as it has them (see describeDecoding).
//
// Unknown fields are rejected rather than dropped, so that a misspelled field
// cannot silently turn into a zero value. A name that occurs twice, and
// anything but whitespace after the value, are rejected as well, since
// parsers disagree on what they mean: one takes the first value, another the
// last, and one stops after the value, while another reads on. A filter or a
// proxy in front of the application might then check another value than the
// one the application uses.
func decodeStrictly[TBody any](body []byte) (TBody, error) {
	var value TBody

	err := jsonv2.Unmarshal(body, &value, strictJSON)
	if err == nil {
		return value, nil
	}

	// encoding/json does not say where some failures are, such as an unknown
	// field, so the body is decoded once more, reporting errors the way
	// encoding/json/v2 does, which points to them.
	detailed := jsonv2.Unmarshal(body, new(TBody), strictJSON, json.ReportErrorsWithLegacySemantics(false))

	var zero TBody

	return zero, describeDecoding(reflect.TypeFor[TBody](), body, err, detailed)
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

// crossOrigin tells a request that a browser sends from another origin (see
// NoBody). It has no trusted origins.
var crossOrigin = http.NewCrossOriginProtection()

// requireNoBody refuses a request that a browser sends from another origin,
// before it looks at the body. Then it reads the body, and accepts it only if
// it is empty, or {}.
func requireNoBody(r *http.Request) error {
	if crossOrigin.Check(r) != nil {
		return fmt.Errorf("%w: a request without a body is not accepted from another origin", ErrForbidden)
	}

	// A request that a server receives always has a body, which is empty if
	// the caller sent none, but one that is made with http.NewRequest without
	// a body, as in a test of a handler, has nil instead.
	if r.Body == nil {
		return nil
	}

	body, err := readBody(r)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}

	// null decodes into a nil pointer, so only {} gives one that is not nil.
	if empty, err := decodeStrictly[*NoBody](body); err == nil && empty != nil {
		return nil
	}

	return fmt.Errorf("%w: this route takes no body, so the body has to be empty, or {}", ErrMalformed)
}

// readBody reads at most MaxRequestBody bytes and tells a body that is too
// large apart from one that could not be read. A body that a middleware cuts
// off at a lower limit, with http.MaxBytesReader, is too large as well.
//
// The error of reading is the reader's, such as that of a middleware that
// decompresses the body, whose text names its package, such as "gzip:
// invalid checksum". So the caller is told that the body could not be read,
// in words of its own, while the error still wraps it.
func readBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBody+1))
	if tooLarge, isTooLarge := errors.AsType[*http.MaxBytesError](err); isTooLarge {
		return nil, fmt.Errorf("%w: at most %d bytes are read", ErrTooLarge, tooLarge.Limit)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, &decodeFailure{text: "the body could not be read", causes: []error{err}})
	}

	if len(body) > MaxRequestBody {
		return nil, fmt.Errorf("%w: at most %d bytes are read", ErrTooLarge, MaxRequestBody)
	}

	return body, nil
}
