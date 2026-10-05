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

// toClosedNote takes everything it needs from the path, so it takes no body.
func toClosedNote(r *http.Request, _ httpapi.NoBody, _ user) (note, error) {
	return note{ID: r.PathValue("id"), Text: "closed"}, nil
}

// closing returns a request of a known user that closes the note 42, with the
// given Content-Type, if any, and body.
func closing(contentType string, body io.Reader) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/notes/42/close", body)
	request.Header.Set("X-User", "golo")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	return request
}

func TestNoBody(t *testing.T) {
	t.Run("a route executes a command without a Content-Type and a body", func(t *testing.T) {
		mux := routedShowing(t, "POST /notes/{id}/close", toClosedNote)

		response := serve(t, mux, closing("", http.NoBody))

		assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.JSONEq(t, `{"revision": "0", "id": "42", "text": "closed"}`, response.Body.String())
	})

	t.Run("a route executes a command with an empty body, whatever the Content-Type says", func(t *testing.T) {
		for _, contentType := range []string{
			"application/json",
			// What curl sends with -d ''.
			"application/x-www-form-urlencoded",
			"not a media type at all;;;",
		} {
			t.Run(contentType, func(t *testing.T) {
				mux := routedShowing(t, "POST /notes/{id}/close", toClosedNote)

				response := serve(t, mux, closing(contentType, strings.NewReader("")))

				assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
				assert.JSONEq(t, `{"revision": "0", "id": "42", "text": "closed"}`, response.Body.String())
			})
		}
	})

	t.Run("a route executes a command with {}, whatever the Content-Type says", func(t *testing.T) {
		// Callers that send {} out of habit keep working.
		for _, test := range []struct {
			label       string
			contentType string
			body        string
		}{
			{label: "with JSON", contentType: "application/json", body: `{}`},
			{label: "without a Content-Type", contentType: "", body: `{}`},
			{label: "with plain text", contentType: "text/plain", body: `{}`},
			{label: "with whitespace", contentType: "application/json", body: " { \t}\r\n"},
		} {
			t.Run(test.label, func(t *testing.T) {
				mux := routedShowing(t, "POST /notes/{id}/close", toClosedNote)

				response := serve(t, mux, closing(test.contentType, strings.NewReader(test.body)))

				assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
				assert.JSONEq(t, `{"revision": "0", "id": "42", "text": "closed"}`, response.Body.String())
			})
		}
	})

	t.Run("a route refuses any other body, and says that it takes none", func(t *testing.T) {
		for _, test := range []struct {
			label       string
			contentType string
			body        string
		}{
			{label: "an object with a field", contentType: "application/json", body: `{"text":"hello"}`},
			{label: "an object with a field, without a Content-Type", contentType: "", body: `{"text":"hello"}`},
			{label: "null", contentType: "application/json", body: `null`},
			{label: "an empty list", contentType: "application/json", body: `[]`},
			{label: "a string", contentType: "application/json", body: `"hello"`},
			{label: "a single digit", contentType: "application/json", body: `0`},
			{label: "a second value", contentType: "application/json", body: `{} {}`},
			{label: "garbage", contentType: "application/json", body: `not json`},
			{label: "garbage without a Content-Type", contentType: "", body: `not json`},
			{label: "a form", contentType: "application/x-www-form-urlencoded", body: `text=hello`},
			{label: "nothing but whitespace", contentType: "", body: " \n"},
			{label: "a single space", contentType: "", body: " "},
		} {
			t.Run(test.label, func(t *testing.T) {
				mux := routedShowing(t, "POST /notes/{id}/close", toClosedNote)

				response := serve(t, mux, closing(test.contentType, strings.NewReader(test.body)))

				assert.Equal(t, http.StatusBadRequest, response.Code)
				assert.JSONEq(t,
					`{"message": "malformed request: this route takes no body, so the body has to be empty, or {}"}`,
					response.Body.String())
			})
		}
	})

	t.Run("a route refuses a body over the limit", func(t *testing.T) {
		mux := routedShowing(t, "POST /notes/{id}/close", toClosedNote)

		response := serve(t, mux, closing("", strings.NewReader(strings.Repeat(" ", httpapi.MaxRequestBody+1))))

		assert.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
	})

	t.Run("other request types still require JSON and a body", func(t *testing.T) {
		// An empty struct is not NoBody, so it is read by the strict rules, as
		// is every other type.
		toEmptyNote := func(r *http.Request, _ struct{}, _ user) (note, error) {
			return note{ID: r.PathValue("id")}, nil
		}
		toTextNote := func(r *http.Request, request textRequest, _ user) (note, error) {
			return note{ID: r.PathValue("id"), Text: request.Text}, nil
		}

		for name, mux := range map[string]*http.ServeMux{
			"an empty struct": routedShowing(t, "POST /notes/{id}/close", toEmptyNote),
			"a struct":        routedShowing(t, "POST /notes/{id}/close", toTextNote),
		} {
			t.Run(name+" is refused without a Content-Type", func(t *testing.T) {
				response := serve(t, mux, closing("", strings.NewReader(`{}`)))

				assert.Equal(t, http.StatusUnsupportedMediaType, response.Code)
			})

			t.Run(name+" is refused with an empty body", func(t *testing.T) {
				response := serve(t, mux, closing("application/json", strings.NewReader("")))

				assert.Equal(t, http.StatusBadRequest, response.Code)
				assert.Contains(t, response.Body.String(), "empty body")
			})
		}
	})

	t.Run("Handle executes a command without a body", func(t *testing.T) {
		api := httpapi.NewAPI(writingStore(t), userFrom)
		request := closing("", http.NoBody)
		request.SetPathValue("id", "42")

		handled, err := httpapi.Handle(request, api, toClosedNote, noteDecider())

		require.NoError(t, err)
		assert.Equal(t, note{ID: "42", Text: "closed"}, handled.Command)
		require.Len(t, handled.Events, 1)
		assert.Equal(t, "/note/42", handled.Events[0].Subject)
	})

	t.Run("Handle refuses a body before it builds a command", func(t *testing.T) {
		api := httpapi.NewAPI(writingStore(t), userFrom)
		request := closing("application/json", strings.NewReader(`{"text":"hello"}`))
		request.SetPathValue("id", "42")

		built := false
		handled, err := httpapi.Handle(request, api, func(r *http.Request, body httpapi.NoBody, u user) (note, error) {
			built = true
			return toClosedNote(r, body, u)
		}, noteDecider())

		require.ErrorIs(t, err, httpapi.ErrMalformed)
		assert.ErrorContains(t, err, "this route takes no body")
		assert.False(t, built, "the command must not be built from a request with a body")
		assert.Empty(t, handled.Events)
	})

	t.Run("BodyOf accepts no body, an empty one, and {}", func(t *testing.T) {
		withoutBody, err := http.NewRequest(http.MethodPost, "/notes/42/close", nil)
		require.NoError(t, err)
		require.Nil(t, withoutBody.Body, "http.NewRequest is expected to leave the body nil")

		for label, request := range map[string]*http.Request{
			"a request made without a body": withoutBody,
			"an empty body":                 bodyRequest("", http.NoBody),
			"{}":                            bodyRequest("application/json", strings.NewReader(`{}`)),
		} {
			t.Run(label, func(t *testing.T) {
				value, err := httpapi.BodyOf[httpapi.NoBody](request)

				require.NoError(t, err)
				assert.Equal(t, httpapi.NoBody{}, value)
			})
		}
	})

	t.Run("BodyOf refuses any other body", func(t *testing.T) {
		_, err := httpapi.BodyOf[httpapi.NoBody](bodyRequest("", strings.NewReader(`{"text":"hello"}`)))

		require.ErrorIs(t, err, httpapi.ErrMalformed)
		assert.EqualError(t, err, "malformed request: this route takes no body, so the body has to be empty, or {}")
	})

	t.Run("BodyOf reads no more than the limit", func(t *testing.T) {
		_, err := httpapi.BodyOf[httpapi.NoBody](bodyRequest("", strings.NewReader("{"+strings.Repeat(" ", httpapi.MaxRequestBody)+"}")))

		assert.ErrorIs(t, err, httpapi.ErrTooLarge)
	})

	t.Run("BodyOf reads {} at the limit", func(t *testing.T) {
		padding := strings.Repeat(" ", httpapi.MaxRequestBody-len("{}"))

		_, err := httpapi.BodyOf[httpapi.NoBody](bodyRequest("", strings.NewReader("{"+padding+"}")))

		assert.NoError(t, err)
	})

	t.Run("BodyOf refuses a body that can not be read", func(t *testing.T) {
		_, err := httpapi.BodyOf[httpapi.NoBody](bodyRequest("", failingReader{}))

		assert.ErrorIs(t, err, httpapi.ErrMalformed)
		assert.ErrorIs(t, err, errBrokenBody, "the error of reading has to stay inspectable")
	})

	t.Run("BodyOf answers with the statuses of a command", func(t *testing.T) {
		api := httpapi.NewPublicAPI(deadStore(t))
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			value, err := httpapi.BodyOf[httpapi.NoBody](r)
			httpapi.RespondResult(w, r, api, value, err)
		})

		for _, test := range []struct {
			label  string
			body   string
			status int
		}{
			{label: "no body", body: ``, status: http.StatusOK},
			{label: "{}", body: `{}`, status: http.StatusOK},
			{label: "a body", body: `{"text":"hello"}`, status: http.StatusBadRequest},
			{label: "too large", body: strings.Repeat(" ", httpapi.MaxRequestBody+1), status: http.StatusRequestEntityTooLarge},
		} {
			t.Run(test.label, func(t *testing.T) {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, bodyRequest("", strings.NewReader(test.body)))

				assert.Equal(t, test.status, response.Code)
			})
		}
	})
}

