package httpapi_test

import (
	"bytes"
	"context"
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

// outcomeUnknown returns what Write returns if the database hangs up once it
// has read the write, as a step that writes something of its own does, such
// as one that records a session.
func outcomeUnknown(t *testing.T) error {
	t.Helper()

	_, err := architecturekit.Write(context.Background(), hangingUpStore(t),
		[]architecturekit.EventOn{{Subject: "/sessions/23", Event: noted{Text: "hello"}}},
		architecturekit.Unconditionally())
	require.ErrorIs(t, err, architecturekit.ErrOutcomeUnknown)

	return err
}

// assertAnsweredAsOutcomeUnknown asserts that an answer tells the caller that
// the request may have succeeded, so that it does not simply try again, and
// that the failure was logged with its details.
func assertAnsweredAsOutcomeUnknown(t *testing.T, response *httptest.ResponseRecorder, logs string) {
	t.Helper()

	assert.Equal(t, http.StatusInternalServerError, response.Code, "503 would invite trying again")
	assert.JSONEq(t, `{"message": "outcome unknown: the request may have succeeded"}`, response.Body.String(),
		"the write has begun, so the caller has to learn that it may have stored the events")
	assert.Equal(t, 1, strings.Count(logs, "\n"), "want exactly one entry")
	assert.Contains(t, logs, `level=ERROR msg="httpapi: internal failure"`)
	assert.Contains(t, logs, "outcome unknown: writing", "the details have to reach the log")
}

func TestOutcomeUnknownAfterTheCommand(t *testing.T) {
	t.Run("a route answers it from executing the command with 500 and the text that the request may have succeeded", func(t *testing.T) {
		var logs bytes.Buffer
		mux := routed(httpapi.NewAPI(hangingUpStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs))))

		response := postNote(t, mux, `{"id":"1","text":"hello"}`)

		assertAnsweredAsOutcomeUnknown(t, response, logs.String())
	})

	for name, options := range map[string][]httpapi.QueryOption{
		"a query":            nil,
		"a revisioned query": {httpapi.Revisioned(noteView(), time.Second)},
		"an awaiting query":  {httpapi.Awaiting(noteView(), time.Second)},
	} {
		t.Run(name+" answers it from answering the query with 500 and the text that the request may have succeeded", func(t *testing.T) {
			var logs bytes.Buffer
			api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
			failure := outcomeUnknown(t)

			mux := http.NewServeMux()
			httpapi.Query(api, mux, "QUERY /notes", toListNotes,
				func(context.Context, listNotes) ([]noteResponse, error) { return nil, failure }, options...)

			response := ask(t, mux, "/notes", "golo")

			assertAnsweredAsOutcomeUnknown(t, response, logs.String())
		})
	}

	t.Run("Handle, Ask, and UserOf return it unmarked once the request has begun, so that a handler of your own answers it as an unknown outcome", func(t *testing.T) {
		for name, returnError := range map[string]func(t *testing.T) error{
			"Handle": func(t *testing.T) error {
				api := httpapi.NewAPI(hangingUpStore(t), userFrom)
				_, err := httpapi.Handle(postingTo("/note", `{"id":"1","text":"hello"}`), api, toNote, noteDecider())

				return err
			},
			"Ask": func(t *testing.T) error {
				failure := outcomeUnknown(t)
				api := httpapi.NewAPI(deadStore(t), userFrom)
				request := httptest.NewRequest("QUERY", "/notes", nil)
				request.Header.Set("X-User", "golo")
				_, err := httpapi.Ask(request, api, toListNotes,
					func(context.Context, listNotes) ([]noteResponse, error) { return nil, failure })

				return err
			},
			// A handler of your own that has determined the user writes on its
			// own, so the write has begun.
			"UserOf": func(t *testing.T) error {
				api := httpapi.NewAPI(hangingUpStore(t), userFrom)
				request := httptest.NewRequest(http.MethodPost, "/sessions", nil)
				request.Header.Set("X-User", "golo")

				_, err := httpapi.UserOf(request, api)
				require.NoError(t, err)

				return outcomeUnknown(t)
			},
		} {
			for answerer, answer := range answerers {
				t.Run(name+" with "+answerer, func(t *testing.T) {
					err := returnError(t)
					require.ErrorIs(t, err, architecturekit.ErrOutcomeUnknown)

					var logs bytes.Buffer
					request, api := inAHandler(&logs)
					response := httptest.NewRecorder()

					answer(response, request, api, err)

					assertAnsweredAsOutcomeUnknown(t, response, logs.String())
				})
			}
		}
	})
}
