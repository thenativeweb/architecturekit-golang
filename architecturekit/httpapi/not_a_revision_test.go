package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
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

// Read, CompareRevisions, and WaitFor refuse a value that is not a revision
// with an error that wraps ErrNotARevision. Such a value usually comes from
// the request, so an answer that hands it on unchecked is the caller's
// mistake, and has to be answered with 400 and the error, rather than with
// 500.

// pageOfNotes asks for the notes after an event, which the caller names.
type pageOfNotes struct {
	After string
}

// toPageOfNotes takes the event from the query string, without checking it.
func toPageOfNotes(r *http.Request, _ httpapi.NoBody, _ user) (pageOfNotes, error) {
	return pageOfNotes{After: r.URL.Query().Get("after")}, nil
}

// readingNotesAfter answers with the IDs of the notes after the event of the
// page, which it reads from the store.
func readingNotesAfter(store *architecturekit.Store) httpapi.Answer[pageOfNotes, []string] {
	return func(ctx context.Context, page pageOfNotes) ([]string, error) {
		ids := []string{}

		for event, err := range architecturekit.Read(ctx, store, architecturekit.SubjectTree("/note"), architecturekit.AfterEvent(page.After)) {
			if err != nil {
				return nil, err
			}
			ids = append(ids, event.ID)
		}

		return ids, nil
	}
}

// comparingWith answers with whether the view has seen the event of the
// page, which it compares with the revision of the view.
func comparingWith(view architecturekit.Revisioned) httpapi.Answer[pageOfNotes, bool] {
	return func(_ context.Context, page pageOfNotes) (bool, error) {
		reached, err := architecturekit.CompareRevisions(view.Revision(), page.After)
		if err != nil {
			return false, err
		}

		return reached >= 0, nil
	}
}

// waitingIn answers once the view has seen the event of the page.
func waitingIn(view architecturekit.Revisioned) httpapi.Answer[pageOfNotes, bool] {
	return func(ctx context.Context, page pageOfNotes) (bool, error) {
		if err := view.WaitFor(ctx, page.After); err != nil {
			return false, err
		}

		return true, nil
	}
}

// messageOf is the body of an answer that explains the error with the given
// message.
func messageOf(t *testing.T, message string) string {
	t.Helper()

	body, err := json.Marshal(map[string]string{"message": message})
	require.NoError(t, err)

	return string(body)
}

