package httpapi_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/httpapi"
)

// The ID that OnEventID checks usually comes from the request, so one that is
// not a revision is the caller's mistake, and has to be answered with 400 and
// the error, rather than with 500, which the database would lead to.

// revisedNote revises a note, but only if the caller has seen its last event.
type revisedNote struct {
	ID              string
	Text            string
	ExpectedEventID string
}

func (c revisedNote) Subject() string { return "/note/" + c.ID }

func (c revisedNote) Preconditions() []architecturekit.Precondition {
	return []architecturekit.Precondition{architecturekit.OnEventID(c.Subject(), c.ExpectedEventID)}
}

type revisedNoteRequest struct {
	Text            string `json:"text"`
	ExpectedEventID string `json:"expectedEventId"`
}

// toRevisedNote takes the event ID from the body, without checking it.
func toRevisedNote(r *http.Request, request revisedNoteRequest, _ user) (revisedNote, error) {
	return revisedNote{ID: r.PathValue("id"), Text: request.Text, ExpectedEventID: request.ExpectedEventID}, nil
}

func revisedNoteDecider() architecturekit.Decider[revisedNote, notes] {
	state := architecturekit.NewState(notes{})
	state.Evolve(func(current notes, _ noted) notes {
		current.Count++
		return current
	})

	return architecturekit.NewDecider(state,
		func(_ context.Context, cmd revisedNote, _ notes) ([]architecturekit.Event, error) {
			return []architecturekit.Event{noted{Text: cmd.Text}}, nil
		})
}

// reviseNote sends a request to revise note 1, expecting the given event ID.
func reviseNote(mux *http.ServeMux, expectedEventID string) *httptest.ResponseRecorder {
	request := postingTo("/note/1/revise", `{"text":"hello","expectedEventId":"`+expectedEventID+`"}`)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	return recorder
}

func TestRouteWithAnEventIDThatIsNotARevision(t *testing.T) {
	t.Run("answers 400 and the error, without asking the database or logging it", func(t *testing.T) {
		for _, id := range []string{"abc", "", "-1", "9223372036854775808"} {
			t.Run(id, func(t *testing.T) {
				var logs bytes.Buffer
				mux := http.NewServeMux()
				// The store can not reach a database, so asking it would be
				// answered with 503.
				api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
				httpapi.Route(api, mux, "POST /note/{id}/revise", toRevisedNote, revisedNoteDecider())

				defaults := logsOf(func() {
					response := reviseNote(mux, id)

					assert.Equal(t, http.StatusBadRequest, response.Code)
					assert.JSONEq(t, messageOf(t, `not a revision: "`+id+`"`), response.Body.String())
				})

				assert.Empty(t, logs.String(), "a mistake of the caller must not be logged")
				assert.Empty(t, defaults, "nothing must go to the default logger either")
			})
		}
	})

	t.Run("lets the database check an ID that is a revision", func(t *testing.T) {
		mux := http.NewServeMux()
		httpapi.Route(httpapi.NewAPI(conflictingStore(t), userFrom), mux, "POST /note/{id}/revise", toRevisedNote,
			revisedNoteDecider())

		response := reviseNote(mux, "9223372036854775807")

		assert.Equal(t, http.StatusConflict, response.Code)
		assert.JSONEq(t, messageOf(t, "conflict: the data has changed since it was read"), response.Body.String())
	})
}
