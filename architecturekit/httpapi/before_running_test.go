package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/httpapi"
)

// assertAnsweredAsInternalFailure asserts that an answer says neither that
// the request has succeeded nor anything that invites trying again, and that
// the failure was logged with its details.
func assertAnsweredAsInternalFailure(t *testing.T, response *httptest.ResponseRecorder, logs, detail string) {
	t.Helper()

	assert.Equal(t, http.StatusInternalServerError, response.Code, "503 would invite trying again")
	assert.JSONEq(t, `{"message": "internal server error"}`, response.Body.String(),
		"nothing of the request has run, so it must not be told that it may have succeeded")
	assert.Equal(t, 1, strings.Count(logs, "\n"), "want exactly one entry")
	assert.Contains(t, logs, `level=ERROR msg="httpapi: internal failure"`)
	assert.Contains(t, logs, detail, "the details have to reach the log")
}

// failuresBeforeRunning are the failures that a fixed text of their own
// answers after the command, which says that the request may have succeeded,
// or that it has, together with what of them has to reach the log. Before
// the request has run, they are answered as any other internal failure.
var failuresBeforeRunning = map[string]struct {
	failure func(t *testing.T) error
	detail  string
}{
	"a write whose outcome is unknown": {outcomeUnknown, "outcome unknown: writing 1 events"},
	"a view that did not catch up":     {notCaughtUp, "did not catch up within 1ms"},
}

func TestBeforeTheRequestHasRun(t *testing.T) {
	for kind, failing := range failuresBeforeRunning {
		t.Run(kind, func(t *testing.T) {
			t.Run("a route answers it from ToCommand as an internal failure, without running the command", func(t *testing.T) {
				var logs bytes.Buffer
				api := httpapi.NewAPI(writingStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
				failure := failing.failure(t)

				hasDecided := false
				mux := http.NewServeMux()
				httpapi.Route(api, mux, "POST /note", failingToCommand(failure),
					architecturekit.NewDecider(architecturekit.NewState(notes{}),
						func(context.Context, note, notes) ([]architecturekit.Event, error) {
							hasDecided = true
							return []architecturekit.Event{noted{Text: "hello"}}, nil
						}))

				response := postNote(t, mux, `{"id":"1","text":"hello"}`)

				assert.False(t, hasDecided, "the command must not run")
				assertAnsweredAsInternalFailure(t, response, logs.String(), failing.detail)
			})

			for name, options := range map[string][]httpapi.QueryOption{
				"a query":            nil,
				"a revisioned query": {httpapi.Revisioned(noteView(), time.Second)},
				"an awaiting query":  {httpapi.Awaiting(noteView(), time.Second)},
			} {
				t.Run(name+" answers it from ToQuery as an internal failure, without answering the query", func(t *testing.T) {
					var logs bytes.Buffer
					api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
					failure := failing.failure(t)

					hasAnswered := false
					mux := http.NewServeMux()
					httpapi.Query(api, mux, "QUERY /notes", failingToQuery(failure),
						func(ctx context.Context, query listNotes) ([]noteResponse, error) {
							hasAnswered = true
							return answerListNotes(ctx, query)
						}, options...)

					response := ask(t, mux, "/notes", "golo")

					assert.False(t, hasAnswered, "the query must not be answered")
					assertAnsweredAsInternalFailure(t, response, logs.String(), failing.detail)
				})
			}

			for name, enter := range map[string]func(t *testing.T, api *httpapi.API[user]) *httptest.ResponseRecorder{
				"a route": func(t *testing.T, api *httpapi.API[user]) *httptest.ResponseRecorder {
					return postNote(t, routed(api), `{"id":"1","text":"hello"}`)
				},
				"a query": func(t *testing.T, api *httpapi.API[user]) *httptest.ResponseRecorder {
					mux := http.NewServeMux()
					httpapi.Query(api, mux, "QUERY /notes", toListNotes, answerListNotes)

					return ask(t, mux, "/notes", "golo")
				},
				"a revisioned query": func(t *testing.T, api *httpapi.API[user]) *httptest.ResponseRecorder {
					mux := http.NewServeMux()
					httpapi.Query(api, mux, "QUERY /notes", toListNotes, answerListNotes,
						httpapi.Revisioned(noteView(), time.Second))

					return ask(t, mux, "/notes", "golo")
				},
				"an awaiting query": func(t *testing.T, api *httpapi.API[user]) *httptest.ResponseRecorder {
					mux := http.NewServeMux()
					httpapi.Query(api, mux, "QUERY /notes", toListNotes, answerListNotes,
						httpapi.Awaiting(noteView(), time.Second))

					return ask(t, mux, "/notes", "golo")
				},
			} {
				t.Run(name+" answers it from userFrom as an internal failure", func(t *testing.T) {
					var logs bytes.Buffer
					api := httpapi.NewAPI(writingStore(t), userFromFailing(failing.failure(t)),
						httpapi.WithLogger(loggerInto(&logs)))

					response := enter(t, api)

					assertAnsweredAsInternalFailure(t, response, logs.String(), failing.detail)
				})
			}

			t.Run("Handle, Ask, and UserOf return it marked, so that a handler of your own answers it as an internal failure", func(t *testing.T) {
				failure := failing.failure(t)

				for name, returnError := range map[string]func(t *testing.T) error{
					"Handle": func(t *testing.T) error {
						api := httpapi.NewAPI(writingStore(t), userFrom)
						_, err := httpapi.Handle(postingTo("/note", `{"id":"1","text":"hello"}`), api, failingToCommand(failure), noteDecider())

						return err
					},
					"Ask": func(t *testing.T) error {
						api := httpapi.NewAPI(deadStore(t), userFrom)
						request := httptest.NewRequest("QUERY", "/notes", nil)
						request.Header.Set("X-User", "golo")
						_, err := httpapi.Ask(request, api, failingToQuery(failure), answerListNotes)

						return err
					},
					"UserOf": func(t *testing.T) error {
						api := httpapi.NewAPI(deadStore(t), userFromFailing(failure))
						_, err := httpapi.UserOf(httptest.NewRequest(http.MethodGet, "/", nil), api)

						return err
					},
				} {
					for answerer, answer := range answerers {
						t.Run(name+" with "+answerer, func(t *testing.T) {
							err := returnError(t)

							require.ErrorIs(t, err, failure, "errors.Is has to find the error itself")
							assert.EqualError(t, err, failure.Error(), "the mark keeps the text")
							assert.Equal(t, http.StatusInternalServerError, httpapi.StatusFor(err))

							var logs bytes.Buffer
							request, api := inAHandler(&logs)
							response := httptest.NewRecorder()

							answer(response, request, api, err)

							assertAnsweredAsInternalFailure(t, response, logs.String(), failing.detail)
						})
					}
				}
			})

			t.Run("a mark survives wrapping, so that a handler of your own may add to the error", func(t *testing.T) {
				api := httpapi.NewAPI(writingStore(t), userFrom)
				_, err := httpapi.Handle(postingTo("/note", `{"id":"1","text":"hello"}`), api,
					failingToCommand(failing.failure(t)), noteDecider())

				var logs bytes.Buffer
				request, handlerAPI := inAHandler(&logs)
				response := httptest.NewRecorder()

				httpapi.RespondError(response, request, handlerAPI, errors.Join(errors.New("registering the instance"), err))

				assertAnsweredAsInternalFailure(t, response, logs.String(), failing.detail)
			})
		})
	}
}
