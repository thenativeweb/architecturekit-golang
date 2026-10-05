package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/httpapi"
)

// An answer is encoded as encoding/json encodes it, except that a nil slice is
// [] and a nil map is {}, at every depth, rather than null, so that a caller
// gets a list or an object, whether it holds anything or not.

// shelfBody is an answer that holds a slice, a map, a pointer, and a list of
// further answers, each of which may be nil.
type shelfBody struct {
	Name     string            `json:"name"`
	BookIDs  []string          `json:"bookIds"`
	Labels   map[string]string `json:"labels"`
	Parent   *shelfBody        `json:"parent"`
	Children []shelfBody       `json:"children"`
}

// emptyShelves are shelves whose slices and maps are nil at every depth, and
// whose pointers are nil, and emptyShelvesJSON is how they are answered.
var (
	emptyShelves = []shelfBody{{Name: "empty", Children: []shelfBody{{Name: "inner"}}}}

	emptyShelvesJSON = `[{"name":"empty","bookIds":[],"labels":{},"parent":null,"children":[` +
		`{"name":"inner","bookIds":[],"labels":{},"parent":null,"children":[]}]}]` + "\n"
)

// respondedWith answers the result with RespondResult, and returns the
// response and what was logged.
func respondedWith(result any) (*httptest.ResponseRecorder, string) {
	var logs bytes.Buffer
	request, api := inAHandler(&logs)
	recorder := httptest.NewRecorder()

	httpapi.RespondResult(recorder, request, api, result, nil)

	return recorder, logs.String()
}

func TestNilSlicesAndMapsInAnswers(t *testing.T) {
	answerShelves := func(context.Context, countNotes) ([]shelfBody, error) { return emptyShelves, nil }

	for kind, options := range map[string][]httpapi.QueryOption{
		"a query":            nil,
		"a revisioned query": {httpapi.Revisioned(seenView("4"), time.Second)},
		"an awaiting query":  {httpapi.Awaiting(seenView("4"), time.Second)},
	} {
		t.Run(kind+" answers a nil slice as [] and a nil map as {} at every depth, and a nil pointer as null", func(t *testing.T) {
			mux := http.NewServeMux()
			httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom), mux, "QUERY /notes", allNotes, answerShelves, options...)

			response := askNotes(mux, nil)

			require.Equal(t, http.StatusOK, response.Code)
			assert.Equal(t, emptyShelvesJSON, response.Body.String())
		})
	}

	t.Run("a handler of your own that answers with Ask and RespondResult does the same", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /notes", func(w http.ResponseWriter, r *http.Request) {
			shelves, err := httpapi.Ask(r, api, allNotes, answerShelves)
			httpapi.RespondResult(w, r, api, shelves, err)
		})

		request := httptest.NewRequest(http.MethodGet, "/notes", nil)
		request.Header.Set("X-User", "golo")
		response := serve(t, mux, request)

		require.Equal(t, http.StatusOK, response.Code)
		assert.Equal(t, emptyShelvesJSON, response.Body.String())
	})

	for _, test := range []struct {
		label  string
		result any
		want   string
	}{
		{label: "a nil slice as the whole result is []", result: []shelfBody(nil), want: `[]`},
		{label: "a nil map as the whole result is {}", result: map[string]int(nil), want: `{}`},
		{label: "a nil pointer as the whole result is null", result: (*shelfBody)(nil), want: `null`},
		{label: "a nil slice or map in a map is [] or {}", result: map[string]any{"list": []int(nil), "map": map[string]int(nil)},
			want: `{"list":[],"map":{}}`},
		{label: "a nil slice or map in a list of interfaces is [] or {}, while a nil pointer or interface is null",
			result: []any{[]string(nil), map[string]int(nil), (*int)(nil), nil}, want: `[[],{},null,null]`},
		{label: "a nil slice or map with omitempty is still left out", result: struct {
			List []int          `json:"list,omitempty"`
			Map  map[int]int    `json:"map,omitempty"`
			Kept map[string]int `json:"kept"`
		}{}, want: `{"kept":{}}`},
		// A slice of bytes is written as a string in base64.
		{label: "a nil slice of bytes is an empty string", result: struct {
			Cover []byte `json:"cover"`
		}{}, want: `{"cover":""}`},
		// A json.RawMessage holds JSON text, which a nil one does not.
		{label: "a nil json.RawMessage is null", result: struct {
			Raw json.RawMessage `json:"raw"`
		}{}, want: `{"raw":null}`},
	} {
		t.Run("RespondResult answers "+test.label, func(t *testing.T) {
			response, _ := respondedWith(test.result)

			require.Equal(t, http.StatusOK, response.Code)
			assert.Equal(t, test.want+"\n", response.Body.String())
		})
	}

	t.Run("Adding answers a nil slice as [] and a nil map as {} in its fields, and a nil pointer as null", func(t *testing.T) {
		mux := routed(httpapi.NewAPI(writingStore(t), userFrom),
			httpapi.Adding(func(httpapi.Handled[note]) (any, error) {
				return struct {
					Tags    []string       `json:"tags"`
					Meta    map[string]int `json:"meta"`
					Parent  *string        `json:"parent"`
					Shelves []shelfBody    `json:"shelves"`
				}{Shelves: emptyShelves}, nil
			}))

		response := postNote(t, mux, `{"id":"1","text":"hello"}`)

		// The fields go through a map on their way into the answer, which sorts
		// the names, and JSONEq tells [] and {} from null as well.
		require.Equal(t, http.StatusOK, response.Code)
		assert.JSONEq(t, `{"revision":"0","tags":[],"meta":{},"parent":null,"shelves":`+emptyShelvesJSON+`}`, response.Body.String())
	})
}

