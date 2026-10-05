package httpapi_test

import (
	"bytes"
	"context"
	"fmt"
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

// A panic while a request is handled is a mistake in the code, such as a
// subject built from an ID that ToCommand did not check. Left to net/http, it
// closes the connection, and the caller gets no answer at all.

var noteSubject = architecturekit.NewSubjectScheme("/note/{id}")

// uncheckedNote builds its subject from an ID that nobody checked, so Build
// panics for an empty one, or one with a slash.
type uncheckedNote struct {
	ID string
}

func (c uncheckedNote) Subject() string { return noteSubject.Build(c.ID) }

func (uncheckedNote) Preconditions() []architecturekit.Precondition {
	return []architecturekit.Precondition{architecturekit.Unconditionally()}
}

type uncheckedNoteRequest struct {
	ID string `json:"id"`
}

func toUncheckedNote(_ *http.Request, request uncheckedNoteRequest, _ user) (uncheckedNote, error) {
	return uncheckedNote(request), nil
}

func uncheckedNoteDecider() architecturekit.Decider[uncheckedNote, notes] {
	return architecturekit.Decider[uncheckedNote, notes]{
		State: architecturekit.NewState(notes{}),
		Decide: func(context.Context, uncheckedNote, notes) ([]architecturekit.Event, error) {
			return nil, nil
		},
	}
}

// toNotePanicking panics while it turns a request into a command, with an
// error that claims to be transient.
func toNotePanicking(*http.Request, noteRequest, user) (note, error) {
	panic(fmt.Errorf("%w: the session store is down", architecturekit.ErrTransient))
}

// toNoteAborting aborts the response while it turns a request into a command,
// the way net/http expects a handler to.
func toNoteAborting(*http.Request, noteRequest, user) (note, error) {
	panic(http.ErrAbortHandler)
}

// explosive panics while it is encoded, as a MarshalJSON function with a bug
// does.
type explosive struct{}

func (explosive) MarshalJSON() ([]byte, error) {
	panic("the encoder is broken")
}

// serve hands a request to the mux, and fails the test if a panic gets out.
func serve(t *testing.T, mux *http.ServeMux, request *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	recorder := httptest.NewRecorder()
	require.NotPanics(t, func() { mux.ServeHTTP(recorder, request) }, "the panic got out of the route")

	return recorder
}

// postUncheckedNote sends an unchecked note with the given ID to the mux.
func postUncheckedNote(t *testing.T, mux *http.ServeMux, id string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, "/note", strings.NewReader(`{"id":"`+id+`"}`))
	request.Header.Set("X-User", "golo")
	request.Header.Set("Content-Type", "application/json")

	return serve(t, mux, request)
}

// askAsGolo asks the mux for the path, as a known user.
func askAsGolo(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("X-User", "golo")

	return serve(t, mux, request)
}

// assertPanicAnswered asserts that a panic was answered like any other
// internal failure, and logged once, with the request, its value, and the
// stack it happened on.
func assertPanicAnswered(t *testing.T, response *httptest.ResponseRecorder, logs, method, route, value, frame string) {
	t.Helper()

	assert.Equal(t, http.StatusInternalServerError, response.Code)
	assert.JSONEq(t, `{"message": "internal server error"}`, response.Body.String())

	assert.Equal(t, 1, strings.Count(logs, "\n"), "want exactly one entry")
	assert.Contains(t, logs, `level=ERROR msg="httpapi: internal failure"`)
	assert.Contains(t, logs, "method="+method)
	assert.Contains(t, logs, fmt.Sprintf("route=%q", route))
	assert.Contains(t, logs, fmt.Sprintf("panic=%q", value), "the log has to hold the value of the panic")
	assert.Contains(t, logs, "stack=", "the log has to hold the stack")
	assert.Contains(t, logs, frame, "the stack has to show where the panic happened")
}