func TestAnsweringAValueThatIsNotARevision(t *testing.T) {
	const notARevision = `not a revision: "abc"`
	const notABound = `not a revision: reading "/note": AfterEvent("abc") needs the ID of an event`

	queries := map[string]struct {
		wire    func(t *testing.T, api *httpapi.API[user], mux *http.ServeMux)
		message string
	}{
		"a query answers a bound of Read that is not the ID of an event": {
			wire: func(t *testing.T, api *httpapi.API[user], mux *http.ServeMux) {
				httpapi.Query(api, mux, "QUERY /notes", toPageOfNotes, readingNotesAfter(emptyDatabase(t, false)))
			},
			message: notABound,
		},
		"a revisioned query answers a bound of Read that is not the ID of an event": {
			wire: func(t *testing.T, api *httpapi.API[user], mux *http.ServeMux) {
				httpapi.Query(api, mux, "QUERY /notes", toPageOfNotes, readingNotesAfter(emptyDatabase(t, false)),
					httpapi.Revisioned(noteView(), time.Second))
			},
			message: notABound,
		},
		"a query answers a revision to compare that is not one": {
			wire: func(t *testing.T, api *httpapi.API[user], mux *http.ServeMux) {
				httpapi.Query(api, mux, "QUERY /notes", toPageOfNotes, comparingWith(noteView()))
			},
			message: notARevision,
		},
		"a query answers a revision to wait for that is not one": {
			wire: func(t *testing.T, api *httpapi.API[user], mux *http.ServeMux) {
				httpapi.Query(api, mux, "QUERY /notes", toPageOfNotes, waitingIn(noteView()))
			},
			message: notARevision,
		},
	}

	for name, asked := range queries {
		t.Run(name+" with 400 and the error, without logging it", func(t *testing.T) {
			var logs bytes.Buffer
			api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
			mux := http.NewServeMux()
			asked.wire(t, api, mux)

			defaults := logsOf(func() {
				response := ask(t, mux, "/notes?after=abc", "golo")

				assert.Equal(t, http.StatusBadRequest, response.Code)
				assert.JSONEq(t, messageOf(t, asked.message), response.Body.String())
			})

			assert.Empty(t, logs.String(), "a mistake of the caller must not be logged")
			assert.Empty(t, defaults, "nothing must go to the default logger either")
		})
	}

	t.Run("a query answers a bound of Read that is the ID of an event", func(t *testing.T) {
		mux := http.NewServeMux()
		httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom), mux, "QUERY /notes", toPageOfNotes, readingNotesAfter(emptyDatabase(t, false)))

		response := ask(t, mux, "/notes?after=0", "golo")

		assert.Equal(t, http.StatusOK, response.Code)
		assert.JSONEq(t, `[]`, response.Body.String())
	})

	t.Run("Ask returns the error, which StatusFor maps to 400", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/notes?after=abc", nil)
		request.Header.Set("X-User", "golo")

		_, err := httpapi.Ask(request, httpapi.NewAPI(deadStore(t), userFrom), toPageOfNotes, readingNotesAfter(emptyDatabase(t, false)))

		require.ErrorIs(t, err, architecturekit.ErrNotARevision)
		assert.Equal(t, http.StatusBadRequest, httpapi.StatusFor(err))
	})

	t.Run("Respond, RespondResult, and RespondError answer it with 400 and the error, without logging it", func(t *testing.T) {
		_, refused := architecturekit.CompareRevisions("abc", "")
		require.ErrorIs(t, refused, architecturekit.ErrNotARevision)

		responders := map[string]func(w http.ResponseWriter, r *http.Request, api *httpapi.API[user]){
			"Respond": func(w http.ResponseWriter, r *http.Request, api *httpapi.API[user]) {
				httpapi.Respond(w, r, api, nil, refused)
			},
			"RespondResult": func(w http.ResponseWriter, r *http.Request, api *httpapi.API[user]) {
				httpapi.RespondResult(w, r, api, []noteResponse(nil), refused)
			},
			"RespondError": func(w http.ResponseWriter, r *http.Request, api *httpapi.API[user]) {
				httpapi.RespondError(w, r, api, refused)
			},
		}

		for name, respond := range responders {
			t.Run(name, func(t *testing.T) {
				var logs bytes.Buffer
				request, api := inAHandler(&logs)
				recorder := httptest.NewRecorder()

				respond(recorder, request, api)

				assert.Equal(t, http.StatusBadRequest, recorder.Code)
				assert.JSONEq(t, messageOf(t, notARevision), recorder.Body.String())
				assert.Empty(t, logs.String(), "a mistake of the caller must not be logged")
			})
		}
	})

	t.Run("a query answers it with 500 if it is a permanent failure as well, and logs it", func(t *testing.T) {
		// An ID that the server stored or made itself, such as one in a view,
		// is no mistake of the caller.
		var logs bytes.Buffer
		api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
		mux := http.NewServeMux()
		httpapi.Query(api, mux, "QUERY /notes", toPageOfNotes, func(context.Context, pageOfNotes) ([]string, error) {
			_, err := architecturekit.CompareRevisions("broken", "0")
			return nil, fmt.Errorf("%w: the view holds %w", architecturekit.ErrPermanent, err)
		})

		response := ask(t, mux, "/notes", "golo")

		assert.Equal(t, http.StatusInternalServerError, response.Code)
		assert.JSONEq(t, `{"message": "internal server error"}`, response.Body.String())
		assert.Equal(t, 1, strings.Count(logs.String(), "httpapi: internal failure"))
		assert.Contains(t, logs.String(), "the view holds not a revision")
	})
}
