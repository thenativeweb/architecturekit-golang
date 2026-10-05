package httpapi

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isbn decodes itself, so it may take any kind of JSON value.
type isbn string

func (value *isbn) UnmarshalJSON(data []byte) error {
	*value = isbn(data)
	return nil
}

// shelf decodes itself from a reader of JSON text, so it may take any kind of
// JSON value as well.
type shelf struct{}

func (*shelf) UnmarshalJSONFrom(decoder *jsontext.Decoder) error {
	return decoder.SkipValue()
}

// genre decodes itself from text, which JSON holds as a string.
type genre struct{ name string }

func (value *genre) UnmarshalText(text []byte) error {
	value.name = string(text)
	return nil
}

// rating decodes itself from text, with a method of the value, not of a
// pointer to it.
type rating int

func (rating) UnmarshalText([]byte) error { return nil }

// edition decodes itself from JSON and from text, and the former counts.
type edition struct{}

func (*edition) UnmarshalJSON([]byte) error { return nil }

func (*edition) UnmarshalText([]byte) error { return nil }

type title string

func TestKindOf(t *testing.T) {
	for _, test := range []struct {
		value   any
		kind    string
		isKnown bool
	}{
		{value: "", kind: kindString, isKnown: true},
		{value: title(""), kind: kindString, isKnown: true},
		{value: false, kind: kindBoolean, isKnown: true},
		{value: 0, kind: kindNumber, isKnown: true},
		{value: int8(0), kind: kindNumber, isKnown: true},
		{value: int16(0), kind: kindNumber, isKnown: true},
		{value: int32(0), kind: kindNumber, isKnown: true},
		{value: int64(0), kind: kindNumber, isKnown: true},
		{value: uint(0), kind: kindNumber, isKnown: true},
		{value: uint8(0), kind: kindNumber, isKnown: true},
		{value: uint16(0), kind: kindNumber, isKnown: true},
		{value: uint32(0), kind: kindNumber, isKnown: true},
		{value: uint64(0), kind: kindNumber, isKnown: true},
		{value: uintptr(0), kind: kindNumber, isKnown: true},
		{value: float32(0), kind: kindNumber, isKnown: true},
		{value: float64(0), kind: kindNumber, isKnown: true},
		{value: time.Duration(0), kind: kindNumber, isKnown: true},
		{value: new(int), kind: kindNumber, isKnown: true},
		{value: new(*string), kind: kindString, isKnown: true},
		{value: []string{}, kind: kindArray, isKnown: true},
		{value: []byte{}, kind: kindString, isKnown: true},
		{value: [2]byte{}, kind: kindArray, isKnown: true},
		{value: [3]int{}, kind: kindArray, isKnown: true},
		{value: map[string]int{}, kind: kindObject, isKnown: true},
		{value: map[int]string{}, kind: kindObject, isKnown: true},
		{value: struct{}{}, kind: kindObject, isKnown: true},
		{value: time.Time{}, kind: kindString, isKnown: true},
		{value: new(time.Time), kind: kindString, isKnown: true},
		{value: json.Number(""), kind: kindNumber, isKnown: true},
		{value: genre{}, kind: kindString, isKnown: true},
		{value: rating(0), kind: kindString, isKnown: true},
		{value: isbn(""), isKnown: false},
		{value: shelf{}, isKnown: false},
		{value: edition{}, isKnown: false},
		{value: json.RawMessage{}, isKnown: false},
		{value: complex128(0), isKnown: false},
		{value: make(chan int), isKnown: false},
		{value: func() {}, isKnown: false},
	} {
		valueType := reflect.TypeOf(test.value)

		t.Run(valueType.String(), func(t *testing.T) {
			kind, isKnown := kindOf(valueType)

			assert.Equal(t, test.kind, kind)
			assert.Equal(t, test.isKnown, isKnown)
		})
	}

	t.Run("any", func(t *testing.T) {
		kind, isKnown := kindOf(reflect.TypeFor[any]())

		assert.Empty(t, kind)
		assert.False(t, isKnown)
	})
}

// The types for isQuoted and fieldNamed: an order whose quantity is a number
// in a string, as are the code of its voucher, which an embedded struct that
// is not exported holds, and the percentage of its discount, which an
// exported one holds.

type voucher struct {
	Code int `json:"code,string"`
}

type Discount struct {
	Percent int `json:"percent,string"`
}

type address struct {
	Street string `json:"street"`
	Floor  int    `json:"floor,string"`
}