func TestPanicsInRoutes(t *testing.T) {
	t.Run("a command whose subject panics is answered with 500, and logged with the stack", func(t *testing.T) {
		var logs bytes.Buffer
		api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
		mux := http.NewServeMux()
		httpapi.Route(api, mux, "POST /note", toUncheckedNote, uncheckedNoteDecider())

		response := postUncheckedNote(t, mux, "")

		assertPanicAnswered(t, response, logs.String(), "POST", "POST /note",
			`architecturekit: value for "id" in "/note/{id}" must not be empty`, "httpapi_test.uncheckedNote.Subject")
	})

	t.Run("a command whose subject panics is answered over a real connection", func(t *testing.T) {
		var logs bytes.Buffer
		api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
		mux := http.NewServeMux()
		httpapi.Route(api, mux, "POST /note", toUncheckedNote, uncheckedNoteDecider())

		server := httptest.NewServer(mux)
		defer server.Close()

		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/note", strings.NewReader(`{"id":""}`))
		require.NoError(t, err)
		request.Header.Set("X-User", "golo")
		request.Header.Set("Content-Type", "application/json")

		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err, "the connection was closed without an answer")
		defer response.Body.Close()

		assert.Equal(t, http.StatusInternalServerError, response.StatusCode)
	})

	t.Run("a command whose fields panic after it succeeded is answered with 500", func(t *testing.T) {
		var logs bytes.Buffer
		mux := routed(httpapi.NewAPI(writingStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs))),
			httpapi.Adding(func(httpapi.Handled[note]) (any, error) {
				panic("the fields are broken")
			}))

		request := httptest.NewRequest(http.MethodPost, "/note", strings.NewReader(`{"id":"1","text":"hello"}`))
		request.Header.Set("X-User", "golo")
		request.Header.Set("Content-Type", "application/json")

		response := serve(t, mux, request)

		assertPanicAnswered(t, response, logs.String(), "POST", "POST /note", "the fields are broken", "httpapi_test.TestPanicsInRoutes")
	})

	t.Run("a query whose answer panics is answered with 500", func(t *testing.T) {
		var logs bytes.Buffer
		api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
		mux := http.NewServeMux()
		httpapi.Query(api, mux, "GET /notes", toListNotes, func(context.Context, listNotes) ([]noteResponse, error) {
			panic("the index is broken")
		})

		response := askAsGolo(t, mux, "/notes")

		assertPanicAnswered(t, response, logs.String(), "GET", "GET /notes", "the index is broken", "httpapi_test.TestPanicsInRoutes")
	})

	t.Run("a query whose result panics while it is encoded is answered with 500", func(t *testing.T) {
		var logs bytes.Buffer
		api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
		mux := http.NewServeMux()
		httpapi.Query(api, mux, "GET /notes", toListNotes, func(context.Context, listNotes) ([]explosive, error) {
			return []explosive{{}}, nil
		})

		response := askAsGolo(t, mux, "/notes")

		assertPanicAnswered(t, response, logs.String(), "GET", "GET /notes", "the encoder is broken", "httpapi_test.explosive.MarshalJSON")
	})

	t.Run("a revisioned query whose answer panics is answered with 500, without a revision", func(t *testing.T) {
		var logs bytes.Buffer
		view := noteView()
		view.Seen("4")

		api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
		mux := http.NewServeMux()
		httpapi.Query(api, mux, "GET /notes", allNotes, func(context.Context, countNotes) (int, error) {
			panic("the index is broken")
		}, httpapi.Revisioned(view, time.Second))

		response := askAsGolo(t, mux, "/notes")

		assertPanicAnswered(t, response, logs.String(), "GET", "GET /notes", "the index is broken", "httpapi_test.TestPanicsInRoutes")
		assert.Empty(t, response.Header().Get("ETag"), "a panic was tagged")
		assert.Empty(t, response.Header().Get(httpapi.HeaderRevision), "a panic carries a revision")
	})

	t.Run("a revisioned query whose result panics while it is encoded is answered with 500, without a revision", func(t *testing.T) {
		var logs bytes.Buffer
		view := noteView()
		view.Seen("4")

		api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
		mux := http.NewServeMux()
		httpapi.Query(api, mux, "GET /notes", allNotes, func(context.Context, countNotes) (explosive, error) {
			return explosive{}, nil
		}, httpapi.Revisioned(view, time.Second))

		response := askAsGolo(t, mux, "/notes")

		assertPanicAnswered(t, response, logs.String(), "GET", "GET /notes", "the encoder is broken", "httpapi_test.explosive.MarshalJSON")
		assert.Empty(t, response.Header().Get("ETag"), "a panic was tagged")
		assert.Empty(t, response.Header().Get(httpapi.HeaderRevision), "a panic carries a revision")
		assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	})

	t.Run("a revisioned query whose variance panics is answered with 500", func(t *testing.T) {
		var logs bytes.Buffer
		view := noteView()
		view.Seen("4")

		api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
		mux := http.NewServeMux()
		httpapi.Query(api, mux, "GET /notes", allNotes, countNotesIn(view),
			httpapi.Revisioned(view, time.Second),
			httpapi.Varying(func(*http.Request) string { panic("the clock is broken") }))

		response := askAsGolo(t, mux, "/notes")

		assertPanicAnswered(t, response, logs.String(), "GET", "GET /notes", "the clock is broken", "httpapi_test.TestPanicsInRoutes")
	})

	t.Run("a route passes on http.ErrAbortHandler", func(t *testing.T) {
		mux := routed(httpapi.NewAPI(writingStore(t), userFrom),
			httpapi.Adding(func(httpapi.Handled[note]) (any, error) {
				panic(http.ErrAbortHandler)
			}))

		request := httptest.NewRequest(http.MethodPost, "/note", strings.NewReader(`{"id":"1","text":"hello"}`))
		request.Header.Set("X-User", "golo")
		request.Header.Set("Content-Type", "application/json")

		assert.PanicsWithValue(t, http.ErrAbortHandler, func() {
			mux.ServeHTTP(httptest.NewRecorder(), request)
		}, "net/http expects the panic, to abort the response")
	})
}