// valueMarshaler encodes itself, from a value, with JSON that holds what
// encoding/json escapes, and whitespace that it removes.
type valueMarshaler struct{}

func (valueMarshaler) MarshalJSON() ([]byte, error) {
	return []byte(`{ "html" : "<b>&amp;</b>" , "list" : [ 1 , 2 ] }`), nil
}

// pointerMarshaler encodes itself only from a pointer, which encoding/json
// calls only for a value that it can address.
type pointerMarshaler struct{ Text string }

func (*pointerMarshaler) MarshalJSON() ([]byte, error) { return []byte(`"by pointer"`), nil }

// textMarshaler encodes itself as text, also as the key of a map.
type textMarshaler struct{ Text string }

func (value textMarshaler) MarshalText() ([]byte, error) { return []byte("text:" + value.Text), nil }

// pointerTextMarshaler encodes itself as text only from a pointer.
type pointerTextMarshaler struct{ Text string }

func (*pointerTextMarshaler) MarshalText() ([]byte, error) { return []byte("by pointer"), nil }

// invalidMarshaler encodes itself to something that is no JSON.
type invalidMarshaler struct{}

func (invalidMarshaler) MarshalJSON() ([]byte, error) { return []byte(`{`), nil }

// zeroer says itself whether it is zero, which omitzero asks.
type zeroer struct{ Value int }

func (value zeroer) IsZero() bool { return value.Value < 0 }

// keyString is a string type, as the keys of a map may have.
type keyString string

// bytesType is a slice of bytes of a type of its own.
type bytesType []byte

// Shelf, Location, and hidden are embedded in embedding, along with Count,
// which is no struct.
type (
	Shelf struct {
		Name string `json:"name"`
		Row  int    `json:"row"`
	}

	Location struct {
		Room string `json:"room"`
		Row  int    `json:"row"`
	}

	hidden struct {
		Visible   string
		invisible string
	}

	Count int
)

type embedding struct {
	Shelf
	*Location
	hidden
	Count

	// Room shadows the field of Location, which is at a deeper level.
	Room string `json:"room"`

	// Tagged is embedded, but has a name of its own, so it is a field.
	Tagged Shelf `json:"tagged"`
}

// cycle points to itself, which encoding/json refuses once it has gone deep
// enough.
type cycle struct{ Next *cycle }

// pointerTo returns a pointer to the value.
func pointerTo[TValue any](value TValue) *TValue { return &value }

