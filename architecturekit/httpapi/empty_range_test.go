package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/httpapi"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/query"
)

// Read refuses bounds that leave no room for an event with an error that
// wraps ErrEmptyRange. Such bounds usually come from the request, so an
// answer that hands them on is the caller's mistake, and has to be answered
// with 400 and the error, as a value that is not a revision is.

// rangeOfNotes asks for the notes between two events, which the caller names.
type rangeOfNotes struct {
	After  string
	Before string
}

// toRangeOfNotes takes the events from the query string, without checking
// them.
func toRangeOfNotes(r *http.Request, _ httpapi.NoBody, _ user) (rangeOfNotes, error) {
	return rangeOfNotes{After: r.URL.Query().Get("after"), Before: r.URL.Query().Get("before")}, nil
}

// readingNotesBetween answers with the IDs of the notes between the events of
// the range, which it reads from the store.
func readingNotesBetween(store *architecturekit.Store) httpapi.Answer[rangeOfNotes, []string] {
	return func(ctx context.Context, between rangeOfNotes) ([]string, error) {
		ids := []string{}

		options := []architecturekit.ReadOption{architecturekit.BeforeEvent(between.Before)}
		if between.After != "" {
			options = append(options, architecturekit.AfterEvent(between.After))
		}

		for event, err := range architecturekit.Read(ctx, store, architecturekit.SubjectTree("/note"), options...) {
			if err != nil {
				return nil, err
			}
			ids = append(ids, event.ID)
		}

		return ids, nil
	}
}

func TestStatusForAnEmptyRange(t *testing.T) {
	emptyRange := fmt.Errorf("%w: no event can lie before %q", architecturekit.ErrEmptyRange, "0")

	t.Run("maps it to 400, unless it is a permanent failure", func(t *testing.T) {
		cases := []struct {
			label string
			err   error
			want  int
		}{
			{"alone", architecturekit.ErrEmptyRange, http.StatusBadRequest},
			{"as Read returns it", emptyRange, http.StatusBadRequest},
			{"wrapped", fmt.Errorf("listing the notes: %w", emptyRange), http.StatusBadRequest},
			{"joined with a permanent failure", errors.Join(architecturekit.ErrPermanent, emptyRange), http.StatusInternalServerError},
			{"joined with a permanent failure, the other way round", errors.Join(emptyRange, architecturekit.ErrPermanent), http.StatusInternalServerError},
			{"wrapped as a permanent failure", fmt.Errorf("%w: %w", architecturekit.ErrPermanent, emptyRange), http.StatusInternalServerError},
			{"wrapping a permanent failure", fmt.Errorf("%w: %w", emptyRange, architecturekit.ErrPermanent), http.StatusInternalServerError},
			{"joined with an event that could not be verified", errors.Join(architecturekit.ErrUnverified, emptyRange), http.StatusInternalServerError},
		}

		for _, c := range cases {
			t.Run(c.label, func(t *testing.T) {
				assert.Equal(t, c.want, httpapi.StatusFor(c.err))
			})
		}
	})

	t.Run("keeps the status of every other category", func(t *testing.T) {
		categories := []struct {
			label string
			err   error
			want  int
		}{
			{"unauthorized", httpapi.ErrUnauthorized, http.StatusUnauthorized},
			{"forbidden", httpapi.ErrForbidden, http.StatusForbidden},
			{"too large", httpapi.ErrTooLarge, http.StatusRequestEntityTooLarge},
			{"wrong media type", httpapi.ErrUnsupportedMediaType, http.StatusUnsupportedMediaType},
			{"malformed", httpapi.ErrMalformed, http.StatusBadRequest},
			{"not found", httpapi.ErrNotFound, http.StatusNotFound},
			{"no items", query.ErrNoItems, http.StatusNotFound},
			{"domain rule", architecturekit.NewDomainError("nope"), http.StatusUnprocessableEntity},
			{"conflict", architecturekit.ErrConflict, http.StatusConflict},
			{"transient", architecturekit.ErrTransient, http.StatusServiceUnavailable},
			{"a caller who went away", context.Canceled, 499},
			{"a deadline that ran out", context.DeadlineExceeded, http.StatusServiceUnavailable},
		}

		for _, category := range categories {
			t.Run(category.label, func(t *testing.T) {
				assert.Equal(t, category.want, httpapi.StatusFor(errors.Join(category.err, emptyRange)))
				assert.Equal(t, category.want, httpapi.StatusFor(errors.Join(emptyRange, category.err)))
				assert.Equal(t, category.want, httpapi.StatusFor(fmt.Errorf("%w: %w", category.err, emptyRange)))
			})
		}
	})
}