type order struct {
	voucher
	Discount

	Quantity int      `json:"quantity,omitempty,string"`
	Count    int      `json:"count,omitempty"`
	Delivery *address `json:"delivery"`
	//lint:ignore SA5008 the test is about the option not applying to what the field holds
	Billing address `json:"billing,string"`
	//lint:ignore SA5008 the test is about the option not applying to what the field holds
	Counts []int `json:"counts,string"`
	//lint:ignore SA5008 the test is about the option not applying to what the field holds
	Floors    map[string]int     `json:"floors,string"`
	Addresses []address          `json:"addresses"`
	ByName    map[string]address `json:"byName"`
	Skipped   int                `json:"-"`
	//lint:ignore SA5008 the test is about a field whose name is a dash
	Dash     int `json:"-,string"`
	Untagged int
	secret   int
}

// casings has two fields whose names differ in case only, of which only one
// takes its number in a string.
type casings struct {
	Lower int `json:"name,string"`
	Upper int `json:"NAME"`
}

// hiddenCasing has a field that is not exported, which encoding/json does not
// decode into, although its name is the very one.
type hiddenCasing struct {
	quantity int
	Quantity int `json:"QUANTITY,string"`
}

// dotted has fields whose names hold a dot, one of which looks like the path
// to the floor of its delivery.
type dotted struct {
	Delivery    address `json:"delivery"`
	IsUpstairs  bool    `json:"delivery.floor"`
	ShelfRow    int     `json:"shelf.row,string"`
	EmptyQuoted int     `json:",string"`
}

func TestIsQuoted(t *testing.T) {
	_ = order{}.secret
	_ = hiddenCasing{}.quantity

	for _, test := range []struct {
		valueType reflect.Type
		pointer   jsontext.Pointer
		isQuoted  bool
	}{
		{reflect.TypeFor[order](), "/quantity", true},
		{reflect.TypeFor[order](), "/QUANTITY", true},
		{reflect.TypeFor[order](), "/count", false},
		{reflect.TypeFor[*order](), "/quantity", true},
		{reflect.TypeFor[order](), "/code", true},
		{reflect.TypeFor[order](), "/delivery/floor", true},
		{reflect.TypeFor[order](), "/delivery/street", false},
		{reflect.TypeFor[order](), "/billing/street", false},
		{reflect.TypeFor[order](), "/counts/0", false},
		{reflect.TypeFor[order](), "/floors/ground", false},
		{reflect.TypeFor[order](), "/addresses/1/floor", true},
		{reflect.TypeFor[order](), "/byName/home/floor", true},
		{reflect.TypeFor[order](), "/byName/home.floor/floor", true},
		{reflect.TypeFor[order](), "/byName//floor", true},
		{reflect.TypeFor[order](), "/Untagged", false},
		{reflect.TypeFor[order](), "/unknown", false},
		{reflect.TypeFor[order](), "/Skipped", false},
		{reflect.TypeFor[order](), "/-", true},
		{reflect.TypeFor[order](), "/secret", false},
		{reflect.TypeFor[order](), "/voucher", false},
		{reflect.TypeFor[order](), "/voucher/code", false},
		{reflect.TypeFor[order](), "/percent", true},
		{reflect.TypeFor[order](), "/Discount/percent", false},
		{reflect.TypeFor[order](), "/quantity/more", false},
		{reflect.TypeFor[order](), "", false},
		{reflect.TypeFor[order](), "/", false},
		{reflect.TypeFor[[]order](), "/0/quantity", true},
		{reflect.TypeFor[casings](), "/name", true},
		{reflect.TypeFor[casings](), "/NAME", false},
		{reflect.TypeFor[casings](), "/Name", true},
		{reflect.TypeFor[hiddenCasing](), "/quantity", true},
		{reflect.TypeFor[dotted](), "/delivery.floor", false},
		{reflect.TypeFor[dotted](), "/delivery/floor", true},
		{reflect.TypeFor[dotted](), "/shelf.row", true},
		{reflect.TypeFor[dotted](), "/shelf/row", false},
		{reflect.TypeFor[dotted](), "/EmptyQuoted", true},
	} {
		t.Run(test.valueType.String()+" "+string(test.pointer), func(t *testing.T) {
			assert.Equal(t, test.isQuoted, isQuoted(test.valueType, test.pointer))
		})
	}
}

func TestSubjectOf(t *testing.T) {
	for _, test := range []struct {
		pointer jsontext.Pointer
		subject string
	}{
		{pointer: "", subject: "the body"},
		{pointer: "/", subject: `""`},
		{pointer: "//", subject: `"."`},
		{pointer: "/quantity", subject: `"quantity"`},
		{pointer: "/items/0/bookId", subject: `"items.0.bookId"`},
		{pointer: "/shelf.row", subject: `"shelf.row"`},
		{pointer: "/a~1b/c~0d", subject: `"a/b.c~d"`},
	} {
		t.Run(string(test.pointer), func(t *testing.T) {
			assert.Equal(t, test.subject, subjectOf(test.pointer))
		})
	}
}

