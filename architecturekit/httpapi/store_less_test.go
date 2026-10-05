package httpapi_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/httpapi"
)

func TestAPIWithoutStore(t *testing.T) {
	t.Run("answers queries", func(t *testing.T) {
		api := httpapi.NewAPI(nil, userFrom)
		mux := http.NewServeMux()
		httpapi.Query(api, mux, "QUERY /notes",
			func(*http.Request, httpapi.NoBody, user) (string, error) { return "all", nil },
			func(context.Context, string) ([]string, error) { return []string{"first", "second"}, nil },
		)

		request := httptest.NewRequest("QUERY", "/notes", nil)
		request.Header.Set("X-User", "golo")
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)

		require.Equal(t, http.StatusOK, response.Code)
		assert.JSONEq(t, `["first","second"]`, response.Body.String())
	})

	t.Run("answers a command with 500 and says why, instead of panicking", func(t *testing.T) {
		var logs bytes.Buffer
		api := httpapi.NewAPI(nil, userFrom, httpapi.WithLogger(loggerInto(&logs)))
		mux := http.NewServeMux()
		httpapi.Route(api, mux, "POST /note", toNote, noteDecider())

		var response *httptest.ResponseRecorder
		require.NotPanics(t, func() {
			response = postNote(t, mux, `{"id":"1","text":"hello"}`)
		})

		assert.Equal(t, http.StatusInternalServerError, response.Code)
		assert.Contains(t, logs.String(), "the API has no store", "the log has to say what is wrong")
		assert.Contains(t, logs.String(), `route="POST /note"`)
		assert.NotContains(t, response.Body.String(), "store", "the caller is not told about the wiring")
	})
}
