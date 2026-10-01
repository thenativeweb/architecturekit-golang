package httpapi_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/httpapi"
)

type previewRequest struct {
	CustomerID string `json:"customerId"`
	Quantity   int    `json:"quantity"`
}

func bodyRequest(contentType string, body io.Reader) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/preview", body)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	return request
}

func TestBodyOf(t *testing.T) {
	t.Run("decodes a body that fits", func(t *testing.T) {
		request := bodyRequest("application/json; charset=utf-8",
			strings.NewReader(`{"customerId":"42","quantity":3}`))

		preview, err := httpapi.BodyOf[previewRequest](request)
		require.NoError(t, err)
		assert.Equal(t, previewRequest{CustomerID: "42", Quantity: 3}, preview)
	})

	t.Run("rejects a body that does not claim to be JSON", func(t *testing.T) {
		for _, contentType := range []string{
			"",
			"text/plain",
			"text/plain; application/json",
			"not a media type at all;;;",
		} {
			t.Run(contentType, func(t *testing.T) {
				request := bodyRequest(contentType, strings.NewReader(`{"customerId":"42"}`))

				_, err := httpapi.BodyOf[previewRequest](request)
				assert.ErrorIs(t, err, httpapi.ErrUnsupportedMediaType)
			})
		}
	})

	t.Run("rejects a body over the limit", func(t *testing.T) {
		padding := strings.Repeat("a", httpapi.MaxRequestBody)
		request := bodyRequest("application/json", strings.NewReader(`{"customerId":"`+padding+`"}`))

		_, err := httpapi.BodyOf[previewRequest](request)
		assert.ErrorIs(t, err, httpapi.ErrTooLarge)
	})

	t.Run("reads a body at the limit", func(t *testing.T) {
		prefix := `{"customerId":"`
		suffix := `"}`
		padding := strings.Repeat("a", httpapi.MaxRequestBody-len(prefix)-len(suffix))
		request := bodyRequest("application/json", strings.NewReader(prefix+padding+suffix))

		preview, err := httpapi.BodyOf[previewRequest](request)
		require.NoError(t, err)
		assert.Equal(t, padding, preview.CustomerID)
	})

	t.Run("rejects JSON that does not fit", func(t *testing.T) {
		for _, test := range []struct {
			label string
			body  string
			names string
		}{
			{label: "malformed", body: `not json`, names: "invalid character"},
			{label: "an unknown field", body: `{"customerId":"42","quantiy":3}`, names: "quantiy"},
			{label: "a value of the wrong type", body: `{"quantity":"three"}`, names: "quantity"},
		} {
			t.Run(test.label, func(t *testing.T) {
				request := bodyRequest("application/json", strings.NewReader(test.body))

				preview, err := httpapi.BodyOf[previewRequest](request)
				require.ErrorIs(t, err, httpapi.ErrMalformed)
				assert.ErrorContains(t, err, test.names, "the error has to say what is wrong")
				assert.Zero(t, preview, "nothing half-decoded may be handed back")
			})
		}
	})

	t.Run("rejects a body that can not be read", func(t *testing.T) {
		_, err := httpapi.BodyOf[previewRequest](bodyRequest("application/json", failingReader{}))
		assert.ErrorIs(t, err, httpapi.ErrMalformed)
	})

	t.Run("answers with the statuses of a command", func(t *testing.T) {
		api := httpapi.NewPublicAPI(deadStore(t))
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			preview, err := httpapi.BodyOf[previewRequest](r)
			httpapi.RespondResult(w, r, api, preview, err)
		})

		for _, test := range []struct {
			label       string
			contentType string
			body        string
			status      int
		}{
			{label: "fits", contentType: "application/json", body: `{"customerId":"42"}`, status: http.StatusOK},
			{label: "not JSON", contentType: "text/plain", body: `{"customerId":"42"}`, status: http.StatusUnsupportedMediaType},
			{label: "too large", contentType: "application/json", body: strings.Repeat(" ", httpapi.MaxRequestBody+1), status: http.StatusRequestEntityTooLarge},
			{label: "an unknown field", contentType: "application/json", body: `{"name":"42"}`, status: http.StatusBadRequest},
		} {
			t.Run(test.label, func(t *testing.T) {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, bodyRequest(test.contentType, strings.NewReader(test.body)))

				assert.Equal(t, test.status, response.Code)
			})
		}
	})
}
