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
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// notCaughtUp returns what WaitForWritten returns once the time has run out,
// as a step that waits for a write of its own does.
func notCaughtUp(t *testing.T) error {
	t.Helper()

	view := architecturekit.NewInMemoryView(func(item string) string { return item })
	err := architecturekit.WaitForWritten(context.Background(), view,
		[]eventsourcingdb.Event{{ID: "7", Subject: "/sessions/23"}}, time.Millisecond)
	require.ErrorIs(t, err, architecturekit.ErrNotCaughtUp)

	return err
}

// assertAnsweredAsInternalFailure asserts that an answer says neither that
// the request has succeeded nor anything that invites trying again, and that
// the failure was logged with its details.
func assertAnsweredAsInternalFailure(t *testing.T, response *httptest.ResponseRecorder, logs, detail string) {
	t.Helper()

	assert.Equal(t, http.StatusInternalServerError, response.Code, "503 would invite trying again")
	assert.JSONEq(t, `{"message": "internal server error"}`, response.Body.String(),
		"nothing of the request has run, so it must not be told that it has succeeded")
	assert.Equal(t, 1, strings.Count(logs, "\n"), "want exactly one entry")
	assert.Contains(t, logs, `level=ERROR msg="httpapi: internal failure"`)
	assert.Contains(t, logs, detail, "the details have to reach the log")
}

func TestNotCaughtUpBeforeTheRequestHasRun(t *testing.T) {
	t.Run("a route answers it from ToCommand as an internal failure, without running the command", func(t *testing.T) {
		var logs bytes.Buffer
		api := httpapi.NewAPI(writingStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
		failure := notCaughtUp(t)

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
		assertAnsweredAsInternalFailure(t, response, logs.String(), "did not catch up within 1ms")
	})

	for name, options := range map[string][]httpapi.QueryOption{
		"a query":            nil,
		"a revisioned query": {httpapi.Revisioned(noteView(), time.Second)},
		"an awaiting query":  {httpapi.Awaiting(noteView(), time.Second)},
	} {
		t.Run(name+" answers it from ToQuery as an internal failure, without answering the query", func(t *testing.T) {
			var logs bytes.Buffer
			api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
			failure := notCaughtUp(t)

			hasAnswered := false
			mux := http.NewServeMux()
			httpapi.Query(api, mux, "QUERY /notes", failingToQuery(failure),
				func(ctx context.Context, query listNotes) ([]noteResponse, error) {
					hasAnswered = true
					return answerListNotes(ctx, query)
				}, options...)

			response := ask(t, mux, "/notes", "golo")

			assert.False(t, hasAnswered, "the query must not be answered")
			assertAnsweredAsInternalFailure(t, response, logs.String(), "did not catch up within 1ms")
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
	} {
		t.Run(name+" answers it from userFrom as an internal failure", func(t *testing.T) {
			var logs bytes.Buffer
			api := httpapi.NewAPI(writingStore(t), userFromFailing(notCaughtUp(t)), httpapi.WithLogger(loggerInto(&logs)))

			response := enter(t, api)

			assertAnsweredAsInternalFailure(t, response, logs.String(), "did not catch up within 1ms")
		})
	}

	t.Run("Handle, Ask, and UserOf return it marked, so that a handler of your own answers it as an internal failure", func(t *testing.T) {
		failure := notCaughtUp(t)

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

					require.ErrorIs(t, err, architecturekit.ErrNotCaughtUp, "errors.Is has to find it")
					assert.ErrorIs(t, err, failure, "errors.Is has to find the error itself")
					assert.EqualError(t, err, failure.Error(), "the mark keeps the text")
					assert.Equal(t, http.StatusInternalServerError, httpapi.StatusFor(err))

					var logs bytes.Buffer
					request, api := inAHandler(&logs)
					response := httptest.NewRecorder()

					answer(response, request, api, err)

					assertAnsweredAsInternalFailure(t, response, logs.String(), "did not catch up within 1ms")
				})
			}
		}
	})

	t.Run("a mark survives wrapping, so that a handler of your own may add to the error", func(t *testing.T) {
		api := httpapi.NewAPI(writingStore(t), userFrom)
		_, err := httpapi.Handle(postingTo("/note", `{"id":"1","text":"hello"}`), api,
			failingToCommand(notCaughtUp(t)), noteDecider())

		var logs bytes.Buffer
		request, handlerAPI := inAHandler(&logs)
		response := httptest.NewRecorder()

		httpapi.RespondError(response, request, handlerAPI, errors.Join(errors.New("registering the instance"), err))

		assertAnsweredAsInternalFailure(t, response, logs.String(), "did not catch up within 1ms")
	})
}

