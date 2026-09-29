package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/httpapi"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/query"
)

// The read side touches no store, so none of these tests needs a database.

type listNotes struct {
	Limit int
}

type noteResponse struct {
	Text string `json:"text"`
}

func toListNotes(r *http.Request, _ user) (listNotes, error) {
	ask := listNotes{}

	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			return listNotes{}, errors.New("limit must be a number")
		}
		ask.Limit = limit
	}

	return ask, nil
}

func answerListNotes(ctx context.Context, ask listNotes) ([]noteResponse, error) {
	notes := []noteResponse{{Text: "first"}, {Text: "second"}, {Text: "third"}}

	if ask.Limit > 0 && ask.Limit < len(notes) {
		notes = notes[:ask.Limit]
	}

	return notes, nil
}

func queryMux(t *testing.T) *http.ServeMux {
	t.Helper()

	api := httpapi.NewAPI(deadStore(t), userFrom)
	mux := http.NewServeMux()
	httpapi.Query(api, mux, "GET /notes", toListNotes, answerListNotes)

	return mux
}

func ask(t *testing.T, mux *http.ServeMux, path, asUser string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, path, nil)
	if asUser != "" {
		request.Header.Set("X-User", asUser)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	return recorder
}

func TestQuery(t *testing.T) {
	t.Run("answers with the result", func(t *testing.T) {
		response := ask(t, queryMux(t), "/notes", "golo")

		assert.Equal(t, http.StatusOK, response.Code)
		assert.Equal(t, "application/json", response.Header().Get("Content-Type"))

		var notes []noteResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &notes))
		assert.Len(t, notes, 3)
	})

	t.Run("passes request parameters through", func(t *testing.T) {
		response := ask(t, queryMux(t), "/notes?limit=2", "golo")

		var notes []noteResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &notes))
		assert.Len(t, notes, 2)
	})

	t.Run("needs a user", func(t *testing.T) {
		response := ask(t, queryMux(t), "/notes", "")

		assert.Equal(t, http.StatusUnauthorized, response.Code)
	})

	t.Run("rejects unusable parameters", func(t *testing.T) {
		response := ask(t, queryMux(t), "/notes?limit=banana", "golo")

		assert.Equal(t, http.StatusBadRequest, response.Code)
		assert.Contains(t, response.Body.String(), "limit must be a number")
	})

	t.Run("without items is not found", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)
		mux := http.NewServeMux()

		// A single-item lookup that finds nothing reports query.ErrNoItems, which
		// the HTTP layer turns into 404 without the application saying so.
		httpapi.Query(api, mux, "GET /notes/{id}", toListNotes,
			func(context.Context, listNotes) (noteResponse, error) {
				return noteResponse{}, query.ErrNoItems
			})

		response := ask(t, mux, "/notes/7", "golo")

		assert.Equal(t, http.StatusNotFound, response.Code)
	})

	t.Run("ErrForbidden survives the way out", func(t *testing.T) {
		// An application that refuses in ToQuery keeps its 403 instead of having
		// it turned into a 400.
		api := httpapi.NewAPI(deadStore(t), userFrom)
		mux := http.NewServeMux()

		httpapi.Query(api, mux, "GET /restricted",
			func(*http.Request, user) (listNotes, error) {
				return listNotes{}, errors.Join(httpapi.ErrForbidden, errors.New("not for you"))
			},
			answerListNotes)

		response := ask(t, mux, "/restricted", "golo")

		assert.Equal(t, http.StatusForbidden, response.Code)
	})

	t.Run("domain errors from the query side keep their status", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)
		mux := http.NewServeMux()

		httpapi.Query(api, mux, "GET /rule",
			func(*http.Request, user) (listNotes, error) {
				return listNotes{}, architecturekit.NewDomainError("that combination makes no sense")
			},
			answerListNotes)

		response := ask(t, mux, "/rule", "golo")

		assert.Equal(t, http.StatusUnprocessableEntity, response.Code)
	})
}

func TestAsk(t *testing.T) {
	t.Run("returns the result without writing", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)

		request := httptest.NewRequest(http.MethodGet, "/notes", nil)
		request.Header.Set("X-User", "golo")

		notes, err := httpapi.Ask(request, api, toListNotes, answerListNotes)
		require.NoError(t, err)
		assert.Len(t, notes, 3)
	})

	t.Run("reports a failing answer", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)

		request := httptest.NewRequest(http.MethodGet, "/notes", nil)
		request.Header.Set("X-User", "golo")

		_, err := httpapi.Ask(request, api, toListNotes,
			func(context.Context, listNotes) ([]noteResponse, error) {
				return nil, errors.New("the view is unavailable")
			})

		assert.Error(t, err, "expected the error from the answer")
	})
}

func TestPublicAPI(t *testing.T) {
	t.Run("serves everyone", func(t *testing.T) {
		api := httpapi.NewPublicAPI(deadStore(t))
		mux := http.NewServeMux()

		httpapi.Query(api, mux, "GET /public",
			func(r *http.Request, _ httpapi.NoUser) (listNotes, error) {
				return listNotes{}, nil
			},
			answerListNotes)

		// No user header at all, and it is served anyway.
		response := ask(t, mux, "/public", "")

		assert.Equal(t, http.StatusOK, response.Code)
	})
}

func TestRespondResult(t *testing.T) {
	t.Run("writes the result", func(t *testing.T) {
		recorder := httptest.NewRecorder()

		httpapi.RespondResult(recorder, []noteResponse{{Text: "only"}}, nil)

		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.Contains(t, recorder.Body.String(), "only")
	})

	t.Run("answers an empty result with an empty list", func(t *testing.T) {
		recorder := httptest.NewRecorder()

		// slices.Collect, which the kit suggests for turning items into a slice,
		// returns nil when there are no items.
		httpapi.RespondResult(recorder, slices.Collect(slices.Values([]noteResponse{})), nil)

		assert.Equal(t, "[]", strings.TrimSpace(recorder.Body.String()))
	})

	t.Run("explains failures the caller can fix", func(t *testing.T) {
		recorder := httptest.NewRecorder()

		httpapi.RespondResult(recorder, []noteResponse(nil),
			errors.Join(httpapi.ErrNotFound, errors.New("note 7 is unknown")))

		assert.Equal(t, http.StatusNotFound, recorder.Code)
		assert.Contains(t, recorder.Body.String(), "note 7 is unknown")
	})

	t.Run("keeps internal failures to itself", func(t *testing.T) {
		recorder := httptest.NewRecorder()

		httpapi.RespondResult(recorder, []noteResponse(nil), errors.New("the password is hunter2"))

		assert.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.NotContains(t, recorder.Body.String(), "hunter2", "an internal failure must not be explained")
	})

	t.Run("logs internal failures", func(t *testing.T) {
		logs := logsOf(func() {
			httpapi.RespondResult(httptest.NewRecorder(), []noteResponse(nil), errors.New("the view is gone"))
		})

		assert.Contains(t, logs, "the view is gone", "an internal failure must be logged")
	})

	t.Run("does not log failures the caller can fix", func(t *testing.T) {
		logs := logsOf(func() {
			httpapi.RespondResult(httptest.NewRecorder(), []noteResponse(nil), httpapi.ErrNotFound)
		})

		assert.Empty(t, logs, "a failure the caller can fix must not be logged")
	})
}
