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
	"time"

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

// publicationDate decodes itself from a date without a time of day, and fails
// with the error of package time if it is none.
type publicationDate struct{ time.Time }

func (value *publicationDate) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}

	parsed, err := time.Parse(time.DateOnly, text)
	value.Time = parsed

	return err
}

// annotations decodes itself from JSON text in a string, and fails with the
// error of encoding/json if it is none, which points into the string rather
// than into the body.
type annotations map[string]string

func (value *annotations) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}

	return json.Unmarshal([]byte(text), (*map[string]string)(value))
}

type catalogEntry struct {
	ISBN        isbn            `json:"isbn"`
	PublishedOn publicationDate `json:"publishedOn"`
	Annotations annotations     `json:"annotations"`
}

// price decodes itself with encoding/json, as a type does that checks its
// fields once they are decoded, and fails with the error of encoding/json,
// whose path points into the JSON text of the price rather than into the
// body.
type price struct {
	Amount   int    `json:"amount"`
	Currency string `json:"currency"`
}

func (value *price) UnmarshalJSON(data []byte) error {
	type plain price

	return json.Unmarshal(data, (*plain)(value))
}

// shelfMark decodes itself from text, which holds JSON text of its own, and
// fails with the error of encoding/json as well.
type shelfMark struct {
	Row int `json:"row"`
}

func (value *shelfMark) UnmarshalText(text []byte) error {
	type plain shelfMark

	return json.Unmarshal(text, (*plain)(value))
}

// keywords decodes itself from JSON text in a string, in which it refuses a
// name that occurs twice, with the error of encoding/json, whose offset points
// into the string rather than into the body.
type keywords map[string]int

func (value *keywords) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}

	return jsonv2.Unmarshal([]byte(text), (*map[string]int)(value), json.DefaultOptionsV1(), jsontext.AllowDuplicateNames(false))
}

// ratings does the same as keywords, by the rules of encoding/json/v2, whose
// error is one of encoding/json/jsontext.
type ratings map[string]int

func (value *ratings) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}

	return jsonv2.Unmarshal([]byte(text), (*map[string]int)(value))
}

// labels refuses any list of labels, with an error that is a syntax error
// and says that a name occurs twice, without pointing to one.
type labels []string

func (*labels) UnmarshalJSON([]byte) error {
	return errors.Join(&json.SyntaxError{Offset: 1}, jsontext.ErrDuplicateName)
}

// stockEntry holds types that decode themselves next to fields whose names
// they use as well, one of which takes its number in a string.
type stockEntry struct {
	Price    price          `json:"price"`
	Amount   int            `json:"amount,string"`
	Mark     shelfMark      `json:"mark"`
	Keywords keywords       `json:"keywords"`
	Ratings  ratings        `json:"ratings"`
	Labels   labels         `json:"labels"`
	ByName   map[string]int `json:"byName"`
	Count    int            `json:"count"`
}

func bodyRequest(contentType string, body io.Reader) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/preview", body)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	return request
}

// bodyErrorOf decodes a JSON body into a TBody and returns the error.
func bodyErrorOf[TBody any](body string) error {
	_, err := httpapi.BodyOf[TBody](bodyRequest("application/json", strings.NewReader(body)))

	return err
}