// encodedValues are values of every kind that encoding/json has rules for, and
// none of them holds a nil slice or a nil map.
func encodedValues() []struct {
	label string
	value any
} {
	looping := &cycle{}
	looping.Next = looping

	return []struct {
		label string
		value any
	}{
		{"nothing", nil},
		{"a string with what HTML escapes", `<script>alert("&")</script>`},
		{"a string with line separators", "  and  "},
		{"a string with invalid UTF-8", "a\xffb\xc0"},
		{"a string with control characters", "\x00\x01\t\n\r\x1f\x7f"},
		{"a string with quotes and backslashes", `"quoted" \ back\slashed`},
		{"a string beyond ASCII", "Ä € 😀"},
		{"integers", []any{int8(math.MinInt8), int16(-1), int32(0), int64(math.MaxInt64), uint8(255), uint64(math.MaxUint64), uintptr(7)}},
		{"floats", []any{0.0, math.Copysign(0, -1), 1e20, 1e21, 1e-6, 1e-7, 123456789.123, math.MaxFloat64, math.SmallestNonzeroFloat64,
			float32(3.14), float32(1e21), float32(1e-7), float32(math.MaxFloat32)}},
		{"booleans", []bool{true, false}},
		{"a nil pointer", (*int)(nil)},
		{"a pointer to a pointer", pointerTo(pointerTo(42))},
		{"an empty slice", []int{}},
		{"an empty map", map[string]int{}},
		{"a map with keys out of order", map[string]int{"b": 1, "a": 2, "A": 3, "ä": 4, "": 5}},
		{"a map with integer keys", map[int]string{10: "ten", 2: "two", -1: "minus one"}},
		{"a map with unsigned keys", map[uint8]bool{2: true, 1: false}},
		{"a map with keys that encode as text", map[textMarshaler]int{{Text: "b"}: 1, {Text: "a"}: 2}},
		{"a map with keys of a string type", map[keyString]int{"b": 1, "a": 2}},
		{"a map with keys that HTML escapes", map[string]int{"<&>": 1}},
		{"values nested in interfaces", map[string]any{"list": []any{1, "two", nil, true, 2.5, []string{"x"}, map[string]any{"y": []any{}}}}},
		{"an error, which has no exported fields", errors.New("an error")},
		{"an empty slice of bytes", []byte{}},
		{"a slice of bytes", []byte("hello <world>")},
		{"a slice of bytes of a type of its own", bytesType("abc")},
		{"slices of bytes in a slice", [][]byte{[]byte("a"), {}}},
		{"an array of bytes", [3]byte{1, 2, 3}},
		{"an array", [2]int{1, 2}},
		{"an empty array", [0]int{}},
		{"a time", time.Date(2026, 10, 5, 12, 0, 0, 123456789, time.FixedZone("CEST", 2*60*60))},
		{"a time in UTC", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{"the zero time", time.Time{}},
		{"a duration", 90 * time.Second},
		{"a json.Number", json.Number("12.50")},
		{"an empty json.Number", json.Number("")},
		{"a json.RawMessage", json.RawMessage(`{ "a" : [ 1, 2 ] , "html" : "<&>" }`)},
		{"a value that encodes itself", valueMarshaler{}},
		{"a value that encodes itself only from a pointer", pointerMarshaler{Text: "as a struct"}},
		{"a pointer to a value that encodes itself from a pointer", &pointerMarshaler{}},
		{"a struct that holds a value that encodes itself only from a pointer", struct{ Field pointerMarshaler }{}},
		{"a pointer to a struct that holds a value that encodes itself from a pointer", &struct{ Field pointerMarshaler }{}},
		{"a value that encodes itself as text", textMarshaler{Text: "x"}},
		{"a value that encodes itself as text only from a pointer", pointerTextMarshaler{Text: "as a struct"}},
		{"a pointer to a value that encodes itself as text from a pointer", &pointerTextMarshaler{}},
		{"a big integer", big.NewInt(0).Lsh(big.NewInt(1), 100)},
		{"an address", netip.MustParseAddr("2001:db8::1")},
		{"a URL", url.URL{Scheme: "https", Host: "example.com", Path: "/a b", RawQuery: "x=<y>"}},
		{"empty fields with omitempty", struct {
			String    string         `json:"string,omitempty"`
			Int       int            `json:"int,omitempty"`
			Float     float64        `json:"float,omitempty"`
			Bool      bool           `json:"bool,omitempty"`
			Pointer   *int           `json:"pointer,omitempty"`
			Interface any            `json:"interface,omitempty"`
			Slice     []int          `json:"slice,omitempty"`
			Map       map[string]int `json:"map,omitempty"`
			Bytes     []byte         `json:"bytes,omitempty"`
			Array     [0]int         `json:"array,omitempty"`
			Struct    struct{}       `json:"struct,omitempty"`
			Time      time.Time      `json:"time,omitempty"`
			Zero      *int           `json:"zero,omitempty"`
		}{Slice: []int{}, Map: map[string]int{}, Bytes: []byte{}, Zero: pointerTo(0)}},
		{"fields with omitzero", struct {
			Time       time.Time       `json:"time,omitzero"`
			Struct     struct{ A int } `json:"struct,omitzero"`
			Zero       zeroer          `json:"zero,omitzero"`
			NotZero    zeroer          `json:"notZero,omitzero"`
			Int        int             `json:"int,omitzero"`
			EmptySlice []int           `json:"emptySlice,omitzero"`
		}{Zero: zeroer{Value: -1}, EmptySlice: []int{}}},
		{"fields with the option string", struct {
			Int        int     `json:"int,string"`
			Uint       uint8   `json:"uint,string"`
			Float      float64 `json:"float,string"`
			Bool       bool    `json:"bool,string"`
			String     string  `json:"string,string"`
			Pointer    *int    `json:"pointer,string"`
			NilPointer *int    `json:"nilPointer,string"`
			//lint:ignore SA5008 the test is about encoding/json ignoring the option here
			Slice  []int       `json:"slice,string"`
			Number json.Number `json:"number,string"`
		}{Int: -1, Uint: 2, Float: 1.5, Bool: true, String: "<s>", Pointer: pointerTo(3), Slice: []int{4}, Number: "5"}},
		{"fields with names of their own, and fields that are left out", struct {
			Renamed string `json:"renamed"`
			Ignored string `json:"-"`
			//lint:ignore SA5008 the test is about a field whose name is a dash
			Dash        string `json:"-,"`
			unexported  string
			Untagged    string
			OnlyOptions string `json:",omitempty"`
			Special     string `json:"a-b.c"`
			//lint:ignore SA5008 the test is about exactly this invalid name
			Invalid string `json:"a\"b"`
		}{"renamed", "ignored", "dash", "unexported", "untagged", "only options", "special", "invalid"}},
		{"embedded structs", embedding{Shelf: Shelf{Name: "A", Row: 1}, hidden: hidden{Visible: "seen", invisible: "unseen"},
			Count: 3, Room: "reading room", Tagged: Shelf{Name: "B"}}},
		{"embedded structs with a pointer", embedding{Location: &Location{Room: "hidden", Row: 2}}},
		{"NaN", math.NaN()},
		{"infinity", math.Inf(-1)},
		{"a channel", make(chan int)},
		{"a function", func() {}},
		{"a complex number", complex(1, 2)},
		{"a map with keys that are floats", map[float64]int{1.5: 1}},
		{"a json.Number that is no number", json.Number("many")},
		{"a json.RawMessage that is no JSON", json.RawMessage(`{`)},
		{"an empty json.RawMessage", json.RawMessage{}},
		{"a value that encodes itself to something that is no JSON", invalidMarshaler{}},
		{"a value whose encoding fails with a category", unencodable{}},
		{"a cycle", looping},
	}
}

func TestAnswersAreEncodedAsByEncodingJSON(t *testing.T) {
	// Apart from nil slices and maps, an answer is byte for byte what an
	// Encoder of encoding/json writes, and a value that it refuses is
	// answered with 500, and logged with the error of encoding/json.
	for _, test := range encodedValues() {
		t.Run(test.label, func(t *testing.T) {
			var want bytes.Buffer
			failure := json.NewEncoder(&want).Encode(test.value)

			response, logs := respondedWith(test.value)

			if failure != nil {
				assert.Equal(t, http.StatusInternalServerError, response.Code)
				assert.Contains(t, logs, "error="+strconv.Quote("httpapi: encoding the result: "+failure.Error()))
				return
			}

			require.Equal(t, http.StatusOK, response.Code, logs)
			assert.Equal(t, want.String(), response.Body.String())
		})
	}
}

func TestFieldsOfAddingAreEncodedAsByEncodingJSON(t *testing.T) {
	// The fields go through a map on their way into the answer, so the answer
	// that encoding/json gives is that of the map that it decodes them into,
	// with numbers kept as they are, and the revision added.
	for _, test := range encodedValues() {
		encoded, err := json.Marshal(test.value)
		if err != nil {
			continue
		}

		var fields map[string]any
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()

		// Only a value that encodes to a JSON object holds fields.
		if decoder.Decode(&fields) != nil || fields == nil {
			continue
		}

		t.Run(test.label, func(t *testing.T) {
			fields["revision"] = "0"

			var want bytes.Buffer
			require.NoError(t, json.NewEncoder(&want).Encode(fields))

			mux := routed(httpapi.NewAPI(writingStore(t), userFrom),
				httpapi.Adding(func(httpapi.Handled[note]) (any, error) { return test.value, nil }))

			response := postNote(t, mux, `{"id":"1","text":"hello"}`)

			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Equal(t, want.String(), response.Body.String())
		})
	}
}
