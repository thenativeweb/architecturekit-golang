package httpapi_test

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
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
	CustomerID string          `json:"customerId"`
	Quantity   int             `json:"quantity"`
	Delivery   *deliveryOption `json:"delivery,omitempty"`
	Items      []previewItem   `json:"items,omitempty"`
	IsGift     bool            `json:"isGift,omitempty"`
}

type deliveryOption struct {
	Address string `json:"address"`
}

type previewItem struct {
	BookID string `json:"bookId"`
}

// errNotAnISBN is what isbn fails with.
var errNotAnISBN = errors.New("an ISBN has 13 digits")

// isbn decodes itself, and refuses anything but 13 digits.
type isbn string

func (value *isbn) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil || len(text) != 13 {
		return errNotAnISBN
	}

	*value = isbn(text)

	return nil
}

type catalogEntry struct {
	ISBN isbn `json:"isbn"`
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

	t.Run("rejects JSON that does not fit, and says why in words of its own", func(t *testing.T) {
		// The decoder would name the types of Go, such as "Go struct field
		// previewRequest.quantity of type int", which the caller does not know.
		for _, test := range []struct {
			label string
			body  string
			text  string
		}{
			{label: "not JSON at all", body: `not json`, text: "invalid JSON"},
			{label: "JSON that ends too early", body: `{"customerId":"42"`, text: "invalid JSON"},
			{label: "JSON without a value", body: `{"customerId":}`, text: "invalid JSON"},
			{label: "JSON with a comma too many", body: `{"customerId":"42",}`, text: "invalid JSON"},
			{label: "JSON with single quotes", body: `{'customerId':'42'}`, text: "invalid JSON"},
			{label: "JSON after a character that is no whitespace in JSON", body: "\f{}", text: "invalid JSON"},
			{label: "an unknown field", body: `{"customerId":"42","quantiy":3}`, text: `unknown field "quantiy"`},
			{label: "a nested unknown field", body: `{"delivery":{"adress":"Main Street 1"}}`, text: `unknown field "adress"`},
			{label: "an unknown field in an object in a list", body: `{"items":[{"bookID":"1","isbn":"2"}]}`, text: `unknown field "isbn"`},
			{label: "an unknown field with a quote in its name", body: `{"quan\"tity":3}`, text: `unknown field "quan\"tity"`},
			{label: "a string where a number belongs", body: `{"quantity":"three"}`, text: `"quantity" must be a number`},
			{label: "a number where a string belongs", body: `{"customerId":42}`, text: `"customerId" must be a string`},
			{label: "a string where a boolean belongs", body: `{"isGift":"yes"}`, text: `"isGift" must be a boolean`},
			{label: "an object where a list belongs", body: `{"items":{}}`, text: `"items" must be an array`},
			{label: "a list where an object belongs", body: `{"delivery":[]}`, text: `"delivery" must be an object`},
			{label: "a nested value of the wrong kind", body: `{"delivery":{"address":1}}`, text: `"delivery.address" must be a string`},
			{label: "a value of the wrong kind in a list", body: `{"items":[{"bookId":"1"},{"bookId":2}]}`, text: `"items.1.bookId" must be a string`},
			{label: "an unknown field before a value of the wrong kind", body: `{"quantiy":3,"quantity":"three"}`, text: `unknown field "quantiy"`},
			{label: "a value of the wrong kind before an unknown field", body: `{"quantity":"three","quantiy":3}`, text: `"quantity" must be a number`},
			// The whole body has to be JSON before anything is decoded, so a
			// mistake in the JSON comes first, wherever it is.
			{label: "a value of the wrong kind in JSON that ends too early", body: `{"quantity":"three"`, text: "invalid JSON"},
			{label: "an unknown field before a name that occurs twice", body: `{"quantiy":3,"customerId":"42","customerId":"43"}`, text: `duplicate field "customerId"`},
			{label: "a name that occurs twice with values of the wrong kind", body: `{"quantity":"three","quantity":"four"}`, text: `duplicate field "quantity"`},
		} {
			t.Run(test.label, func(t *testing.T) {
				request := bodyRequest("application/json", strings.NewReader(test.body))

				preview, err := httpapi.BodyOf[previewRequest](request)
				require.ErrorIs(t, err, httpapi.ErrMalformed)
				assert.EqualError(t, err, "malformed request: "+test.text)
				assert.Zero(t, preview, "nothing half-decoded may be handed back")
			})
		}
	})

	t.Run("keeps the text of the decoder for what it has no words of its own for", func(t *testing.T) {
		// These texts are to be replaced as well, once there are words for them.
		for _, test := range []struct {
			label string
			body  string
			text  string
		}{
			{label: "an empty body", body: ``, text: "unexpected end of JSON input"},
			{label: "a body of nothing but whitespace", body: " \t\r\n", text: "unexpected end of JSON input"},
			{label: "a list where the body has to be an object", body: `[]`,
				text: "json: cannot unmarshal array into Go value of type httpapi_test.previewRequest"},
			{label: "a number too large for its field", body: `{"quantity":1e400}`,
				text: "json: cannot unmarshal number 1e400 into Go struct field previewRequest.quantity of type int"},
			{label: "a fraction where an integer belongs", body: `{"quantity":1.5}`,
				text: "json: cannot unmarshal number 1.5 into Go struct field previewRequest.quantity of type int"},
			// The reader of JSON text sees no name twice, and the second decoding
			// stops at the value of the wrong kind before it, so nobody names it.
			{label: "a name that occurs twice in another case, after a value of the wrong kind", body: `{"quantity":"three","QUANTITY":"four"}`,
				text: "duplicate object member name"},
		} {
			t.Run(test.label, func(t *testing.T) {
				request := bodyRequest("application/json", strings.NewReader(test.body))

				preview, err := httpapi.BodyOf[previewRequest](request)
				require.ErrorIs(t, err, httpapi.ErrMalformed)
				assert.EqualError(t, err, "malformed request: "+test.text)
				assert.Zero(t, preview, "nothing half-decoded may be handed back")
			})
		}
	})

	t.Run("takes a slice of bytes for a string, in base64", func(t *testing.T) {
		type cover struct {
			Image []byte `json:"image"`
		}

		_, err := httpapi.BodyOf[cover](bodyRequest("application/json", strings.NewReader(`{"image":42}`)))
		assert.EqualError(t, err, `malformed request: "image" must be a string`)

		// A string that is no base64 has no words of its own yet.
		_, err = httpapi.BodyOf[cover](bodyRequest("application/json", strings.NewReader(`{"image":"!!"}`)))
		assert.EqualError(t, err,
			"malformed request: json: cannot unmarshal string into Go struct field cover.image of type []uint8: illegal base64 data at input byte 0")
	})

	t.Run("keeps the text of the decoder for a field with the option string", func(t *testing.T) {
		// Such a field takes its number or boolean in a string, so neither its
		// type nor the error of decoding says which kind it takes.
		type quotedPreview struct {
			Quantity int  `json:"quantity,string"`
			IsGift   bool `json:"isGift,string"`
		}

		for _, test := range []struct {
			body string
			text string
		}{
			{body: `{"quantity":true}`, text: "json: cannot unmarshal bool into Go struct field quotedPreview.quantity of type int"},
			{body: `{"QUANTITY":true}`, text: "json: cannot unmarshal bool into Go struct field quotedPreview.QUANTITY of type int"},
			{body: `{"isGift":"yes"}`, text: `json: cannot unmarshal string "yes" into Go struct field quotedPreview.isGift of type bool: invalid syntax`},
			{body: `{"isGift":true}`, text: "json: cannot unmarshal bool into Go struct field quotedPreview.isGift of type bool"},
		} {
			t.Run(test.body, func(t *testing.T) {
				_, err := httpapi.BodyOf[quotedPreview](bodyRequest("application/json", strings.NewReader(test.body)))

				require.ErrorIs(t, err, httpapi.ErrMalformed)
				assert.EqualError(t, err, "malformed request: "+test.text)
			})
		}
	})

	t.Run("keeps the text of the decoder for a field of a type that JSON has no kind for", func(t *testing.T) {
		type measurement struct {
			Value complex128 `json:"value"`
		}

		_, err := httpapi.BodyOf[measurement](bodyRequest("application/json", strings.NewReader(`{"value":1}`)))

		require.ErrorIs(t, err, httpapi.ErrMalformed)
		assert.EqualError(t, err, "malformed request: json: cannot unmarshal number into Go struct field measurement.value of type complex128")
	})

	t.Run("keeps the text of the error of a type that decodes itself", func(t *testing.T) {
		// The application wrote the error, so it is the one to say what is wrong.
		_, err := httpapi.BodyOf[catalogEntry](bodyRequest("application/json", strings.NewReader(`{"isbn":"42"}`)))

		require.ErrorIs(t, err, httpapi.ErrMalformed)
		assert.EqualError(t, err, "malformed request: an ISBN has 13 digits")
		assert.ErrorIs(t, err, errNotAnISBN, "the error of the type has to stay inspectable")
	})

	t.Run("rejects anything but whitespace after the value", func(t *testing.T) {
		// Another parser might read on, and use the second value instead of
		// the first.
		for _, test := range []struct {
			label string
			body  string
		}{
			{label: "a second value", body: `{"customerId":"42"} {"customerId":"43"}`},
			{label: "a second value without a space", body: `{"customerId":"42"}{"customerId":"43"}`},
			{label: "garbage", body: `{"customerId":"42"} garbage`},
			{label: "a closing bracket", body: `{"customerId":"42"}]`},
			{label: "garbage after a value of the wrong kind", body: `{"quantity":"three"} garbage`},
			{label: "garbage after an unknown field", body: `{"quantiy":3} garbage`},
			{label: "garbage after a name that occurs twice in another case", body: `{"customerId":"42","CUSTOMERID":"43"} garbage`},
		} {
			t.Run(test.label, func(t *testing.T) {
				request := bodyRequest("application/json", strings.NewReader(test.body))

				preview, err := httpapi.BodyOf[previewRequest](request)
				require.ErrorIs(t, err, httpapi.ErrMalformed)
				assert.EqualError(t, err, "malformed request: data after the JSON value")
				assert.Zero(t, preview, "nothing half-decoded may be handed back")
			})
		}
	})

	t.Run("accepts whitespace after the value", func(t *testing.T) {
		request := bodyRequest("application/json", strings.NewReader(`{"customerId":"42"}`+" \t\r\n"))

		preview, err := httpapi.BodyOf[previewRequest](request)
		require.NoError(t, err)
		assert.Equal(t, previewRequest{CustomerID: "42"}, preview)
	})

	t.Run("rejects a name that occurs twice", func(t *testing.T) {
		// Parsers disagree on which value counts, so a filter in front of the
		// application might check another value than the one it uses.
		for _, test := range []struct {
			label string
			body  string
			name  string
		}{
			{label: "at the top", body: `{"customerId":"42","customerId":"43"}`, name: "customerId"},
			{label: "in a nested object", body: `{"delivery":{"address":"a","address":"b"}}`, name: "address"},
			{label: "in an object in a list", body: `{"items":[{"bookId":"1"},{"bookId":"2","bookId":"3"}]}`, name: "bookId"},
			{label: "spelled with an escape", body: `{"customerId":"42","customer\u0049d":"43"}`, name: "customerId"},
			{label: "in a different case", body: `{"customerId":"42","CUSTOMERID":"43"}`, name: "CUSTOMERID"},
			{label: "in a different case, with whitespace after the value", body: `{"customerId":"42","CUSTOMERID":"43"}` + " \n", name: "CUSTOMERID"},
			{label: "with a slash in it", body: `{"a/b":1,"a/b":2}`, name: "a/b"},
		} {
			t.Run(test.label, func(t *testing.T) {
				request := bodyRequest("application/json", strings.NewReader(test.body))

				preview, err := httpapi.BodyOf[previewRequest](request)
				require.ErrorIs(t, err, httpapi.ErrMalformed)
				assert.EqualError(t, err, `malformed request: duplicate field "`+test.name+`"`, "the error has to say which name")
				assert.Zero(t, preview, "nothing half-decoded may be handed back")
			})
		}
	})

	t.Run("still matches names regardless of case", func(t *testing.T) {
		request := bodyRequest("application/json",
			strings.NewReader(`{"CUSTOMERID":"42","Quantity":3,"delivery":{"ADDRESS":"Main Street 1"}}`))

		preview, err := httpapi.BodyOf[previewRequest](request)
		require.NoError(t, err)
		assert.Equal(t, previewRequest{CustomerID: "42", Quantity: 3, Delivery: &deliveryOption{Address: "Main Street 1"}}, preview)
	})

	t.Run("keeps the error of decoding inspectable", func(t *testing.T) {
		bodyOf := func(t *testing.T, body string) error {
			t.Helper()

			_, err := httpapi.BodyOf[previewRequest](bodyRequest("application/json", strings.NewReader(body)))
			require.ErrorIs(t, err, httpapi.ErrMalformed)

			return err
		}

		t.Run("for a value of the wrong kind", func(t *testing.T) {
			err := bodyOf(t, `{"quantity":"three"}`)

			mismatch, isMismatch := errors.AsType[*json.UnmarshalTypeError](err)
			require.True(t, isMismatch, "errors.As has to find the error of decoding")
			assert.Equal(t, "quantity", mismatch.Field)
		})

		t.Run("for an unknown field", func(t *testing.T) {
			err := bodyOf(t, `{"quantiy":3}`)

			unknown, isUnknown := errors.AsType[*jsonv2.SemanticError](err)
			require.True(t, isUnknown, "errors.As has to find the error that points to the field")
			assert.ErrorIs(t, unknown, jsonv2.ErrUnknownName)
			assert.Equal(t, jsontext.Pointer("/quantiy"), unknown.JSONPointer)
			assert.Contains(t, textsOf(err), `json: unknown field "quantiy"`, "the error of decoding has to stay wrapped")
		})

		t.Run("for a name that occurs twice", func(t *testing.T) {
			err := bodyOf(t, `{"quantity":"three","quantity":"four"}`)

			duplicate, isDuplicate := errors.AsType[*jsontext.SyntacticError](err)
			require.True(t, isDuplicate, "errors.As has to find the error that points to the name")
			assert.ErrorIs(t, duplicate, jsontext.ErrDuplicateName)
			assert.Equal(t, jsontext.Pointer("/quantity"), duplicate.JSONPointer)

			_, isSyntax := errors.AsType[*json.SyntaxError](err)
			assert.True(t, isSyntax, "the error of decoding has to stay wrapped")
		})

		for label, body := range map[string]string{
			"for JSON that is not":     `not json`,
			"for data after the value": `{"customerId":"42"} garbage`,
		} {
			t.Run(label, func(t *testing.T) {
				err := bodyOf(t, body)

				_, isSyntax := errors.AsType[*json.SyntaxError](err)
				assert.True(t, isSyntax, "errors.As has to find the error of decoding")
			})
		}
	})

	t.Run("rejects a body that can not be read", func(t *testing.T) {
		_, err := httpapi.BodyOf[previewRequest](bodyRequest("application/json", failingReader{}))
		assert.ErrorIs(t, err, httpapi.ErrMalformed)
		assert.ErrorIs(t, err, errBrokenBody, "the error of reading has to stay inspectable")
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
			message     string
		}{
			{label: "fits", contentType: "application/json", body: `{"customerId":"42"}`, status: http.StatusOK},
			{label: "not JSON", contentType: "text/plain", body: `{"customerId":"42"}`, status: http.StatusUnsupportedMediaType,
				message: `unsupported media type: "text/plain" is not application/json`},
			{label: "too large", contentType: "application/json", body: strings.Repeat(" ", httpapi.MaxRequestBody+1), status: http.StatusRequestEntityTooLarge,
				message: "request body too large: at most 1048576 bytes are read"},
			{label: "an unknown field", contentType: "application/json", body: `{"name":"42"}`, status: http.StatusBadRequest,
				message: `malformed request: unknown field "name"`},
			{label: "a value of the wrong kind", contentType: "application/json", body: `{"quantity":"three"}`, status: http.StatusBadRequest,
				message: `malformed request: "quantity" must be a number`},
			{label: "a second value", contentType: "application/json", body: `{"customerId":"42"} {"customerId":"43"}`, status: http.StatusBadRequest,
				message: "malformed request: data after the JSON value"},
			{label: "a name that occurs twice", contentType: "application/json", body: `{"customerId":"42","customerId":"43"}`, status: http.StatusBadRequest,
				message: `malformed request: duplicate field "customerId"`},
			{label: "not JSON, said to be JSON", contentType: "application/json", body: `not json`, status: http.StatusBadRequest,
				message: "malformed request: invalid JSON"},
		} {
			t.Run(test.label, func(t *testing.T) {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, bodyRequest(test.contentType, strings.NewReader(test.body)))

				assert.Equal(t, test.status, response.Code)
				if test.message != "" {
					assert.JSONEq(t, messageOf(t, test.message), response.Body.String())
				}
			})
		}
	})
}

// textsOf returns the texts of an error and of every error it wraps.
func textsOf(err error) []string {
	if err == nil {
		return nil
	}

	texts := []string{err.Error()}

	switch wrapping := err.(type) {
	case interface{ Unwrap() []error }:
		for _, wrapped := range wrapping.Unwrap() {
			texts = append(texts, textsOf(wrapped)...)
		}
	case interface{ Unwrap() error }:
		texts = append(texts, textsOf(wrapping.Unwrap())...)
	}

	return texts
}