func TestPanicsInHandleAndAsk(t *testing.T) {
	t.Run("Handle returns a panic as an internal failure, with the command it built", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)

		request := httptest.NewRequest(http.MethodPost, "/note", strings.NewReader(`{"id":"a/b"}`))
		request.Header.Set("X-User", "golo")
		request.Header.Set("Content-Type", "application/json")

		var handled httpapi.Handled[uncheckedNote]
		var err error
		require.NotPanics(t, func() {
			handled, err = httpapi.Handle(request, api, toUncheckedNote, uncheckedNoteDecider())
		})

		require.Error(t, err)
		assert.Equal(t, http.StatusInternalServerError, httpapi.StatusFor(err))
		assert.ErrorContains(t, err, `may only contain A-Z, a-z, 0-9, underscores, and hyphens`, "the error has to name the value of the panic")
		assert.Equal(t, "a/b", handled.Command.ID, "the command is handed back, as for any other failure")
	})

	t.Run("Handle returns a panic with an error as an internal failure, whatever its category", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)

		request := httptest.NewRequest(http.MethodPost, "/note", strings.NewReader(`{}`))
		request.Header.Set("X-User", "golo")
		request.Header.Set("Content-Type", "application/json")

		var err error
		require.NotPanics(t, func() {
			_, err = httpapi.Handle(request, api, toNotePanicking, noteDecider())
		})

		assert.Equal(t, http.StatusInternalServerError, httpapi.StatusFor(err), "a panic is a mistake in the code, not a transient failure")
		assert.NotErrorIs(t, err, architecturekit.ErrTransient)
	})

	t.Run("Handle passes on http.ErrAbortHandler", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)

		request := httptest.NewRequest(http.MethodPost, "/note", strings.NewReader(`{}`))
		request.Header.Set("X-User", "golo")
		request.Header.Set("Content-Type", "application/json")

		assert.PanicsWithValue(t, http.ErrAbortHandler, func() {
			_, _ = httpapi.Handle(request, api, toNoteAborting, noteDecider())
		})
	})

	t.Run("Respond logs a panic that Handle returned with the stack", func(t *testing.T) {
		var logs bytes.Buffer
		api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
		mux := http.NewServeMux()
		mux.HandleFunc("POST /note", func(w http.ResponseWriter, r *http.Request) {
			_, err := httpapi.Handle(r, api, toUncheckedNote, uncheckedNoteDecider())
			httpapi.Respond(w, r, api, nil, err)
		})

		response := postUncheckedNote(t, mux, "")

		assertPanicAnswered(t, response, logs.String(), "POST", "POST /note",
			`architecturekit: value for "id" in "/note/{id}" must not be empty`, "httpapi_test.uncheckedNote.Subject")
	})

	for name, toQuery := range map[string]httpapi.ToQuery[user, listNotes]{
		"while building the query": func(*http.Request, user) (listNotes, error) { panic("the query is broken") },
		"while answering":          toListNotes,
	} {
		t.Run("Ask returns a panic "+name+" as an internal failure", func(t *testing.T) {
			api := httpapi.NewAPI(deadStore(t), userFrom)

			request := httptest.NewRequest(http.MethodGet, "/notes", nil)
			request.Header.Set("X-User", "golo")

			var err error
			require.NotPanics(t, func() {
				_, err = httpapi.Ask(request, api, toQuery, func(context.Context, listNotes) ([]noteResponse, error) {
					panic("the answer is broken")
				})
			})

			require.Error(t, err)
			assert.Equal(t, http.StatusInternalServerError, httpapi.StatusFor(err))
			assert.ErrorContains(t, err, "is broken", "the error has to name the value of the panic")
		})
	}

	t.Run("Ask passes on http.ErrAbortHandler", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)

		request := httptest.NewRequest(http.MethodGet, "/notes", nil)
		request.Header.Set("X-User", "golo")

		assert.PanicsWithValue(t, http.ErrAbortHandler, func() {
			_, _ = httpapi.Ask(request, api, toListNotes, func(context.Context, listNotes) ([]noteResponse, error) {
				panic(http.ErrAbortHandler)
			})
		})
	})
}