func TestAnsweringAnEmptyRange(t *testing.T) {
	for path, message := range map[string]string{
		"/notes?before=0":         `empty range: no event can lie before "0"`,
		"/notes?after=0&before=1": `empty range: no event can lie after "0" and before "1"`,
		"/notes?after=2&before=1": `empty range: no event can lie after "2" and before "1"`,
	} {
		t.Run("a query answers "+path+" with 400 and the error, without asking the database or logging it", func(t *testing.T) {
			var logs bytes.Buffer
			api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
			mux := http.NewServeMux()
			// The store can not reach a database, so asking it would be answered
			// with 503.
			httpapi.Query(api, mux, "QUERY /notes", toRangeOfNotes, readingNotesBetween(deadStore(t)))

			defaults := logsOf(func() {
				response := ask(t, mux, path, "golo")

				assert.Equal(t, http.StatusBadRequest, response.Code)
				assert.JSONEq(t, messageOf(t, message), response.Body.String())
			})

			assert.Empty(t, logs.String(), "a mistake of the caller must not be logged")
			assert.Empty(t, defaults, "nothing must go to the default logger either")
		})
	}

	t.Run("a query answers a range that is only empty for now", func(t *testing.T) {
		mux := http.NewServeMux()
		httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom), mux, "QUERY /notes", toRangeOfNotes,
			readingNotesBetween(emptyDatabase(t, false)))

		response := ask(t, mux, "/notes?after=0&before=2", "golo")

		assert.Equal(t, http.StatusOK, response.Code)
		assert.JSONEq(t, `[]`, response.Body.String())
	})

	t.Run("Ask returns the error, which StatusFor maps to 400", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/notes?before=0", nil)
		request.Header.Set("X-User", "golo")

		_, err := httpapi.Ask(request, httpapi.NewAPI(deadStore(t), userFrom), toRangeOfNotes, readingNotesBetween(deadStore(t)))

		require.ErrorIs(t, err, architecturekit.ErrEmptyRange)
		assert.Equal(t, http.StatusBadRequest, httpapi.StatusFor(err))
	})

	t.Run("a query keeps the error of the function that returns the query as it is", func(t *testing.T) {
		// It has a status of its own, so it is not wrapped with ErrMalformed,
		// which would put "malformed request" in front of it.
		mux := http.NewServeMux()
		httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom), mux, "QUERY /notes",
			func(*http.Request, httpapi.NoBody, user) (rangeOfNotes, error) {
				return rangeOfNotes{}, fmt.Errorf("%w: no event can lie before %q", architecturekit.ErrEmptyRange, "0")
			},
			readingNotesBetween(deadStore(t)))

		response := ask(t, mux, "/notes", "golo")

		assert.Equal(t, http.StatusBadRequest, response.Code)
		assert.JSONEq(t, messageOf(t, `empty range: no event can lie before "0"`), response.Body.String())
	})

	t.Run("Respond, RespondResult, and RespondError answer it with 400 and the error, without logging it", func(t *testing.T) {
		refused := fmt.Errorf("%w: no event can lie before %q", architecturekit.ErrEmptyRange, "0")

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
				assert.JSONEq(t, messageOf(t, `empty range: no event can lie before "0"`), recorder.Body.String())
				assert.Empty(t, logs.String(), "a mistake of the caller must not be logged")
			})
		}
	})

	t.Run("a query answers it with 500 if it is a permanent failure as well, and logs it", func(t *testing.T) {
		var logs bytes.Buffer
		api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
		mux := http.NewServeMux()
		httpapi.Query(api, mux, "QUERY /notes", toRangeOfNotes, func(context.Context, rangeOfNotes) ([]string, error) {
			return nil, fmt.Errorf("%w: the view holds %w", architecturekit.ErrPermanent, architecturekit.ErrEmptyRange)
		})

		response := ask(t, mux, "/notes", "golo")

		assert.Equal(t, http.StatusInternalServerError, response.Code)
		assert.JSONEq(t, `{"message": "internal server error"}`, response.Body.String())
		assert.Equal(t, 1, strings.Count(logs.String(), "httpapi: internal failure"))
		assert.Contains(t, logs.String(), "the view holds empty range")
	})
}