// fromOrigin sets the headers by which a browser tells where a request comes
// from, leaving out a header whose value is empty. The request goes to the
// host example.com, as httptest.NewRequest makes it.
func fromOrigin(request *http.Request, secFetchSite, origin string) *http.Request {
	if secFetchSite != "" {
		request.Header.Set("Sec-Fetch-Site", secFetchSite)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}

	return request
}

// crossOrigins are requests that a browser sends from another origin.
var crossOrigins = []struct {
	label, secFetchSite, origin string
}{
	{label: "another site", secFetchSite: "cross-site", origin: "https://elsewhere.example"},
	{label: "another subdomain of the same site", secFetchSite: "same-site", origin: "https://admin.example.com"},
	{label: "an old browser on another host", secFetchSite: "", origin: "https://elsewhere.example"},
}

// sameOrigins are requests from the same origin, or not from a browser.
var sameOrigins = []struct {
	label, secFetchSite, origin string
}{
	{label: "the same origin", secFetchSite: "same-origin", origin: "http://example.com"},
	{label: "an address the user typed in", secFetchSite: "none", origin: ""},
	{label: "no such headers, as from curl", secFetchSite: "", origin: ""},
	{label: "an old browser on the same host", secFetchSite: "", origin: "http://example.com"},
}

const fromAnotherOrigin = "forbidden: a command without a body is not accepted from another origin"

