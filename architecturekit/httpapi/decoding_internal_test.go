package httpapi

import (
	"encoding/json"
	"encoding/json/jsontext"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

func TestIsQuoted(t *testing.T) {
	_ = order{}.secret
	_ = hiddenCasing{}.quantity

	for _, test := range []struct {
		valueType reflect.Type
		path      string
		isQuoted  bool
	}{
		{reflect.TypeFor[order](), "quantity", true},
		{reflect.TypeFor[order](), "QUANTITY", true},
		{reflect.TypeFor[order](), "count", false},
		{reflect.TypeFor[*order](), "quantity", true},
		{reflect.TypeFor[order](), "code", true},
		{reflect.TypeFor[order](), "delivery.floor", true},
		{reflect.TypeFor[order](), "delivery.street", false},
		{reflect.TypeFor[order](), "billing.street", false},
		{reflect.TypeFor[order](), "counts.0", false},
		{reflect.TypeFor[order](), "floors.ground", false},
		{reflect.TypeFor[order](), "addresses.1.floor", true},
		{reflect.TypeFor[order](), "byName.home.floor", true},
		{reflect.TypeFor[order](), "Untagged", false},
		{reflect.TypeFor[order](), "unknown", false},
		{reflect.TypeFor[order](), "Skipped", false},
		{reflect.TypeFor[order](), "-", true},
		{reflect.TypeFor[order](), "secret", false},
		{reflect.TypeFor[order](), "voucher", false},
		{reflect.TypeFor[order](), "voucher.code", false},
		{reflect.TypeFor[order](), "percent", true},
		{reflect.TypeFor[order](), "Discount.percent", false},
		{reflect.TypeFor[order](), "quantity.more", false},
		{reflect.TypeFor[[]order](), "0.quantity", true},
		{reflect.TypeFor[casings](), "name", true},
		{reflect.TypeFor[casings](), "NAME", false},
		{reflect.TypeFor[casings](), "Name", true},
		{reflect.TypeFor[hiddenCasing](), "quantity", true},
	} {
		t.Run(test.valueType.String()+" "+test.path, func(t *testing.T) {
			assert.Equal(t, test.isQuoted, isQuoted(test.valueType, test.path))
		})
	}
}