func TestIsNameAt(t *testing.T) {
	body := []byte(`{"count" : 1,"items":["a"]}`)

	for _, test := range []struct {
		label  string
		offset int64
		isName bool
	}{
		{label: "a name", offset: 1, isName: true},
		{label: "the inside of a name", offset: 2, isName: false},
		{label: "a value", offset: 11, isName: false},
		{label: "a string in a list", offset: 22, isName: false},
		{label: "a colon", offset: 9, isName: false},
		{label: "the end of an object", offset: 26, isName: false},
		{label: "the end of the body", offset: 27, isName: false},
		{label: "beyond the body", offset: 100, isName: false},
	} {
		t.Run(test.label, func(t *testing.T) {
			assert.Equal(t, test.isName, isNameAt(body, test.offset))
		})
	}
}

// duplicateAt returns the error of encoding/json for a name that occurs twice
// in JSON text of its own, the second time at the given offset, as a type that
// decodes itself might fail with.
func duplicateAt(t *testing.T, offset int) error {
	t.Helper()

	text := `{"a":1,` + strings.Repeat(" ", offset-8) + `"a":2}`
	err := jsonv2.Unmarshal([]byte(text), new(map[string]int), strictJSON)

	duplicate, isDuplicate := errors.AsType[*json.SyntaxError](err)
	require.True(t, isDuplicate)
	require.Equal(t, int64(offset), duplicate.Offset)
	require.EqualError(t, duplicate, jsontext.ErrDuplicateName.Error())

	return err
}