// failingView is a view whose WaitFor fails with the given error.
type failingView struct {
	err error
}

func (failingView) Revision(context.Context) (string, error) { return "", nil }

func (v failingView) WaitFor(context.Context, string) error { return v.err }

func TestNotCaughtUpAfterTheCommand(t *testing.T) {
	written := []eventsourcingdb.Event{{ID: "7", Subject: "/instances/x"}}
	emptyView := func() architecturekit.Revisioned {
		return architecturekit.NewInMemoryView(func(item string) string { return item })
	}

	// Whatever keeps the view from catching up, the write has succeeded, so
	// the caller must neither be invited to try again nor be told that the
	// request failed.
	for name, wait := range map[string]func(t *testing.T) error{
		"once the time has run out": func(*testing.T) error {
			return architecturekit.WaitForWritten(context.Background(), emptyView(), written, time.Millisecond)
		},
		"once the deadline of the request has run out first": func(t *testing.T) error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
			t.Cleanup(cancel)

			return architecturekit.WaitForWritten(ctx, emptyView(), written, httpapi.DefaultWait)
		},
		"once the caller went away": func(t *testing.T) error {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			return architecturekit.WaitForWritten(ctx, emptyView(), written, httpapi.DefaultWait)
		},
		"once the view failed for now": func(*testing.T) error {
			failure := errors.New("polling the revision: connection refused")
			return architecturekit.WaitForWritten(context.Background(),
				failingView{err: errors.Join(architecturekit.ErrTransient, failure)}, written, time.Second)
		},
		"once the view failed for good": func(*testing.T) error {
			return architecturekit.WaitForWritten(context.Background(),
				failingView{err: architecturekit.ErrPermanent}, written, time.Second)
		},
	} {
		for answerer, answer := range answerers {
			t.Run(answerer+" answers a view that did not catch up "+name+" with 500 and the text that the request succeeded", func(t *testing.T) {
				err := wait(t)
				require.ErrorIs(t, err, architecturekit.ErrNotCaughtUp)

				var logs bytes.Buffer
				request, api := inAHandler(&logs)
				response := httptest.NewRecorder()

				answer(response, request, api, err)

				assert.Equal(t, http.StatusInternalServerError, response.Code, "neither 503 nor 499 fit a write that has succeeded")
				assert.JSONEq(t, `{"message": "the request succeeded, but its result is not visible yet"}`, response.Body.String())
				assert.Contains(t, logs.String(), `level=ERROR msg="httpapi: internal failure"`)
				assert.Contains(t, logs.String(), "the events were written", "the details have to reach the log")
			})
		}
	}

	t.Run("a handler of your own that waits after the command answers it as a success that is not visible yet", func(t *testing.T) {
		var logs bytes.Buffer
		api := httpapi.NewAPI(writingStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
		view := architecturekit.NewInMemoryView(func(item string) string { return item })

		mux := http.NewServeMux()
		mux.HandleFunc("POST /note", func(w http.ResponseWriter, r *http.Request) {
			handled, err := httpapi.Handle(r, api, toNote, noteDecider())
			if err == nil {
				err = architecturekit.WaitForWritten(r.Context(), view, handled.Events, time.Millisecond)
			}

			httpapi.Respond(w, r, api, handled.Events, err)
		})

		request := postingTo("/note", `{"id":"1","text":"hello"}`)
		request.Pattern = "POST /note"
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)

		assert.Equal(t, http.StatusInternalServerError, response.Code)
		assert.JSONEq(t, `{"message": "the request succeeded, but its result is not visible yet"}`, response.Body.String())
		assert.Contains(t, logs.String(), `level=ERROR msg="httpapi: internal failure"`)
		assert.Contains(t, logs.String(), "did not catch up within 1ms")
	})
}