func TestNoBodyFromAnotherOrigin(t *testing.T) {
	for _, test := range crossOrigins {
		t.Run("a route refuses "+test.label, func(t *testing.T) {
			mux := routedShowing(t, "POST /notes/{id}/close", toClosedNote)

			response := serve(t, mux, fromOrigin(closing("", http.NoBody), test.secFetchSite, test.origin))

			assert.Equal(t, http.StatusForbidden, response.Code)
			assert.JSONEq(t, `{"message": "`+fromAnotherOrigin+`"}`, response.Body.String())
		})

		t.Run("BodyOf refuses "+test.label, func(t *testing.T) {
			_, err := httpapi.BodyOf[httpapi.NoBody](fromOrigin(bodyRequest("", http.NoBody), test.secFetchSite, test.origin))

			require.ErrorIs(t, err, httpapi.ErrForbidden)
			assert.EqualError(t, err, fromAnotherOrigin)
		})
	}

	for _, test := range sameOrigins {
		t.Run("a route accepts "+test.label, func(t *testing.T) {
			mux := routedShowing(t, "POST /notes/{id}/close", toClosedNote)

			response := serve(t, mux, fromOrigin(closing("", http.NoBody), test.secFetchSite, test.origin))

			assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.JSONEq(t, `{"revision": "0", "id": "42", "text": "closed"}`, response.Body.String())
		})

		t.Run("BodyOf accepts "+test.label, func(t *testing.T) {
			_, err := httpapi.BodyOf[httpapi.NoBody](fromOrigin(bodyRequest("", http.NoBody), test.secFetchSite, test.origin))

			assert.NoError(t, err)
		})
	}

	t.Run("a route refuses another origin before it looks at the body", func(t *testing.T) {
		mux := routedShowing(t, "POST /notes/{id}/close", toClosedNote)

		response := serve(t, mux, fromOrigin(closing("application/json", strings.NewReader(`{"text":"hello"}`)), "cross-site", ""))

		assert.Equal(t, http.StatusForbidden, response.Code, "a body from another origin has to be refused for its origin, not for the body")
		assert.JSONEq(t, `{"message": "`+fromAnotherOrigin+`"}`, response.Body.String())
	})

	t.Run("BodyOf refuses another origin before it reads the body", func(t *testing.T) {
		for label, body := range map[string]io.Reader{
			"a body":           strings.NewReader(`{"text":"hello"}`),
			"a body too large": strings.NewReader(strings.Repeat(" ", httpapi.MaxRequestBody+1)),
			"a broken body":    failingReader{},
		} {
			t.Run(label, func(t *testing.T) {
				_, err := httpapi.BodyOf[httpapi.NoBody](fromOrigin(bodyRequest("", body), "cross-site", ""))

				assert.EqualError(t, err, fromAnotherOrigin)
			})
		}
	})

	t.Run("BodyOf lets GET, HEAD, and OPTIONS pass, which must not change anything", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
			t.Run(method, func(t *testing.T) {
				request := fromOrigin(httptest.NewRequest(method, "/notes/42", nil), "cross-site", "")

				_, err := httpapi.BodyOf[httpapi.NoBody](request)

				assert.NoError(t, err)
			})
		}
	})

	t.Run("a route with a body accepts another origin, since it requires JSON", func(t *testing.T) {
		mux := routedShowing(t, "POST /notes/{id}/text", func(r *http.Request, request textRequest, _ user) (note, error) {
			return note{ID: r.PathValue("id"), Text: request.Text}, nil
		})

		response := serve(t, mux, fromOrigin(postingTo("/notes/42/text", `{"text":"hello"}`), "cross-site", ""))

		assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.JSONEq(t, `{"revision": "0", "id": "42", "text": "hello"}`, response.Body.String())
	})

	t.Run("BodyOf with a body accepts another origin", func(t *testing.T) {
		request := fromOrigin(bodyRequest("application/json", strings.NewReader(`{"text":"hello"}`)), "cross-site", "")

		body, err := httpapi.BodyOf[textRequest](request)

		require.NoError(t, err)
		assert.Equal(t, textRequest{Text: "hello"}, body)
	})
}