// A nil API or function given to Handle or Ask, or a decider without its state
// or without its function that decides, is a mistake in the code, as it is
// for Route and Query. Handle and Ask are called on every request, so they
// report it there, before they determine the caller, and name it, rather than
// fail with a nil pointer.

func TestMissingPartsInHandleAndAsk(t *testing.T) {
	const (
		handleWithoutAPI     = "architecturekit/httpapi: Handle needs the API, not nil"
		handleWithoutCommand = "architecturekit/httpapi: Handle needs a function that turns the request into a command, not nil"
		askWithoutAPI        = "architecturekit/httpapi: Ask needs the API, not nil"
	)

	var (
		noAPI       *httpapi.API[user]
		toNoCommand httpapi.ToCommand[user, noteRequest, note]
		toNoQuery   httpapi.ToQuery[user, listNotes]
		noAnswer    httpapi.Answer[listNotes, []noteResponse]
	)

	withoutState := noteDecider()
	withoutState.State = nil

	withoutDecide := noteDecider()
	withoutDecide.Decide = nil

	// handleWithout lists the ways to give Handle, along with an API, a nil
	// function or a decider without one of its parts, with the message it
	// fails with.
	handleWithout := map[string]struct {
		toCommand httpapi.ToCommand[user, noteRequest, note]
		decider   architecturekit.Decider[note, notes]
		message   string
	}{
		"a function that turns the request into a command": {
			toNoCommand, noteDecider(), handleWithoutCommand,
		},
		"a state in the decider": {
			toNote, withoutState,
			"architecturekit/httpapi: Handle needs a decider with a state, not one whose State is nil",
		},
		"a function in the decider that decides": {
			toNote, withoutDecide,
			"architecturekit/httpapi: Handle needs a decider with a function that decides, not one whose Decide is nil",
		},
	}

	// askWithout lists the ways to give Ask a nil function, with the message
	// it fails with.
	askWithout := map[string]struct {
		toQuery httpapi.ToQuery[user, listNotes]
		answer  httpapi.Answer[listNotes, []noteResponse]
		message string
	}{
		"a function that turns the request into a query": {
			toNoQuery, answerListNotes,
			"architecturekit/httpapi: Ask needs a function that turns the request into a query, not nil",
		},
		"a function that answers the query": {
			toListNotes, noAnswer,
			"architecturekit/httpapi: Ask needs a function that answers the query, not nil",
		},
	}

	// postingNote returns a request that posts a note, from the given caller.
	postingNote := func(id string) *http.Request {
		request := httptest.NewRequest(http.MethodPost, "/note", strings.NewReader(`{"id":"1","text":"hello"}`))
		request.Header.Set("X-User", id)
		request.Header.Set("Content-Type", "application/json")

		return request
	}

	// askingForNotes returns a request that asks for the notes, from the given
	// caller.
	askingForNotes := func(id string) *http.Request {
		request := httptest.NewRequest(http.MethodGet, "/notes", nil)
		request.Header.Set("X-User", id)

		return request
	}

	for caller, id := range map[string]string{"a known caller": "golo", "an unknown caller": ""} {
		t.Run("Handle without an API returns an internal failure that names the mistake, for "+caller, func(t *testing.T) {
			var err error
			require.NotPanics(t, func() {
				_, err = httpapi.Handle(postingNote(id), noAPI, toNote, noteDecider())
			})

			require.Error(t, err)
			assert.Equal(t, http.StatusInternalServerError, httpapi.StatusFor(err))
			assert.ErrorContains(t, err, handleWithoutAPI)
		})

		for without, test := range handleWithout {
			t.Run("Handle without "+without+" returns an internal failure that names the mistake, for "+caller, func(t *testing.T) {
				api := httpapi.NewAPI(deadStore(t), userFrom)

				var err error
				require.NotPanics(t, func() {
					_, err = httpapi.Handle(postingNote(id), api, test.toCommand, test.decider)
				})

				require.Error(t, err)
				assert.Equal(t, http.StatusInternalServerError, httpapi.StatusFor(err))
				assert.ErrorContains(t, err, test.message)
			})
		}

		t.Run("Ask without an API returns an internal failure that names the mistake, for "+caller, func(t *testing.T) {
			var err error
			require.NotPanics(t, func() {
				_, err = httpapi.Ask(askingForNotes(id), noAPI, toListNotes, answerListNotes)
			})

			require.Error(t, err)
			assert.Equal(t, http.StatusInternalServerError, httpapi.StatusFor(err))
			assert.ErrorContains(t, err, askWithoutAPI)
		})

		for without, test := range askWithout {
			t.Run("Ask without "+without+" returns an internal failure that names the mistake, for "+caller, func(t *testing.T) {
				api := httpapi.NewAPI(deadStore(t), userFrom)

				var err error
				require.NotPanics(t, func() {
					_, err = httpapi.Ask(askingForNotes(id), api, test.toQuery, test.answer)
				})

				require.Error(t, err)
				assert.Equal(t, http.StatusInternalServerError, httpapi.StatusFor(err))
				assert.ErrorContains(t, err, test.message)
			})
		}
	}

	for without, test := range handleWithout {
		t.Run("Respond answers Handle without "+without+" with 500, and logs the mistake", func(t *testing.T) {
			var logs bytes.Buffer
			api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
			mux := http.NewServeMux()
			mux.HandleFunc("POST /note", func(w http.ResponseWriter, r *http.Request) {
				_, err := httpapi.Handle(r, api, test.toCommand, test.decider)
				httpapi.Respond(w, r, api, nil, err)
			})

			response := serve(t, mux, postingNote("golo"))

			assertPanicAnswered(t, response, logs.String(), "POST", "POST /note", test.message, "architecturekit/httpapi.Handle[")
			assert.NotContains(t, logs.String(), "nil pointer dereference")
		})
	}

	for without, test := range askWithout {
		t.Run("RespondResult answers Ask without "+without+" with 500, and logs the mistake", func(t *testing.T) {
			var logs bytes.Buffer
			api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
			mux := http.NewServeMux()
			mux.HandleFunc("GET /notes", func(w http.ResponseWriter, r *http.Request) {
				result, err := httpapi.Ask(r, api, test.toQuery, test.answer)
				httpapi.RespondResult(w, r, api, result, err)
			})

			response := askAsGolo(t, mux, "/notes")

			assertPanicAnswered(t, response, logs.String(), "GET", "GET /notes", test.message, "architecturekit/httpapi.Ask[")
			assert.NotContains(t, logs.String(), "nil pointer dereference")
		})
	}
}