func TestDescribeDecoding(t *testing.T) {
	// encoding/json/v2 reports the same failure as encoding/json for every
	// body, so these failures are made up, to show that the description
	// does not rely on it.
	bodyType := reflect.TypeFor[order]()

	t.Run("names a value by the path of encoding/json, if encoding/json/v2 reports another failure", func(t *testing.T) {
		mismatch := &json.UnmarshalTypeError{Value: "string", Type: reflect.TypeFor[int](), Field: "count", Offset: 16}

		err := describeDecoding(bodyType, []byte(`{"count":"three"}`), mismatch, errors.New("another failure"))

		assert.EqualError(t, err, `"count" must be a number`)
		assert.ErrorIs(t, err, mismatch)
	})

	t.Run("names a value by the path of encoding/json, if encoding/json/v2 reports a failure without a type", func(t *testing.T) {
		// encoding/json/v2 takes a failure that a type that decodes itself
		// reports with an error of encoding/json/v2 of its own, which may lack
		// the type, as the failure of the type it names.
		mismatch := &json.UnmarshalTypeError{Value: "string", Type: reflect.TypeFor[int](), Field: "count", Offset: 16}

		err := describeDecoding(bodyType, []byte(`{"count":"three"}`), mismatch, &jsonv2.SemanticError{Err: mismatch})

		assert.EqualError(t, err, `"count" must be a number`)
	})

	t.Run("names a value by the path of encoding/json, if encoding/json/v2 reports a failure of another type", func(t *testing.T) {
		mismatch := &json.UnmarshalTypeError{Value: "string", Type: reflect.TypeFor[int](), Field: "count", Offset: 16}
		failure := &jsonv2.SemanticError{GoType: reflect.TypeFor[string](), JSONPointer: "/customerId"}

		err := describeDecoding(bodyType, []byte(`{"count":"three"}`), mismatch, failure)

		assert.EqualError(t, err, `"count" must be a number`)
	})

	t.Run("follows the path of encoding/json, if encoding/json/v2 reports another failure", func(t *testing.T) {
		// The floor of the delivery takes its number in a string.
		mismatch := &json.UnmarshalTypeError{Value: "bool", Type: reflect.TypeFor[int](), Field: "delivery.floor", Offset: 27}

		err := describeDecoding(bodyType, []byte(`{"delivery":{"floor":true}}`), mismatch, errors.New("another failure"))

		assert.EqualError(t, err, `"delivery.floor" must be a string that holds a number`)
	})

	t.Run("unescapes the names in the path of encoding/json, if encoding/json/v2 reports another failure", func(t *testing.T) {
		// encoding/json writes the path as a JSON pointer, with dots for
		// slashes, so a slash in a name stays escaped, as does a tilde.
		mismatch := &json.UnmarshalTypeError{Value: "string", Type: reflect.TypeFor[int](), Field: "floors.a~1b~0c", Offset: 22}

		err := describeDecoding(bodyType, []byte(`{"floors":{"a/b~c":"x"}}`), mismatch, errors.New("another failure"))

		assert.EqualError(t, err, `"floors.a/b~c" must be a number`)
	})

	t.Run("says that the body can not be decoded for a value without a path, if encoding/json/v2 reports another failure", func(t *testing.T) {
		mismatch := &json.UnmarshalTypeError{Value: "bool", Type: reflect.TypeFor[json.Number]()}

		err := describeDecoding(bodyType, []byte(`{"count":true}`), mismatch, errors.New("another failure"))

		assert.EqualError(t, err, "the body can not be decoded")
		assert.ErrorIs(t, err, mismatch)
	})

	t.Run("says that the key of a map can not be decoded, if encoding/json/v2 reports another failure", func(t *testing.T) {
		// encoding/json takes the key for a number that it could not read.
		mismatch := &json.UnmarshalTypeError{Value: "number x", Type: reflect.TypeFor[int](), Field: "stock.x", Offset: 14}

		err := describeDecoding(bodyType, []byte(`{"stock":{"x":1}}`), mismatch, errors.New("another failure"))

		assert.EqualError(t, err, `"stock.x" can not be decoded`)
		assert.ErrorIs(t, err, mismatch)
	})

	t.Run("says that the body can not be decoded for an error that encoding/json/v2 does not point to", func(t *testing.T) {
		failure := errors.New("a failure")

		for label, detailed := range map[string]error{
			"without a failure":    nil,
			"with another failure": errors.New("another failure"),
		} {
			t.Run(label, func(t *testing.T) {
				err := describeDecoding(bodyType, []byte(`{"count":1}`), failure, detailed)

				assert.EqualError(t, err, "the body can not be decoded")
				assert.ErrorIs(t, err, failure)
			})
		}
	})

	t.Run("keeps an error of a type that decodes itself, which encoding/json/v2 points to", func(t *testing.T) {
		failure := errors.New("a failure of its own")

		err := describeDecoding(bodyType, []byte(`{"isbn":"42"}`), failure, &jsonv2.SemanticError{GoType: reflect.TypeFor[isbn](), JSONPointer: "/isbn", Err: failure})

		assert.Same(t, failure, err)
	})

	t.Run("says that the body can not be decoded for an error of the decoder about a time", func(t *testing.T) {
		// A time decodes itself, but is no type of the application.
		failure := errors.New("a failure of a time")

		err := describeDecoding(bodyType, []byte(`{"dueOn":"x"}`), failure, &jsonv2.SemanticError{GoType: reflect.TypeFor[time.Time](), JSONPointer: "/dueOn", Err: failure})

		assert.EqualError(t, err, "the body can not be decoded")
	})

	t.Run("keeps a name that occurs twice where no name is in the body", func(t *testing.T) {
		// A type that decodes itself may fail with such an error, which points
		// into its own JSON text rather than into the body.
		for label, offset := range map[string]int{
			"at a colon":      8,
			"beyond the body": 100,
		} {
			t.Run(label, func(t *testing.T) {
				duplicate := duplicateAt(t, offset)

				err := describeDecoding(bodyType, []byte(`{"count":1}`), duplicate, nil)

				assert.Same(t, duplicate, err)
			})
		}
	})

	t.Run("names a name that occurs twice only where it repeats an earlier name of its object, regardless of case", func(t *testing.T) {
		for _, test := range []struct {
			label  string
			body   string
			offset int
			name   string
		}{
			{label: "in another case", body: `{"count":1,"COUNT":2}`, offset: 11, name: "COUNT"},
			{label: "in a nested object", body: `{"delivery":{"street":"a","STREET":"b"}}`, offset: 26, name: "STREET"},
			{label: "in an object after a nested one", body: `{"count":1,"delivery":{"street":"a"},"COUNT":2}`, offset: 37, name: "COUNT"},
			{label: "once", body: `{"count":1,"quantity":"2"}`, offset: 11},
			{label: "once in its object, after a nested one that holds it", body: `{"delivery":{"street":"a"},"STREET":1}`, offset: 27},
			{label: "once in its object, inside one that holds it", body: `{"street":"a","delivery":{"STREET":"b"}}`, offset: 26},
			{label: "once in an object in a list, after another one that holds it", body: `[{"count":1},{"COUNT":2}]`, offset: 14},
			{label: "as a value", body: `{"count":1,"x":"count"}`, offset: 15},
			{label: "as an item of a list", body: `{"count":1,"x":["a","b","COUNT"]}`, offset: 24},
		} {
			t.Run(test.label, func(t *testing.T) {
				duplicate := duplicateAt(t, test.offset)

				err := describeDecoding(bodyType, []byte(test.body), duplicate, nil)

				if test.name == "" {
					assert.Same(t, duplicate, err)
					return
				}

				assert.EqualError(t, err, `duplicate field "`+test.name+`"`)
				assert.ErrorIs(t, err, duplicate)
			})
		}
	})

	t.Run("keeps a name that occurs twice in the JSON text of a type that decodes itself", func(t *testing.T) {
		// The offset points to a name that occurs twice in another case, which
		// encoding/json refuses for a struct, but would take for a map. The
		// type failed with an error at the same offset, so the error is its.
		body := []byte(`{"count":1,"COUNT":2}`)

		for _, test := range []struct {
			label  string
			failed error
		}{
			{label: "of encoding/json", failed: duplicateAt(t, 11)},
			{label: "of encoding/json/jsontext", failed: &jsontext.SyntacticError{ByteOffset: 11, Err: jsontext.ErrDuplicateName}},
		} {
			t.Run(test.label, func(t *testing.T) {
				duplicate := duplicateAt(t, 11)
				failure := &jsonv2.SemanticError{GoType: reflect.TypeFor[isbn](), Err: test.failed}

				err := describeDecoding(bodyType, body, duplicate, failure)

				assert.Same(t, duplicate, err)
			})
		}

		// An error of the type elsewhere, or one of a type that does not decode
		// itself, says nothing about the name, which is then named.
		for _, test := range []struct {
			label   string
			failure *jsonv2.SemanticError
		}{
			{label: "at another offset", failure: &jsonv2.SemanticError{GoType: reflect.TypeFor[isbn](), Err: duplicateAt(t, 12)}},
			{label: "at another offset, of encoding/json/jsontext",
				failure: &jsonv2.SemanticError{GoType: reflect.TypeFor[isbn](), Err: &jsontext.SyntacticError{ByteOffset: 12, Err: jsontext.ErrDuplicateName}}},
			{label: "of another kind", failure: &jsonv2.SemanticError{GoType: reflect.TypeFor[isbn](), Err: errors.New("a failure of its own")}},
			{label: "of a type that does not decode itself", failure: &jsonv2.SemanticError{GoType: reflect.TypeFor[int](), Err: errors.New("a failure")}},
		} {
			t.Run(test.label, func(t *testing.T) {
				err := describeDecoding(bodyType, body, duplicateAt(t, 11), test.failure)

				assert.EqualError(t, err, `duplicate field "COUNT"`)
			})
		}
	})

	t.Run("keeps a syntax error of a type that decodes itself, wherever it points to", func(t *testing.T) {
		// The offset points to a name that occurs twice in another case.
		text := `{"a":1,` + strings.Repeat(" ", 3) + `x}`
		failure := jsonv2.Unmarshal([]byte(text), new(map[string]int), strictJSON)

		invalid, isSyntax := errors.AsType[*json.SyntaxError](failure)
		require.True(t, isSyntax)
		require.Equal(t, int64(11), invalid.Offset)

		err := describeDecoding(bodyType, []byte(`{"count":1,"COUNT":2}`), failure, nil)

		assert.Same(t, failure, err)
	})

	t.Run("keeps a name that occurs twice, if encoding/json/v2 says so without pointing to it", func(t *testing.T) {
		duplicate := duplicateAt(t, 11)

		err := describeDecoding(bodyType, []byte(`{"count":1,"COUNT":2}`), duplicate, jsontext.ErrDuplicateName)

		assert.Same(t, duplicate, err)
	})

	t.Run("keeps a value that does not fit in the JSON text of a type that decodes itself", func(t *testing.T) {
		// The path is in the JSON text of the type, here the path of a field of
		// the body that takes its number in a string.
		mismatch := &json.UnmarshalTypeError{Value: "string", Type: reflect.TypeFor[int](), Field: "quantity", Offset: 12}

		for _, goType := range []reflect.Type{reflect.TypeFor[isbn](), reflect.TypeFor[shelf](), reflect.TypeFor[genre]()} {
			t.Run(goType.String(), func(t *testing.T) {
				failure := &jsonv2.SemanticError{GoType: goType, JSONPointer: "/isbn", Err: mismatch}

				err := describeDecoding(bodyType, []byte(`{"isbn":{"quantity":"x"}}`), mismatch, failure)

				assert.Same(t, mismatch, err)
			})
		}
	})
}