// assertMalformed asserts that err is ErrMalformed with the given text after
// its own.
func assertMalformed(t *testing.T, err error, text string) {
	t.Helper()

	require.ErrorIs(t, err, httpapi.ErrMalformed)
	assert.EqualError(t, err, "malformed request: "+text)
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
			{label: "an empty body", body: ``, text: "empty body"},
			{label: "a body of nothing but whitespace", body: " \t\r\n", text: "empty body"},
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
			{label: "a list where the body has to be an object", body: `[]`, text: "the body must be an object"},
			{label: "a number too large for its field", body: `{"quantity":1e400}`, text: `"quantity" is out of range`},
			{label: "a fraction where an integer belongs", body: `{"quantity":1.5}`, text: `"quantity" must be an integer`},
			{label: "an unknown field before a value of the wrong kind", body: `{"quantiy":3,"quantity":"three"}`, text: `unknown field "quantiy"`},
			{label: "a value of the wrong kind before an unknown field", body: `{"quantity":"three","quantiy":3}`, text: `"quantity" must be a number`},
			// The whole body has to be JSON before anything is decoded, so a
			// mistake in the JSON comes first, wherever it is.
			{label: "a value of the wrong kind in JSON that ends too early", body: `{"quantity":"three"`, text: "invalid JSON"},
			{label: "an unknown field before a name that occurs twice", body: `{"quantiy":3,"customerId":"42","customerId":"43"}`, text: `duplicate field "customerId"`},
			{label: "a name that occurs twice with values of the wrong kind", body: `{"quantity":"three","quantity":"four"}`, text: `duplicate field "quantity"`},
			{label: "a name that occurs twice in another case, after a value of the wrong kind", body: `{"quantity":"three","QUANTITY":"four"}`, text: `duplicate field "QUANTITY"`},
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

	t.Run("says which kind the body as a whole has to be, or why it does not fit", func(t *testing.T) {
		for _, test := range []struct {
			label string
			err   error
			text  string
		}{
			{label: "a list for an object", err: bodyErrorOf[previewRequest](`[]`), text: "the body must be an object"},
			{label: "a number for an object", err: bodyErrorOf[previewRequest](`1`), text: "the body must be an object"},
			{label: "a string for an object", err: bodyErrorOf[previewRequest](`"42"`), text: "the body must be an object"},
			{label: "a boolean for an object", err: bodyErrorOf[previewRequest](`true`), text: "the body must be an object"},
			{label: "a list for a pointer to an object", err: bodyErrorOf[*previewRequest](`[]`), text: "the body must be an object"},
			{label: "a list for a map", err: bodyErrorOf[map[string]int](`[]`), text: "the body must be an object"},
			{label: "an object for a list", err: bodyErrorOf[[]previewItem](`{}`), text: "the body must be an array"},
			{label: "a number for a string", err: bodyErrorOf[string](`42`), text: "the body must be a string"},
			{label: "a boolean for a number", err: bodyErrorOf[int](`true`), text: "the body must be a number"},
			{label: "a number too large", err: bodyErrorOf[int](`1e400`), text: "the body is out of range"},
			{label: "a fraction for an integer", err: bodyErrorOf[int](`1.5`), text: "the body must be an integer"},
			{label: "a string that is no base64", err: bodyErrorOf[[]byte](`"!!"`), text: "the body must be base64"},
			{label: "a string that is no number", err: bodyErrorOf[json.Number](`"many"`), text: "the body must be a number"},
			{label: "a string that is no time", err: bodyErrorOf[time.Time](`"tomorrow"`),
				text: `the body must be a time such as "2026-10-05T12:00:00Z"`},
			{label: "a key that is no number", err: bodyErrorOf[map[int]int](`{"x":1}`), text: "the keys of the body must be numbers"},
		} {
			t.Run(test.label, func(t *testing.T) {
				assertMalformed(t, test.err, test.text)
			})
		}
	})

	t.Run("says which number does not fit its type", func(t *testing.T) {
		// A number is out of range if it is too large or too small for its type,
		// and an integer has to be written as one, without a fraction or an
		// exponent, which encoding/json refuses for an integer even if the number
		// is one.
		type stockLevel struct {
			Count    int           `json:"count"`
			Shelf    int8          `json:"shelf"`
			Copies   uint          `json:"copies"`
			Floor    uint8         `json:"floor"`
			Weight   float32       `json:"weight"`
			Price    float64       `json:"price"`
			Rating   any           `json:"rating"`
			LoanTime time.Duration `json:"loanTime"`
			Items    []previewItem `json:"items"`
		}

		for _, test := range []struct {
			body string
			text string
		}{
			{body: `{"count":1e400}`, text: `"count" is out of range`},
			{body: `{"count":-1e400}`, text: `"count" is out of range`},
			{body: `{"count":99999999999999999999}`, text: `"count" is out of range`},
			// Written as an integer, a number is out of range, even if a float64
			// rounds it to the smallest integer that fits.
			{body: `{"count":-9223372036854775809}`, text: `"count" is out of range`},
			{body: `{"count":9223372036854775808}`, text: `"count" is out of range`},
			{body: `{"count":9.223372036854775808e18}`, text: `"count" is out of range`},
			{body: `{"count":-9.223372036854775808e18}`, text: `"count" must be an integer`},
			{body: `{"count":1.5}`, text: `"count" must be an integer`},
			{body: `{"count":-0.5}`, text: `"count" must be an integer`},
			{body: `{"count":1.0}`, text: `"count" must be an integer`},
			{body: `{"count":1e2}`, text: `"count" must be an integer`},
			{body: `{"count":1E2}`, text: `"count" must be an integer`},
			{body: `{"shelf":128}`, text: `"shelf" is out of range`},
			{body: `{"shelf":1.28e2}`, text: `"shelf" is out of range`},
			{body: `{"shelf":-1.28e2}`, text: `"shelf" must be an integer`},
			{body: `{"shelf":-1.29e2}`, text: `"shelf" is out of range`},
			{body: `{"copies":-1}`, text: `"copies" is out of range`},
			{body: `{"copies":-1.5}`, text: `"copies" is out of range`},
			{body: `{"copies":0.0}`, text: `"copies" must be an integer`},
			{body: `{"copies":1.5}`, text: `"copies" must be an integer`},
			{body: `{"floor":256}`, text: `"floor" is out of range`},
			{body: `{"floor":2.56e2}`, text: `"floor" is out of range`},
			{body: `{"floor":2.55e2}`, text: `"floor" must be an integer`},
			{body: `{"weight":1e40}`, text: `"weight" is out of range`},
			{body: `{"price":1e400}`, text: `"price" is out of range`},
			{body: `{"price":-1e400}`, text: `"price" is out of range`},
			{body: `{"rating":1e400}`, text: `"rating" is out of range`},
			{body: `{"rating":{"stars":[1,1e400]}}`, text: `"rating.stars.1" is out of range`},
			{body: `{"loanTime":1.5}`, text: `"loanTime" must be an integer`},
			// A number where a string belongs is of the wrong kind, however large.
			{body: `{"items":[{"bookId":1e400}]}`, text: `"items.0.bookId" must be a string`},
		} {
			t.Run(test.body, func(t *testing.T) {
				assertMalformed(t, bodyErrorOf[stockLevel](test.body), test.text)
			})
		}
	})

	t.Run("says which keys of a map do not fit their type", func(t *testing.T) {
		type inventory struct {
			Stock   map[int]int        `json:"stock"`
			Floors  map[uint8]string   `json:"floors"`
			Weights map[float64]string `json:"weights"`
			Shelves []map[int]int      `json:"shelves"`
			Returns map[time.Time]int  `json:"returns"`
		}

		for _, test := range []struct {
			label string
			body  string
			text  string
		}{
			{label: "a key that is no number", body: `{"stock":{"x":1}}`, text: `the keys of "stock" must be numbers`},
			{label: "a key with whitespace around it", body: `{"stock":{ "x" : 1}}`, text: `the keys of "stock" must be numbers`},
			{label: "a key with a dot in it", body: `{"stock":{"1":1,"1.5":2}}`, text: `the keys of "stock" must be numbers`},
			{label: "a key that is no UTF-8", body: "{\"stock\":{\"\xff\":1}}", text: `the keys of "stock" must be numbers`},
			{label: "a key too large for its type", body: `{"floors":{"300":"a"}}`, text: `the keys of "floors" must be numbers`},
			{label: "a key too large for a float", body: `{"weights":{"1e400":"a"}}`, text: `the keys of "weights" must be numbers`},
			{label: "a key of a map in a list", body: `{"shelves":[{},{"x":1}]}`, text: `the keys of "shelves.1" must be numbers`},
			{label: "a key that is no time", body: `{"returns":{"yesterday":1}}`,
				text: `the keys of "returns" must be times such as "2026-10-05T12:00:00Z"`},
			// A value of a map is no key, even where it is a string.
			{label: "a value of the wrong kind", body: `{"stock":{"1":"x"}}`, text: `"stock.1" must be a number`},
			{label: "a value too large", body: `{"stock":{"1":1e400}}`, text: `"stock.1" is out of range`},
		} {
			t.Run(test.label, func(t *testing.T) {
				assertMalformed(t, bodyErrorOf[inventory](test.body), test.text)
			})
		}
	})

	t.Run("says which value is no time", func(t *testing.T) {
		// encoding/json takes a time.Time in RFC 3339 only.
		type loan struct {
			DueOn      time.Time   `json:"dueOn"`
			ReturnedAt *time.Time  `json:"returnedAt"`
			Reminders  []time.Time `json:"reminders"`
		}

		for _, test := range []struct {
			body string
			text string
		}{
			{body: `{"dueOn":"tomorrow"}`, text: `"dueOn" must be a time such as "2026-10-05T12:00:00Z"`},
			{body: `{"dueOn":"2026-10-05"}`, text: `"dueOn" must be a time such as "2026-10-05T12:00:00Z"`},
			{body: `{"returnedAt":"yesterday"}`, text: `"returnedAt" must be a time such as "2026-10-05T12:00:00Z"`},
			{body: `{"reminders":["2026-10-05T12:00:00Z","soon"]}`, text: `"reminders.1" must be a time such as "2026-10-05T12:00:00Z"`},
			{body: `{"dueOn":1}`, text: `"dueOn" must be a string`},
		} {
			t.Run(test.body, func(t *testing.T) {
				assertMalformed(t, bodyErrorOf[loan](test.body), test.text)
			})
		}

		t.Run("and keeps the error of parsing it inspectable", func(t *testing.T) {
			err := bodyErrorOf[loan](`{"dueOn":"tomorrow"}`)

			_, isParse := errors.AsType[*time.ParseError](err)
			assert.True(t, isParse, "errors.As has to find the error of parsing")

			failure, isFailure := errors.AsType[*jsonv2.SemanticError](err)
			require.True(t, isFailure, "errors.As has to find the error that points to the field")
			assert.Equal(t, jsontext.Pointer("/dueOn"), failure.JSONPointer)
		})
	})

	t.Run("says that a json.Number takes a number", func(t *testing.T) {
		// encoding/json leaves out where a json.Number is, wherever it is.
		type valuation struct {
			Price  json.Number   `json:"price"`
			Offers []json.Number `json:"offers"`
		}

		for _, test := range []struct {
			body string
			text string
		}{
			{body: `{"price":"cheap"}`, text: `"price" must be a number`},
			{body: `{"price":true}`, text: `"price" must be a number`},
			{body: `{"offers":[1,"many"]}`, text: `"offers.1" must be a number`},
		} {
			t.Run(test.body, func(t *testing.T) {
				assertMalformed(t, bodyErrorOf[valuation](test.body), test.text)
			})
		}
	})

	t.Run("says that a slice of bytes takes a string in base64", func(t *testing.T) {
		type cover struct {
			Image []byte `json:"image"`
		}

		assertMalformed(t, bodyErrorOf[cover](`{"image":42}`), `"image" must be a string`)
		assertMalformed(t, bodyErrorOf[cover](`{"image":"!!"}`), `"image" must be base64`)
		assertMalformed(t, bodyErrorOf[cover](`{"image":"QQ"}`), `"image" must be base64`)
	})

	t.Run("says what a field with the option string takes", func(t *testing.T) {
		// Such a field takes its number, boolean, or string in a string, as JSON
		// text of its own.
		type quotedPreview struct {
			Quantity int         `json:"quantity,string"`
			Copies   uint        `json:"copies,string"`
			Weight   float32     `json:"weight,string"`
			Limit    *int        `json:"limit,string"`
			IsGift   bool        `json:"isGift,string"`
			Note     string      `json:"note,string"`
			Price    json.Number `json:"price,string"`
			//lint:ignore SA5008 the test is about the option on a type that it does not apply to
			Delivery deliveryOption `json:"delivery,string"`
		}

		for _, test := range []struct {
			body string
			text string
		}{
			{body: `{"quantity":true}`, text: `"quantity" must be a string that holds a number`},
			{body: `{"QUANTITY":true}`, text: `"QUANTITY" must be a string that holds a number`},
			{body: `{"quantity":3}`, text: `"quantity" must be a string that holds a number`},
			{body: `{"quantity":"three"}`, text: `"quantity" must be a string that holds a number`},
			{body: `{"quantity":""}`, text: `"quantity" must be a string that holds a number`},
			{body: `{"quantity":" 3"}`, text: `"quantity" must be a string that holds a number`},
			{body: `{"quantity":"3e"}`, text: `"quantity" must be a string that holds a number`},
			{body: `{"quantity":"\"3\""}`, text: `"quantity" must be a string that holds a number`},
			{body: `{"quantity":"1.5"}`, text: `"quantity" must be an integer`},
			{body: `{"quantity":"1e400"}`, text: `"quantity" is out of range`},
			{body: `{"copies":"-1"}`, text: `"copies" is out of range`},
			{body: `{"weight":"1e40"}`, text: `"weight" is out of range`},
			{body: `{"limit":true}`, text: `"limit" must be a string that holds a number`},
			{body: `{"isGift":"yes"}`, text: `"isGift" must be a string that holds a boolean`},
			{body: `{"isGift":true}`, text: `"isGift" must be a string that holds a boolean`},
			{body: `{"note":"hello"}`, text: `"note" must be a string that holds a string`},
			{body: `{"note":1}`, text: `"note" must be a string that holds a string`},
			{body: `{"price":"\"12\""}`, text: `"price" must be a string that holds a number`},
			// The option applies to booleans, numbers, and strings only, so
			// encoding/json ignores it for anything else.
			{body: `{"delivery":1}`, text: `"delivery" must be an object`},
		} {
			t.Run(test.body, func(t *testing.T) {
				assertMalformed(t, bodyErrorOf[quotedPreview](test.body), test.text)
			})
		}
	})

	t.Run("keeps the text of the decoder for what it has no words of its own for", func(t *testing.T) {
		// encoding/json takes no boolean for the key of a map. It ignores the
		// option string for a time, which encoding/json/v2 refuses, so the
		// second decoding fails elsewhere and says nothing about the time, nor
		// where a json.Number is.
		type shelf struct {
			ByAvailability map[bool]string `json:"byAvailability"`
			//lint:ignore SA5008 the test is about the option on a type that it does not apply to
			ArrivedAt time.Time `json:"arrivedAt,string"`
			DueOn     time.Time `json:"dueOn"`
			//lint:ignore SA5008 the test is about the option on a type that it does not apply to
			Tags  []string    `json:"tags,string"`
			Price json.Number `json:"price"`
		}

		for _, test := range []struct {
			body string
			text string
		}{
			{body: `{"byAvailability":{"true":"a"}}`, text: "json: cannot unmarshal string into Go struct field shelf.byAvailability.true of type bool"},
			{body: `{"arrivedAt":"yesterday"}`, text: `parsing time "yesterday" as "2006-01-02T15:04:05Z07:00": cannot parse "yesterday" as "2006"`},
			{body: `{"arrivedAt":"2026-10-05T12:00:00Z","dueOn":"tomorrow"}`,
				text: `parsing time "tomorrow" as "2006-01-02T15:04:05Z07:00": cannot parse "tomorrow" as "2006"`},
			{body: `{"tags":["new"],"price":true}`, text: "json: cannot unmarshal bool into Go value of type json.Number"},
		} {
			t.Run(test.body, func(t *testing.T) {
				assertMalformed(t, bodyErrorOf[shelf](test.body), test.text)
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

		// So does an error of package time, or of encoding/json, whose offset
		// points into the JSON text of the type, not into the body, where it
		// happens to point to the name of a field.
		for _, test := range []struct {
			body string
			text string
		}{
			{body: `{"publishedOn":"yesterday"}`, text: `parsing time "yesterday" as "2006-01-02": cannot parse "yesterday" as "2006"`},
			{body: `{"annotations":"x"}`, text: "invalid character 'x' looking for beginning of value"},
			{body: `{"annotations":"{\"a\":1}"}`, text: "json: cannot unmarshal number into Go struct field .a of type string"},
		} {
			t.Run(test.body, func(t *testing.T) {
				assertMalformed(t, bodyErrorOf[catalogEntry](test.body), test.text)
			})
		}

		// A value that does not fit names the path in the JSON text of the
		// type, which is no path in the body, and may even be the path of
		// another field, which takes its number in a string.
		for _, test := range []struct {
			body string
			text string
		}{
			{body: `{"price":{"amount":"x"}}`, text: "json: cannot unmarshal string into Go struct field plain.amount of type int"},
			{body: `{"price":{"currency":5}}`, text: "json: cannot unmarshal number into Go struct field plain.currency of type string"},
			{body: `{"mark":"{\"row\":\"x\"}"}`, text: "json: cannot unmarshal string into Go struct field plain.row of type int"},
		} {
			t.Run(test.body, func(t *testing.T) {
				err := bodyErrorOf[stockEntry](test.body)

				assertMalformed(t, err, test.text)

				_, isMismatch := errors.AsType[*json.UnmarshalTypeError](err)
				assert.True(t, isMismatch, "the error of the type has to stay inspectable")
			})
		}

		t.Run("in a list", func(t *testing.T) {
			assertMalformed(t, bodyErrorOf[[]stockEntry](`[{"price":{"currency":5}}]`),
				"json: cannot unmarshal number into Go struct field plain.currency of type string")
		})

		// So does a name that occurs twice in the JSON text of the type, whose
		// offset points to anything in the body, even to the name of a field.
		for _, test := range []struct {
			label string
			body  string
			text  string
		}{
			{label: "at the name of a field", body: `{"count":1,"keywords":"{\"a\":1,` + strings.Repeat(" ", 3) + `\"a\":2}"}`, text: "duplicate object member name"},
			{label: "with an error of encoding/json/jsontext", body: `{"ratings":"{\"a\":1,\"a\":2}"}`, text: "duplicate object member name"},
			{label: "with a syntax error that says so without pointing to a name", body: `{"labels":["a","a"]}`, text: "\nduplicate object member name"},
			// The offset points to a name that occurs twice in another case, in
			// an object that is decoded into a map, which takes both.
			{label: "at a name that occurs twice in a map", body: `{"byName":{"x":1,"X":2},"keywords":"{\"a\":1,` + strings.Repeat(" ", 9) + `\"a\":2}"}`,
				text: "duplicate object member name"},
			{label: "at a name that occurs twice in a map, with an error of encoding/json/jsontext", body: `{"byName":{"x":1,"X":2},"ratings":"{\"a\":1,` + strings.Repeat(" ", 10) + `\"a\":2}"}`,
				text: "duplicate object member name"},
		} {
			t.Run(test.label, func(t *testing.T) {
				assertMalformed(t, bodyErrorOf[stockEntry](test.body), test.text)
			})
		}
	})

	t.Run("still says what a type that decodes itself from text takes", func(t *testing.T) {
		// encoding/json refuses anything but a string for it, before the type
		// decodes anything.
		assertMalformed(t, bodyErrorOf[stockEntry](`{"mark":1}`), `"mark" must be a string`)
	})

	t.Run("still names a value after a time with the option string", func(t *testing.T) {
		// encoding/json ignores the option for a time, which encoding/json/v2
		// refuses, so the second decoding fails at the time, which decodes
		// itself, but is no type of the application.
		type delivery struct {
			//lint:ignore SA5008 the test is about the option on a type that it does not apply to
			ArrivedAt time.Time `json:"arrivedAt,string"`
			Count     int       `json:"count"`
		}

		assertMalformed(t, bodyErrorOf[delivery](`{"arrivedAt":"2026-10-05T12:00:00Z","count":"x"}`), `"count" must be a number`)
	})

	t.Run("names a value by the names in its path, even if they hold a dot, or are empty", func(t *testing.T) {
		type dotted struct {
			ShelfRow int `json:"shelf.row,string"`
		}

		// The name of a field has a dot in it, and looks like the path to
		// another field, which takes its number in a string.
		type collision struct {
			Shelf struct {
				Row int `json:"row,string"`
			} `json:"shelf"`
			IsLent bool `json:"shelf.row"`
		}

		for _, test := range []struct {
			label string
			err   error
			text  string
		}{
			{label: "a field with a dot", err: bodyErrorOf[dotted](`{"shelf.row":true}`), text: `"shelf.row" must be a string that holds a number`},
			{label: "a field with a dot that looks like a path", err: bodyErrorOf[collision](`{"shelf.row":1}`), text: `"shelf.row" must be a boolean`},
			{label: "a key with a slash", err: bodyErrorOf[map[string]int](`{"a/b":"x"}`), text: `"a/b" must be a number`},
			{label: "a key with a tilde", err: bodyErrorOf[map[string]int](`{"a~b":"x"}`), text: `"a~b" must be a number`},
			{label: "an empty key", err: bodyErrorOf[map[string]int](`{"":"x"}`), text: `"" must be a number`},
			{label: "an empty key of a map whose keys are numbers", err: bodyErrorOf[map[string]map[int]int](`{"":{"x":1}}`),
				text: `the keys of "" must be numbers`},
			{label: "an empty key of a map of times", err: bodyErrorOf[map[string]time.Time](`{"":"x"}`),
				text: `"" must be a time such as "2026-10-05T12:00:00Z"`},
			{label: "an empty key of a map whose keys are times", err: bodyErrorOf[map[string]map[time.Time]int](`{"":{"x":1}}`),
				text: `the keys of "" must be times such as "2026-10-05T12:00:00Z"`},
		} {
			t.Run(test.label, func(t *testing.T) {
				assertMalformed(t, test.err, test.text)
			})
		}
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
			// encoding/json passes over these failures to refuse the name all
			// the same, while the second decoding stops at them.
			{label: "in a different case, after a value of the wrong kind", body: `{"quantity":"three","QUANTITY":"four"}`, name: "QUANTITY"},
			{label: "in a different case, after a number too large", body: `{"quantity":1e400,"Quantity":1}`, name: "Quantity"},
			{label: "in a different case, after an unknown field", body: `{"quantiy":3,"quantity":1,"QUANTITY":2}`, name: "QUANTITY"},
			{label: "in a different case, in an object in a list, after a value of the wrong kind", body: `{"items":[{"bookId":1,"BOOKID":"2"}]}`, name: "BOOKID"},
			{label: "in a different case, spelled with an escape, after a value of the wrong kind", body: `{"quantity":"three","QUANTITY":4}`, name: "QUANTITY"},
		} {
			t.Run(test.label, func(t *testing.T) {
				request := bodyRequest("application/json", strings.NewReader(test.body))

				preview, err := httpapi.BodyOf[previewRequest](request)
				require.ErrorIs(t, err, httpapi.ErrMalformed)
				assert.EqualError(t, err, `malformed request: duplicate field "`+test.name+`"`, "the error has to say which name")
				assert.Zero(t, preview, "nothing half-decoded may be handed back")
			})
		}

		for _, test := range []struct {
			label string
			body  string
			name  string
		}{
			{label: "in a different case, after the error of a type that decodes itself", body: `{"isbn":"42","ISBN":"9783161484100"}`, name: "ISBN"},
			{label: "in a different case, after a date of a type that decodes itself", body: `{"publishedOn":"x","PublishedOn":"2026-10-05"}`, name: "PublishedOn"},
		} {
			t.Run(test.label, func(t *testing.T) {
				assertMalformed(t, bodyErrorOf[catalogEntry](test.body), `duplicate field "`+test.name+`"`)
			})
		}

		t.Run("in a different case, after a name that occurs twice in the JSON text of a type that decodes itself", func(t *testing.T) {
			assertMalformed(t, bodyErrorOf[stockEntry](`{"keywords":"{\"a\":1,\"a\":2}","count":1,"COUNT":2}`), `duplicate field "COUNT"`)
		})

		t.Run("in a different case, after a time that does not parse", func(t *testing.T) {
			type loan struct {
				DueOn time.Time `json:"dueOn"`
				Count int       `json:"count"`
			}

			assertMalformed(t, bodyErrorOf[loan](`{"dueOn":"x","count":1,"COUNT":2}`), `duplicate field "COUNT"`)
		})
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

		t.Run("for a name that occurs twice in another case", func(t *testing.T) {
			err := bodyOf(t, `{"customerId":"42","CUSTOMERID":"43"}`)

			duplicate, isDuplicate := errors.AsType[*jsontext.SyntacticError](err)
			require.True(t, isDuplicate, "errors.As has to find the error that points to the name")
			assert.ErrorIs(t, duplicate, jsontext.ErrDuplicateName)
			assert.Equal(t, jsontext.Pointer("/CUSTOMERID"), duplicate.JSONPointer)

			_, isSyntax := errors.AsType[*json.SyntaxError](err)
			assert.True(t, isSyntax, "the error of decoding has to stay wrapped")
		})

		for label, body := range map[string]string{
			"for a number too large":       `{"quantity":1e400}`,
			"for a fraction":               `{"quantity":1.5}`,
			"for a body of the wrong kind": `[]`,
		} {
			t.Run(label, func(t *testing.T) {
				err := bodyOf(t, body)

				_, isMismatch := errors.AsType[*json.UnmarshalTypeError](err)
				assert.True(t, isMismatch, "errors.As has to find the error of decoding")
			})
		}

		for label, body := range map[string]string{
			"for an empty body":        ``,
			"for JSON that is not":     `not json`,
			"for data after the value": `{"customerId":"42"} garbage`,
			"for a name that occurs twice in another case, after a value of the wrong kind": `{"quantity":"three","QUANTITY":"four"}`,
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
			{label: "an empty body", contentType: "application/json", body: ``, status: http.StatusBadRequest,
				message: "malformed request: empty body"},
			{label: "a number too large", contentType: "application/json", body: `{"quantity":1e400}`, status: http.StatusBadRequest,
				message: `malformed request: "quantity" is out of range`},
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
