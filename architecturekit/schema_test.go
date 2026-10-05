package architecturekit_test

import (
	"context"
	"encoding"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// country constrains a string with a Schema function of its own.
type country string

func (country) Schema() map[string]any {
	return map[string]any{"type": "string", "pattern": "^[A-Z]{2}$"}
}

// level encodes itself as text, with a value receiver.
type level int

func (l level) MarshalText() ([]byte, error) { return []byte(itoa(int(l))), nil }

// day encodes itself as JSON, and says so with a Schema function.
type day struct{}

func (day) MarshalJSON() ([]byte, error) { return []byte(`"2026-09-30"`), nil }

func (day) Schema() map[string]any {
	return map[string]any{"type": "string", "pattern": `^\d{4}-\d{2}-\d{2}$`}
}

// secret encodes itself as JSON, without a Schema function.
type secret struct{}

func (secret) MarshalJSON() ([]byte, error) { return []byte(`"***"`), nil }

// handle encodes itself as text, but with a pointer receiver only.
// appended writes itself as text with AppendText, which encoding/json calls
// just like MarshalText.
type appended struct{ value int }

func (a appended) AppendText(b []byte) ([]byte, error) {
	return strconv.AppendInt(b, int64(a.value), 10), nil
}

// appendedOnPointer does so only with a pointer receiver.
type appendedOnPointer struct{ value int }

func (a *appendedOnPointer) AppendText(b []byte) ([]byte, error) {
	return strconv.AppendInt(b, int64(a.value), 10), nil
}

// encodedTo writes its JSON itself with MarshalJSONTo of encoding/json/v2,
// which encoding/json calls as well.
type encodedTo struct{ value int }

func (e encodedTo) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteToken(jsontext.Int(int64(e.value)))
}

// encodedToOnPointer does so only with a pointer receiver.
type encodedToOnPointer struct{ value int }

func (e *encodedToOnPointer) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteToken(jsontext.Int(int64(e.value)))
}

type handle struct{ value string }

func (h *handle) MarshalText() ([]byte, error) { return []byte(h.value), nil }

// code has a Schema function with a pointer receiver.
type code string

func (*code) Schema() map[string]any {
	return map[string]any{"type": "string", "pattern": "^[a-z]+$"}
}

// tagList encodes itself as text, although it is a slice.
type tagList []string

func (l tagList) MarshalText() ([]byte, error) { return []byte(strings.Join(l, ",")), nil }

// rating is an unexported type that is not a struct.
type rating int

// chain refers to itself.
type chain struct {
	Next *chain `json:"next"`
}

// withNilSchema has a Schema function that returns nil.
type withNilSchema string

func (withNilSchema) Schema() map[string]any { return nil }

type address struct {
	City string `json:"city"`
}

type audit struct {
	By string `json:"by"`
}

type byText struct {
	By string
}

type byNumber struct {
	By int
}

type taggedName struct {
	Name string `json:"Name"`
}

type untaggedName struct {
	Name int
}

// schemaJSON derives the schema of T and encodes it, so that it can be
// compared with the schema it must be.
func schemaJSON[T any](t *testing.T) string {
	t.Helper()

	encoded, err := json.Marshal(architecturekit.DeriveSchema[T]())
	require.NoError(t, err)

	return string(encoded)
}

func TestDeriveSchema(t *testing.T) {
	tests := []struct {
		name string
		got  func(t *testing.T) string
		want string
	}{
		{"describes a struct as an object with its fields", schemaJSON[struct {
			Title string `json:"title"`
			Pages int    `json:"pages"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"title": {"type": "string"}, "pages": {"type": "integer"}},
			"required": ["title", "pages"]}`},

		{"describes the basic types", schemaJSON[struct {
			Flag   bool    `json:"flag"`
			Small  int8    `json:"small"`
			Big    uint64  `json:"big"`
			Ratio  float32 `json:"ratio"`
			Amount float64 `json:"amount"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"flag": {"type": "boolean"}, "small": {"type": "integer"}, "big": {"type": "integer"},
				"ratio": {"type": "number"}, "amount": {"type": "number"}},
			"required": ["flag", "small", "big", "ratio", "amount"]}`},

		{"names a field without a tag like the field", schemaJSON[struct {
			Title string
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"Title": {"type": "string"}}, "required": ["Title"]}`},

		{"leaves out fields tagged with a dash, and unexported ones", schemaJSON[struct {
			Title    string `json:"title"`
			Internal string `json:"-"`
			hidden   string
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"title": {"type": "string"}}, "required": ["title"]}`},

		{"makes fields with omitempty or omitzero optional", schemaJSON[struct {
			Title    string `json:"title"`
			Subtitle string `json:"subtitle,omitempty"`
			Edition  int    `json:"edition,omitzero"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"title": {"type": "string"}, "subtitle": {"type": "string"}, "edition": {"type": "integer"}},
			"required": ["title"]}`},

		{"leaves out required for an object without required fields", schemaJSON[struct {
			Subtitle string `json:"subtitle,omitempty"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"subtitle": {"type": "string"}}}`},

		{"describes an empty struct as an object without fields", schemaJSON[struct{}],
			`{"type": "object", "additionalProperties": false, "properties": {}}`},

		{"makes a field with the string option a string", schemaJSON[struct {
			Pages    int   `json:"pages,string"`
			Edition  *int  `json:"edition,string"`
			Optional *int  `json:"optional,string,omitempty"`
			Level    level `json:"level,string"`
			//lint:ignore SA5008 the test is about encoding/json ignoring the string option here
			Tags  []int   `json:"tags,string"`
			Ratio float64 `json:"ratio,string"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"pages": {"type": "string"},
				"edition": {"anyOf": [{"type": "string"}, {"type": "null"}]},
				"optional": {"type": "string"},
				"level": {"type": "string"},
				"tags": {"anyOf": [{"type": "array", "items": {"type": "integer"}}, {"type": "null"}]},
				"ratio": {"type": "string"}},
			"required": ["pages", "edition", "level", "tags", "ratio"]}`},

		{"describes a slice as an array, which may be null", schemaJSON[struct {
			Tags []string `json:"tags"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"tags": {"anyOf": [{"type": "array", "items": {"type": "string"}}, {"type": "null"}]}},
			"required": ["tags"]}`},

		{"never allows null for an optional slice, a map or a pointer", schemaJSON[struct {
			Tags   []string       `json:"tags,omitempty"`
			Counts map[string]int `json:"counts,omitzero"`
			Note   *string        `json:"note,omitempty"`
			Data   []byte         `json:"data,omitempty"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"tags": {"type": "array", "items": {"type": "string"}},
				"counts": {"type": "object", "additionalProperties": {"type": "integer"}},
				"note": {"type": "string"},
				"data": {"type": "string"}}}`},

		{"describes a []byte as a string, which may be null", schemaJSON[struct {
			Data []byte `json:"data"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"data": {"anyOf": [{"type": "string"}, {"type": "null"}]}}, "required": ["data"]}`},

		{"describes an array as an array of exactly its length", schemaJSON[struct {
			Scores [3]int `json:"scores"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"scores": {"type": "array", "items": {"type": "integer"}, "minItems": 3, "maxItems": 3}},
			"required": ["scores"]}`},

		{"describes a map as an object, which may be null", schemaJSON[struct {
			Counts map[int]string `json:"counts"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"counts": {"anyOf": [{"type": "object", "additionalProperties": {"type": "string"}}, {"type": "null"}]}},
			"required": ["counts"]}`},

		{"describes a pointer as what it points to, which may be null", schemaJSON[struct {
			Note **string `json:"note"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"note": {"anyOf": [{"anyOf": [{"type": "string"}, {"type": "null"}]}, {"type": "null"}]}},
			"required": ["note"]}`},

		{"describes a time as a date-time string", schemaJSON[struct {
			At      time.Time  `json:"at"`
			Until   *time.Time `json:"until"`
			Checked *time.Time `json:"checked,omitempty"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"at": {"type": "string", "format": "date-time"},
				"until": {"anyOf": [{"type": "string", "format": "date-time"}, {"type": "null"}]},
				"checked": {"type": "string", "format": "date-time"}},
			"required": ["at", "until"]}`},

		{"describes a type with MarshalText as a string", schemaJSON[struct {
			Level  level           `json:"level"`
			Levels map[level]level `json:"levels,omitempty"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"level": {"type": "string"},
				"levels": {"type": "object", "additionalProperties": {"type": "string"}}},
			"required": ["level"]}`},

		{"describes a json.Number as a number, and as a string with the string option", schemaJSON[struct {
			Amount  json.Number            `json:"amount"`
			Quoted  json.Number            `json:"quoted,string"`
			Pointed *json.Number           `json:"pointed,string"`
			Maybe   *json.Number           `json:"maybe"`
			Many    []json.Number          `json:"many,omitempty"`
			ByValue map[json.Number]string `json:"byValue,omitempty"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"amount": {"type": "number"},
				"quoted": {"type": "string"},
				"pointed": {"anyOf": [{"type": "string"}, {"type": "null"}]},
				"maybe": {"anyOf": [{"type": "number"}, {"type": "null"}]},
				"many": {"type": "array", "items": {"type": "number"}},
				"byValue": {"type": "object", "additionalProperties": {"type": "string"}}},
			"required": ["amount", "quoted", "pointed", "maybe"]}`},

		{"describes a type with AppendText as a string", schemaJSON[struct {
			Count  appended              `json:"count"`
			Counts map[appended]appended `json:"counts,omitempty"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"count": {"type": "string"},
				"counts": {"type": "object", "additionalProperties": {"type": "string"}}},
			"required": ["count"]}`},

		{"allows any value for an interface", schemaJSON[struct {
			Payload any `json:"payload"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"payload": {}}, "required": ["payload"]}`},

		{"allows any value for an interface with a Schema function", schemaJSON[struct {
			Shape shape      `json:"shape"`
			Named namedShape `json:"named"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"shape": {}, "named": {}}, "required": ["shape", "named"]}`},

		{"describes a field type by its own Schema function", schemaJSON[struct {
			Country country   `json:"country"`
			Visited []country `json:"visited,omitempty"`
			Home    *country  `json:"home"`
			Due     day       `json:"due"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"country": {"type": "string", "pattern": "^[A-Z]{2}$"},
				"visited": {"type": "array", "items": {"type": "string", "pattern": "^[A-Z]{2}$"}},
				"home": {"anyOf": [{"type": "string", "pattern": "^[A-Z]{2}$"}, {"type": "null"}]},
				"due": {"type": "string", "pattern": "^\\d{4}-\\d{2}-\\d{2}$"}},
			"required": ["country", "home", "due"]}`},

		{"describes a field type by a Schema function with a pointer receiver", schemaJSON[struct {
			Code code `json:"code"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"code": {"type": "string", "pattern": "^[a-z]+$"}}, "required": ["code"]}`},

		{"describes a slice that encodes itself as text as a string, which is never null", schemaJSON[struct {
			Tags     tagList `json:"tags"`
			Optional tagList `json:"optional,omitempty"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"tags": {"type": "string"}, "optional": {"type": "string"}}, "required": ["tags"]}`},

		{"leaves out an embedded unexported type that is not a struct", schemaJSON[struct {
			rating
			Title string `json:"title"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"title": {"type": "string"}}, "required": ["title"]}`},

		{"does not call the own Schema function of the type itself", schemaJSON[country],
			`{"type": "string"}`},

		{"describes a nested struct as an object", schemaJSON[struct {
			Address address `json:"address"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"address": {"type": "object", "additionalProperties": false,
				"properties": {"city": {"type": "string"}}, "required": ["city"]}},
			"required": ["address"]}`},

		{"lifts the fields of an embedded struct", schemaJSON[struct {
			audit
			Title string `json:"title"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"by": {"type": "string"}, "title": {"type": "string"}},
			"required": ["by", "title"]}`},

		{"makes the fields of an embedded pointer optional", schemaJSON[struct {
			*audit
			Title string `json:"title"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"by": {"type": "string"}, "title": {"type": "string"}},
			"required": ["title"]}`},

		{"keeps an embedded struct with a tag as a field", schemaJSON[struct {
			audit `json:"audit"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"audit": {"type": "object", "additionalProperties": false,
				"properties": {"by": {"type": "string"}}, "required": ["by"]}},
			"required": ["audit"]}`},

		{"lets the least deeply embedded field win", schemaJSON[struct {
			audit
			By int `json:"by"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"by": {"type": "integer"}}, "required": ["by"]}`},

		{"lets the only tagged field win among equally deep ones", schemaJSON[struct {
			untaggedName
			taggedName
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"Name": {"type": "string"}}, "required": ["Name"]}`},

		{"leaves out a name that equally deep fields share", schemaJSON[struct {
			byText
			byNumber
			Title string `json:"title"`
		}], `{"type": "object", "additionalProperties": false,
			"properties": {"title": {"type": "string"}}, "required": ["title"]}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.JSONEq(t, test.want, test.got(t))
		})
	}

	t.Run("leaves out the fields of a struct embedded twice at the same depth", func(t *testing.T) {
		type first struct{ audit }
		type second struct{ audit }

		assert.JSONEq(t, `{"type": "object", "additionalProperties": false, "properties": {}}`,
			schemaJSON[struct {
				first
				second
			}](t))
	})

	t.Run("returns a schema that belongs to the caller", func(t *testing.T) {
		schema := architecturekit.DeriveSchema[address]()
		schema["properties"].(map[string]any)["city"] = "changed"

		assert.JSONEq(t, `{"type": "object", "additionalProperties": false,
			"properties": {"city": {"type": "string"}}, "required": ["city"]}`, schemaJSON[address](t))
	})

	t.Run("returns the shape encoding/json decodes into", func(t *testing.T) {
		schema := architecturekit.DeriveSchema[struct {
			Scores [2]int `json:"scores"`
		}]()

		scores := schema["properties"].(map[string]any)["scores"].(map[string]any)
		assert.IsType(t, float64(0), scores["minItems"])
		assert.IsType(t, []any{}, schema["required"])
	})
}

func TestDeriveSchemaPanics(t *testing.T) {
	tests := []struct {
		name   string
		derive func()
		want   string
	}{
		{"for a type that encodes itself as JSON without a Schema function",
			func() { architecturekit.DeriveSchema[struct{ Secret secret }]() },
			"architecturekit_test.secret encodes itself with MarshalJSON"},
		{"for a type that encodes itself as text with a pointer receiver only",
			func() { architecturekit.DeriveSchema[struct{ Handle handle }]() },
			"architecturekit_test.handle encodes itself with MarshalText or AppendText on a pointer only"},
		{"for a type that encodes itself as text with AppendText on a pointer only",
			func() { architecturekit.DeriveSchema[struct{ Count appendedOnPointer }]() },
			"architecturekit_test.appendedOnPointer encodes itself with MarshalText or AppendText on a pointer only"},
		{"for a type that encodes itself with MarshalJSONTo",
			func() { architecturekit.DeriveSchema[struct{ Value encodedTo }]() },
			"architecturekit_test.encodedTo encodes itself with MarshalJSON or MarshalJSONTo"},
		{"for a type that encodes itself with MarshalJSONTo on a pointer",
			func() { architecturekit.DeriveSchema[struct{ Value encodedToOnPointer }]() },
			"architecturekit_test.encodedToOnPointer encodes itself with MarshalJSON or MarshalJSONTo"},
		{"for an interface that encodes itself as JSON",
			func() { architecturekit.DeriveSchema[struct{ Value json.Marshaler }]() },
			"json.Marshaler encodes itself with MarshalJSON or MarshalJSONTo"},
		{"for an interface that encodes itself with MarshalJSONTo",
			func() { architecturekit.DeriveSchema[struct{ Value jsonv2.MarshalerTo }]() },
			"json.MarshalerTo encodes itself with MarshalJSON or MarshalJSONTo"},
		{"for an interface with a Schema function that encodes itself as JSON, as for one without",
			func() { architecturekit.DeriveSchema[struct{ Value jsonShape }]() },
			"architecturekit_test.jsonShape encodes itself with MarshalJSON or MarshalJSONTo"},
		{"for an interface with a Schema function that encodes itself with MarshalJSONTo, as for one without",
			func() { architecturekit.DeriveSchema[struct{ Value jsonToShape }]() },
			"architecturekit_test.jsonToShape encodes itself with MarshalJSON or MarshalJSONTo"},
		{"for a slice of a type that can not be derived",
			func() { architecturekit.DeriveSchema[struct{ Secrets []secret }]() },
			"architecturekit_test.secret encodes itself with MarshalJSON"},
		{"for an optional slice of a type that can not be derived",
			func() {
				architecturekit.DeriveSchema[struct {
					Secrets []secret `json:"secrets,omitempty"`
				}]()
			},
			"architecturekit_test.secret encodes itself with MarshalJSON"},
		{"for an array of a type that can not be derived",
			func() { architecturekit.DeriveSchema[struct{ Secrets [2]secret }]() },
			"architecturekit_test.secret encodes itself with MarshalJSON"},
		{"for a map of a type that can not be derived",
			func() { architecturekit.DeriveSchema[struct{ Secrets map[string]secret }]() },
			"architecturekit_test.secret encodes itself with MarshalJSON"},
		{"for a recursive type",
			func() { architecturekit.DeriveSchema[chain]() },
			"architecturekit_test.chain is recursive"},
		{"for a channel",
			func() { architecturekit.DeriveSchema[struct{ Updates chan int }]() },
			"chan int can not be written by encoding/json"},
		{"for a function",
			func() { architecturekit.DeriveSchema[struct{ Callback func() }]() },
			"func() can not be written by encoding/json"},
		{"for a complex number",
			func() { architecturekit.DeriveSchema[struct{ Value complex128 }]() },
			"complex128 can not be written by encoding/json"},
		{"for a map whose keys encoding/json can not write",
			func() { architecturekit.DeriveSchema[struct{ Index map[address]int }]() },
			"has keys encoding/json can not write"},
		{"for an invalid json tag name",
			func() {
				architecturekit.DeriveSchema[struct {
					//lint:ignore SA5008 the test is about exactly this invalid name
					Title string `json:"a\"b"`
				}]()
			},
			"has the json tag name \"a\\\"b\", which encoding/json reads differently depending on the Go version"},
		{"for a Schema function that returns nil",
			func() { architecturekit.DeriveSchema[struct{ Value withNilSchema }]() },
			"the Schema function of architecturekit_test.withNilSchema returns nil"},
		{"for a Schema function whose schema can not be encoded",
			func() { architecturekit.DeriveSchema[struct{ Value unencodableSchema }]() },
			"can not be encoded as JSON"},
		{"for a type with the Schema function of an embedded struct only",
			func() { architecturekit.DeriveSchema[feePaid]() },
			"architecturekit_test.feePaid has a Schema function only from its embedded field money, which describes money alone"},
		{"for a pointer to such a type",
			func() { architecturekit.DeriveSchema[*feePaid]() },
			"architecturekit_test.feePaid has a Schema function only from its embedded field money"},
		{"for a type with the Schema function of an embedded struct on a pointer receiver",
			func() {
				architecturekit.DeriveSchema[struct {
					deposit
					Note string `json:"note"`
				}]()
			},
			"has a Schema function only from its embedded field deposit"},
		{"for a type that embeds a pointer to a struct with a Schema function",
			func() {
				architecturekit.DeriveSchema[struct {
					*money
					Note string `json:"note"`
				}]()
			},
			"has a Schema function only from its embedded field money"},
		{"for a type with the Schema function of a struct embedded two levels deep",
			func() {
				architecturekit.DeriveSchema[struct {
					charge
					Note string `json:"note"`
				}]()
			},
			"has a Schema function only from its embedded field charge.money, which describes charge.money alone"},
		{"for a type that embeds a struct whose Schema function hides the one of its own embedded field",
			func() {
				architecturekit.DeriveSchema[struct {
					feePaidWithSchema
				}]()
			},
			"has a Schema function only from its embedded field feePaidWithSchema,"},
		{"for a type that embeds a struct with a Schema function under a json tag",
			func() {
				architecturekit.DeriveSchema[struct {
					money `json:"fee"`
				}]()
			},
			"has a Schema function only from its embedded field money"},
		{"for a type with the Schema function of an embedded type that is not a struct",
			func() {
				architecturekit.DeriveSchema[struct {
					country
					Note string `json:"note"`
				}]()
			},
			"has a Schema function only from its embedded field country"},
		{"for a type with the Schema function of an embedded type that is not a struct, on a pointer receiver",
			func() {
				architecturekit.DeriveSchema[struct {
					code
					Note string `json:"note"`
				}]()
			},
			"has a Schema function only from its embedded field code"},
		{"for a type with the Schema function of an embedded interface",
			func() {
				architecturekit.DeriveSchema[struct {
					describer
					Note string `json:"note"`
				}]()
			},
			"has a Schema function only from its embedded field describer"},
		{"for a type with the Schema function of an embedded interface that declares further functions",
			func() { architecturekit.DeriveSchema[shapeFramed]() },
			"architecturekit_test.shapeFramed has a Schema function only from its embedded field shape, which describes shape alone"},
		{"for a field whose type has the Schema function of an embedded interface only",
			func() { architecturekit.DeriveSchema[struct{ Framed shapeFramed }]() },
			"field Framed of struct { Framed architecturekit_test.shapeFramed }: " +
				"architecturekit_test.shapeFramed has a Schema function only from its embedded field shape"},
		{"for a generic type with the Schema function of an embedded struct only",
			func() { architecturekit.DeriveSchema[priced[string]]() },
			"architecturekit_test.priced[string] has a Schema function only from its embedded field money"},
		{"for a type with the Schema function of an embedded instance of a generic type",
			func() {
				architecturekit.DeriveSchema[struct {
					labeled[int]
					Note string `json:"note"`
				}]()
			},
			"has a Schema function only from its embedded field labeled"},
		{"for a field whose type has the Schema function of an embedded struct only",
			func() { architecturekit.DeriveSchema[struct{ Fee feePaid }]() },
			"field Fee of struct { Fee architecturekit_test.feePaid }: architecturekit_test.feePaid has a Schema function only from its embedded field money"},
		{"for an optional field whose type has the Schema function of an embedded struct only",
			func() {
				architecturekit.DeriveSchema[struct {
					Fee feePaid `json:"fee,omitempty"`
				}]()
			},
			"architecturekit_test.feePaid has a Schema function only from its embedded field money"},
		{"for a field that points to such a type",
			func() { architecturekit.DeriveSchema[struct{ Fee *feePaid }]() },
			"architecturekit_test.feePaid has a Schema function only from its embedded field money"},
		{"for a slice of such a type",
			func() { architecturekit.DeriveSchema[struct{ Fees []feePaid }]() },
			"architecturekit_test.feePaid has a Schema function only from its embedded field money"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message := panicMessage(t, test.derive)
			assert.Contains(t, message, test.want)
			assert.Contains(t, message, "architecturekit: ")
		})
	}

	t.Run("names the field that can not be derived", func(t *testing.T) {
		message := panicMessage(t, func() { architecturekit.DeriveSchema[struct{ Secret secret }]() })
		assert.Contains(t, message, "field Secret")
	})

	t.Run("points to a Schema function where one helps", func(t *testing.T) {
		message := panicMessage(t, func() { architecturekit.DeriveSchema[chain]() })
		assert.Contains(t, message, "give it a Schema function")
	})

	t.Run("says how to resolve a Schema function of an embedded field", func(t *testing.T) {
		message := panicMessage(t, func() { architecturekit.DeriveSchema[feePaid]() })
		assert.Contains(t, message,
			"give architecturekit_test.feePaid a Schema function of its own, or make money a named field")
	})

	t.Run("names the least deeply embedded field with a Schema function, as Go does", func(t *testing.T) {
		// code declares its Schema function one level below the struct, money
		// two levels below, so Go promotes the one of code.
		message := panicMessage(t, func() {
			architecturekit.DeriveSchema[struct {
				charge
				code
			}]()
		})
		assert.Contains(t, message, "only from its embedded field code,")
	})

	t.Run("names the embedded field, not one of an embedded type without a Schema function or a named one", func(t *testing.T) {
		message := panicMessage(t, func() {
			architecturekit.DeriveSchema[struct {
				audit
				Country country `json:"country"`
				money
			}]()
		})
		assert.Contains(t, message, "only from its embedded field money,")
	})
}

// unencodableSchema has a Schema function whose schema holds a function.
type unencodableSchema string

func (unencodableSchema) Schema() map[string]any { return map[string]any{"const": func() {}} }

// panicMessage runs the given function, which has to panic with a string, and
// returns that string.
func panicMessage(t *testing.T, run func()) (message string) {
	t.Helper()

	defer func() {
		recovered := recover()
		require.NotNil(t, recovered, "expected a panic")

		text, isText := recovered.(string)
		require.True(t, isText, "expected a panic with a string, got %T", recovered)
		message = text
	}()

	run()

	return ""
}

// noted has no Schema function, so its schema is derived.
type noted struct {
	Text    string    `json:"text"`
	Tags    []string  `json:"tags"`
	Country country   `json:"country,omitempty"`
	At      time.Time `json:"at"`
}

func (noted) EventType() string { return "io.thenativeweb.test.noted" }

// unregistrable has a field whose schema can not be derived.
type unregistrable struct {
	Secret secret `json:"secret"`
}

func (unregistrable) EventType() string { return "io.thenativeweb.test.unregistrable" }

func notedState() *architecturekit.State[[]string] {
	return architecturekit.NewState([]string{}).
		Evolve(func(texts []string, event noted) []string { return append(texts, event.Text) })
}

func TestEvolveWithDerivedSchemas(t *testing.T) {
	t.Run("derives the schema of an event without a Schema function", func(t *testing.T) {
		schemas := notedState().Schemas()

		require.Len(t, schemas, 1)
		assert.Equal(t, "io.thenativeweb.test.noted", schemas[0].EventType)

		encoded, err := json.Marshal(schemas[0].Schema)
		require.NoError(t, err)
		assert.JSONEq(t, schemaJSON[noted](t), string(encoded))
	})

	t.Run("keeps the own schema of an event with a Schema function", func(t *testing.T) {
		schemas := counterState().Schemas()

		for _, schema := range schemas {
			if schema.EventType == (incremented{}).EventType() {
				assert.Equal(t, incremented{}.Schema(), schema.Schema)
				return
			}
		}
		assert.Fail(t, "expected the schema of the incremented event")
	})

	t.Run("panics for an event whose schema can not be derived", func(t *testing.T) {
		message := panicMessage(t, func() {
			architecturekit.NewState(0).Evolve(func(count int, _ unregistrable) int { return count + 1 })
		})

		assert.Contains(t, message, `event type "io.thenativeweb.test.unregistrable"`)
		assert.Contains(t, message, "encodes itself with MarshalJSON")
	})

	t.Run("is accepted and checked by the database", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		require.NoError(t, architecturekit.RegisterSchemas(context.Background(), store, notedState().Schemas()))

		// A nil slice is written as null, which the derived schema allows.
		writeRaw(t, subject, noted{Text: "first", Tags: nil, At: time.Now()})
		writeRaw(t, subject, noted{Text: "second", Tags: []string{"urgent"}, Country: "DE", At: time.Now()})

		texts, err := architecturekit.Load(t.Context(), store, notedState(), subject)
		require.NoError(t, err)
		assert.Equal(t, []string{"first", "second"}, texts)

		write := func(data map[string]any) error {
			_, err := rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{{
				Source:  "https://thenativeweb.io",
				Subject: subject,
				Type:    noted{}.EventType(),
				Data:    data,
			}}, nil)
			return err
		}

		// Every refused event differs from this one in one way only.
		valid := func() map[string]any {
			return map[string]any{"text": "third", "tags": nil, "at": "2026-09-30T12:00:00Z", "country": "DE"}
		}
		require.NoError(t, write(valid()), "the database must accept the event every refused one is based on")

		for name, change := range map[string]func(data map[string]any){
			"an unknown field":        func(data map[string]any) { data["unknown"] = true },
			"a missing field":         func(data map[string]any) { delete(data, "text") },
			"a field of a wrong type": func(data map[string]any) { data["text"] = 3 },
			"a broken constraint":     func(data map[string]any) { data["country"] = "Germany" },
		} {
			data := valid()
			change(data)

			assert.ErrorContains(t, write(data), "schema", "the database must refuse %s because of the schema", name)
		}
	})
}

func TestSchemaOf(t *testing.T) {
	t.Run("returns the own schema of an event with a Schema function", func(t *testing.T) {
		assert.Equal(t, architecturekit.EventSchema{
			EventType: "io.thenativeweb.test.incremented",
			Schema:    incremented{}.Schema(),
		}, architecturekit.SchemaOf[incremented]())
	})

	t.Run("derives the schema of an event without a Schema function", func(t *testing.T) {
		assert.Equal(t, architecturekit.EventSchema{
			EventType: "io.thenativeweb.test.noted",
			Schema:    architecturekit.DeriveSchema[noted](),
		}, architecturekit.SchemaOf[noted]())
	})

	t.Run("returns what Evolve and Ignore put into Schemas", func(t *testing.T) {
		for name, testCase := range map[string]struct {
			schema architecturekit.EventSchema
			state  *architecturekit.State[int]
		}{
			"an own schema, by Evolve": {
				architecturekit.SchemaOf[incremented](),
				architecturekit.NewState(0).Evolve(func(count int, _ incremented) int { return count + 1 }),
			},
			"an own schema, by Ignore": {
				architecturekit.SchemaOf[incremented](),
				architecturekit.NewState(0).Ignore[incremented](),
			},
			"a derived schema, by Evolve": {
				architecturekit.SchemaOf[noted](),
				architecturekit.NewState(0).Evolve(func(count int, _ noted) int { return count + 1 }),
			},
			"a derived schema, by Ignore": {
				architecturekit.SchemaOf[noted](),
				architecturekit.NewState(0).Ignore[noted](),
			},
			"an own schema that hides the one of an embedded field": {
				architecturekit.SchemaOf[feePaidWithSchema](),
				architecturekit.NewState(0).Evolve(func(count int, _ feePaidWithSchema) int { return count + 1 }),
			},
			"the own schema of a generic event": {
				architecturekit.SchemaOf[labeled[int]](),
				architecturekit.NewState(0).Evolve(func(count int, _ labeled[int]) int { return count + 1 }),
			},
		} {
			t.Run(name, func(t *testing.T) {
				assert.Equal(t, []architecturekit.EventSchema{testCase.schema}, testCase.state.Schemas())
			})
		}
	})

	t.Run("panics for an event with the Schema function of an embedded field only, as Evolve does", func(t *testing.T) {
		evolving := panicMessage(t, func() {
			architecturekit.NewState(0).Evolve(func(count int, _ feePaid) int { return count + 1 })
		})

		assert.Contains(t, evolving, `architecturekit: event type "io.thenativeweb.test.fee-paid": `+
			"architecturekit_test.feePaid has a Schema function only from its embedded field money")
		assert.PanicsWithValue(t, evolving, func() { architecturekit.SchemaOf[feePaid]() })
	})

	t.Run("panics for an event whose schema can not be derived, as Evolve does", func(t *testing.T) {
		evolving := panicMessage(t, func() {
			architecturekit.NewState(0).Evolve(func(count int, _ unregistrable) int { return count + 1 })
		})

		assert.Contains(t, evolving, `architecturekit: event type "io.thenativeweb.test.unregistrable": `)
		assert.PanicsWithValue(t, evolving, func() { architecturekit.SchemaOf[unregistrable]() })
	})

	t.Run("panics on a pointer as the event type, also if its EventType function has a pointer receiver", func(t *testing.T) {
		assert.PanicsWithValue(t,
			"architecturekit: SchemaOf needs the event type architecturekit_test.incremented, not the pointer *architecturekit_test.incremented",
			func() { architecturekit.SchemaOf[*incremented]() })
		assert.PanicsWithValue(t,
			"architecturekit: SchemaOf needs the event type architecturekit_test.pointed, not the pointer *architecturekit_test.pointed",
			func() { architecturekit.SchemaOf[*pointed]() })
	})

	t.Run("is accepted and checked by the database for an event that only Write writes", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		require.NoError(t, architecturekit.RegisterSchemas(t.Context(), store,
			[]architecturekit.EventSchema{architecturekit.SchemaOf[noted]()}))

		_, err := architecturekit.Write(t.Context(), store, []architecturekit.EventOn{
			{Subject: subject, Event: noted{Text: "first", Tags: []string{}, At: time.Now()}},
		}, architecturekit.Unconditionally())
		require.NoError(t, err)

		_, err = architecturekit.Write(t.Context(), store, []architecturekit.EventOn{
			{Subject: subject, Event: noted{Text: "second", Tags: []string{}, Country: "Germany", At: time.Now()}},
		}, architecturekit.Unconditionally())
		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "the database must refuse the event because of the schema")
	})
}

// money is a value type with constraints, which it describes with a Schema
// function of its own.
type money struct {
	Amount   int    `json:"amount"`
	Currency string `json:"currency"`
}

func (money) Schema() map[string]any {
	schema := architecturekit.DeriveSchema[money]()
	schema["properties"].(map[string]any)["currency"] = map[string]any{"type": "string", "pattern": "^[A-Z]{3}$"}

	return schema
}

// feePaid has a Schema function only because it embeds money, and that
// function describes money alone, while encoding/json writes the note next to
// the amount and the currency.
type feePaid struct {
	money
	Note string `json:"note"`
}

func (feePaid) EventType() string { return "io.thenativeweb.test.fee-paid" }

// feeCharged has a field of the type feePaid.
type feeCharged struct {
	Fee feePaid `json:"fee"`
}

func (feeCharged) EventType() string { return "io.thenativeweb.test.fee-charged" }

// feePaidWithSchema is feePaid with a Schema function of its own, which hides
// the one of money. It has the same event type.
type feePaidWithSchema struct {
	money
	Note string `json:"note"`
}

func (feePaidWithSchema) EventType() string { return "io.thenativeweb.test.fee-paid" }

func (feePaidWithSchema) Schema() map[string]any {
	schema := architecturekit.DeriveSchema[feePaidWithSchema]()
	schema["properties"].(map[string]any)["currency"] = money{}.Schema()["properties"].(map[string]any)["currency"]

	return schema
}

// feeWaived hides the Schema function of money with one on a pointer receiver.
type feeWaived struct {
	money
}

func (*feeWaived) Schema() map[string]any {
	return map[string]any{"type": "object", "maxProperties": 2}
}

// deposit has a Schema function on a pointer receiver.
type deposit struct {
	Amount int `json:"amount"`
}

func (*deposit) Schema() map[string]any { return map[string]any{"type": "object"} }

// charge has a Schema function only because it embeds money, and passes it on
// to whatever embeds it.
type charge struct {
	money
}

// describer is an interface with a Schema function.
type describer interface {
	Schema() map[string]any
}

// labeled is a generic event with a Schema function of its own.
type labeled[T any] struct {
	Label T `json:"label"`
}

func (labeled[T]) EventType() string { return "io.thenativeweb.test.labeled" }

func (labeled[T]) Schema() map[string]any {
	return map[string]any{"type": "object", "required": []any{"label"}}
}

// labeledOnPointer is a generic type with a Schema function on a pointer
// receiver.
type labeledOnPointer[T any] struct {
	Label T `json:"label"`
}

func (*labeledOnPointer[T]) Schema() map[string]any {
	return map[string]any{"type": "object", "maxProperties": 1}
}

// priced is a generic event that has a Schema function only because it embeds
// money.
type priced[T any] struct {
	money
	Item T `json:"item"`
}

func (priced[T]) EventType() string { return "io.thenativeweb.test.priced" }

// discounted is a generic type that hides the Schema function of money with
// one of its own.
type discounted[T any] struct {
	money
	By T `json:"by"`
}

func (discounted[T]) Schema() map[string]any {
	return map[string]any{"type": "object", "minProperties": 3}
}

func TestSchemaFunctionsOfEmbeddedFields(t *testing.T) {
	t.Run("panics in Evolve for an event with the Schema function of an embedded field only", func(t *testing.T) {
		message := panicMessage(t, func() {
			architecturekit.NewState(0).Evolve(func(count int, _ feePaid) int { return count + 1 })
		})

		assert.Contains(t, message, `architecturekit: event type "io.thenativeweb.test.fee-paid": `+
			"architecturekit_test.feePaid has a Schema function only from its embedded field money")
	})

	t.Run("panics in Ignore for such an event", func(t *testing.T) {
		message := panicMessage(t, func() {
			architecturekit.NewState(0).Ignore[feePaid]()
		})

		assert.Contains(t, message, "architecturekit_test.feePaid has a Schema function only from its embedded field money")
	})

	t.Run("panics for a generic event with the Schema function of an embedded field only", func(t *testing.T) {
		message := panicMessage(t, func() {
			architecturekit.NewState(0).Evolve(func(count int, _ priced[string]) int { return count + 1 })
		})

		assert.Contains(t, message, "architecturekit_test.priced[string] has a Schema function only from its embedded field money")
	})

	t.Run("panics for an event with a field whose type has the Schema function of an embedded field only", func(t *testing.T) {
		message := panicMessage(t, func() {
			architecturekit.NewState(0).Evolve(func(count int, _ feeCharged) int { return count + 1 })
		})

		assert.Contains(t, message, `architecturekit: event type "io.thenativeweb.test.fee-charged": `+
			"field Fee of architecturekit_test.feeCharged: "+
			"architecturekit_test.feePaid has a Schema function only from its embedded field money")
	})

	t.Run("keeps the own Schema function of an event that hides the one of an embedded field", func(t *testing.T) {
		schemas := architecturekit.NewState(0).
			Evolve(func(count int, _ feePaidWithSchema) int { return count + 1 }).
			Schemas()

		require.Len(t, schemas, 1)
		assert.Equal(t, feePaidWithSchema{}.Schema(), schemas[0].Schema)
	})

	t.Run("keeps the own Schema function of a generic event", func(t *testing.T) {
		schemas := architecturekit.NewState(0).
			Evolve(func(count int, _ labeled[int]) int { return count + 1 }).
			Schemas()

		require.Len(t, schemas, 1)
		assert.Equal(t, labeled[int]{}.Schema(), schemas[0].Schema)
	})

	t.Run("describes fields by Schema functions that hide those of embedded fields, also on pointer receivers and generic types", func(t *testing.T) {
		assert.JSONEq(t, `{"type": "object", "additionalProperties": false,
			"properties": {
				"withSchema": {"type": "object", "additionalProperties": false,
					"properties": {"amount": {"type": "integer"}, "currency": {"type": "string", "pattern": "^[A-Z]{3}$"},
						"note": {"type": "string"}},
					"required": ["amount", "currency", "note"]},
				"waived": {"type": "object", "maxProperties": 2},
				"discounted": {"type": "object", "minProperties": 3},
				"label": {"type": "object", "required": ["label"]},
				"labelOnPointer": {"type": "object", "maxProperties": 1}},
			"required": ["withSchema", "waived", "discounted", "label", "labelOnPointer"]}`,
			schemaJSON[struct {
				WithSchema     feePaidWithSchema        `json:"withSchema"`
				Waived         feeWaived                `json:"waived"`
				Discounted     discounted[int]          `json:"discounted"`
				Label          labeled[string]          `json:"label"`
				LabelOnPointer labeledOnPointer[string] `json:"labelOnPointer"`
			}](t))
	})

	t.Run("describes a named field by the Schema function of its type", func(t *testing.T) {
		assert.JSONEq(t, `{"type": "object", "additionalProperties": false,
			"properties": {
				"fee": {"type": "object", "additionalProperties": false,
					"properties": {"amount": {"type": "integer"}, "currency": {"type": "string", "pattern": "^[A-Z]{3}$"}},
					"required": ["amount", "currency"]},
				"note": {"type": "string"}},
			"required": ["fee", "note"]}`,
			schemaJSON[struct {
				Fee  money  `json:"fee"`
				Note string `json:"note"`
			}](t))
	})

	t.Run("describes the fields of an embedded struct by their types in a Schema function of its own", func(t *testing.T) {
		// DeriveSchema does not call the Schema function of money, so the
		// currency has no pattern unless the own Schema function adds it.
		assert.JSONEq(t, `{"type": "object", "additionalProperties": false,
			"properties": {"amount": {"type": "integer"}, "currency": {"type": "string"}, "note": {"type": "string"}},
			"required": ["amount", "currency", "note"]}`,
			schemaJSON[feePaidWithSchema](t))
	})

	t.Run("refuses before anything is registered and leaves the event type free", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		// Building the state panics, so RegisterSchemas is never reached.
		panicMessage(t, func() {
			state := architecturekit.NewState(0).Evolve(func(count int, _ feePaid) int { return count + 1 })
			_ = architecturekit.RegisterSchemas(t.Context(), store, state.Schemas())
		})

		// Had the schema of money been registered for the event type, the
		// schema of feePaidWithSchema would differ from it and be refused.
		state := architecturekit.NewState(0).Evolve(func(count int, _ feePaidWithSchema) int { return count + 1 })
		require.NoError(t, architecturekit.RegisterSchemas(t.Context(), store, state.Schemas()))

		writeRaw(t, subject, feePaidWithSchema{money: money{Amount: 5, Currency: "EUR"}, Note: "late"})

		count, err := architecturekit.Load(t.Context(), store, state, subject)
		require.NoError(t, err)
		assert.Equal(t, 1, count)

		_, err = rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{{
			Source:  "https://thenativeweb.io",
			Subject: subject,
			Type:    feePaidWithSchema{}.EventType(),
			Data:    feePaidWithSchema{money: money{Amount: 5, Currency: "euro"}, Note: "late"},
		}}, nil)
		assert.ErrorContains(t, err, "schema", "the database must check the currency against the pattern of money")
	})
}

// shape is an interface that declares a Schema function. encoding/json writes
// the value a field of the interface holds, or null, and every value has a
// Schema function that describes it alone.
type shape interface {
	Schema() map[string]any
	Area() float64
}

// square and circle are shapes that encoding/json writes as different objects.
type square struct {
	Side float64 `json:"side"`
}

func (square) Schema() map[string]any {
	return map[string]any{"type": "object", "required": []any{"side"}}
}

func (s square) Area() float64 { return s.Side * s.Side }

type circle struct {
	Radius float64 `json:"radius"`
}

func (circle) Schema() map[string]any {
	return map[string]any{"type": "object", "required": []any{"radius"}}
}

func (c circle) Area() float64 { return math.Pi * c.Radius * c.Radius }

// namedShape has its Schema function from an embedded interface, which, for
// an interface, is the same as declaring it.
type namedShape interface {
	shape
	Name() string
}

// textShape, appendedShape, jsonShape and jsonToShape are shapes that also
// encode themselves.
type textShape interface {
	shape
	encoding.TextMarshaler
}

type appendedShape interface {
	shape
	encoding.TextAppender
}

type jsonShape interface {
	shape
	json.Marshaler
}

type jsonToShape interface {
	shape
	jsonv2.MarshalerTo
}

// shapeDrawn has fields of an interface type with a Schema function.
type shapeDrawn struct {
	Shape  shape   `json:"shape"`
	Shapes []shape `json:"shapes"`
}

func (shapeDrawn) EventType() string { return "io.thenativeweb.test.shape-drawn" }

// shapeFramed has a Schema function only because it embeds shape.
type shapeFramed struct {
	shape
	Frame string `json:"frame"`
}

func (shapeFramed) EventType() string { return "io.thenativeweb.test.shape-framed" }

func TestInterfacesWithSchemaFunctions(t *testing.T) {
	t.Run("describes a field of such an interface as any value, wherever it appears", func(t *testing.T) {
		assert.JSONEq(t, `{"type": "object", "additionalProperties": false,
			"properties": {
				"shape": {},
				"optional": {},
				"shapes": {"anyOf": [{"type": "array", "items": {}}, {"type": "null"}]},
				"optionalShapes": {"type": "array", "items": {}},
				"fixed": {"type": "array", "items": {}, "minItems": 2, "maxItems": 2},
				"byName": {"anyOf": [{"type": "object", "additionalProperties": {}}, {"type": "null"}]},
				"pinned": {"anyOf": [{}, {"type": "null"}]},
				"optionalPinned": {}},
			"required": ["shape", "shapes", "fixed", "byName", "pinned"]}`,
			schemaJSON[struct {
				Shape          shape            `json:"shape"`
				Optional       shape            `json:"optional,omitempty"`
				Shapes         []shape          `json:"shapes"`
				OptionalShapes []shape          `json:"optionalShapes,omitempty"`
				Fixed          [2]shape         `json:"fixed"`
				ByName         map[string]shape `json:"byName"`
				Pinned         *shape           `json:"pinned"`
				OptionalPinned *shape           `json:"optionalPinned,omitempty"`
			}](t))
	})

	t.Run("describes it exactly as an interface without a Schema function", func(t *testing.T) {
		assert.JSONEq(t,
			schemaJSON[struct {
				Shape          any                    `json:"shape"`
				Named          any                    `json:"named"`
				Optional       any                    `json:"optional,omitempty"`
				Shapes         []any                  `json:"shapes"`
				Fixed          [2]any                 `json:"fixed"`
				ByName         map[string]any         `json:"byName"`
				Pinned         *any                   `json:"pinned"`
				OptionalPinned *any                   `json:"optionalPinned,omitempty"`
				Text           encoding.TextMarshaler `json:"text"`
				Appended       encoding.TextAppender  `json:"appended"`
			}](t),
			schemaJSON[struct {
				Shape          shape            `json:"shape"`
				Named          namedShape       `json:"named"`
				Optional       shape            `json:"optional,omitempty"`
				Shapes         []shape          `json:"shapes"`
				Fixed          [2]shape         `json:"fixed"`
				ByName         map[string]shape `json:"byName"`
				Pinned         *shape           `json:"pinned"`
				OptionalPinned *shape           `json:"optionalPinned,omitempty"`
				Text           textShape        `json:"text"`
				Appended       appendedShape    `json:"appended"`
			}](t))
	})

	t.Run("keeps describing an interface with MarshalText or AppendText as a string", func(t *testing.T) {
		// With a Schema function, such an interface is described the same way,
		// as the test above shows.
		assert.JSONEq(t, `{"type": "object", "additionalProperties": false,
			"properties": {"text": {"type": "string"}, "appended": {"type": "string"}},
			"required": ["text", "appended"]}`,
			schemaJSON[struct {
				Text     encoding.TextMarshaler `json:"text"`
				Appended encoding.TextAppender  `json:"appended"`
			}](t))
	})

	t.Run("describes such an interface itself as any value", func(t *testing.T) {
		assert.JSONEq(t, `{}`, schemaJSON[shape](t))
	})

	t.Run("derives the schema of an event with such a field in Evolve and in Ignore", func(t *testing.T) {
		want := `{"type": "object", "additionalProperties": false,
			"properties": {"shape": {}, "shapes": {"anyOf": [{"type": "array", "items": {}}, {"type": "null"}]}},
			"required": ["shape", "shapes"]}`

		for name, state := range map[string]func() *architecturekit.State[int]{
			"Evolve": func() *architecturekit.State[int] {
				return architecturekit.NewState(0).Evolve(func(count int, _ shapeDrawn) int { return count + 1 })
			},
			"Ignore": func() *architecturekit.State[int] {
				return architecturekit.NewState(0).Ignore[shapeDrawn]()
			},
		} {
			schemas := state().Schemas()
			require.Len(t, schemas, 1, name)

			encoded, err := json.Marshal(schemas[0].Schema)
			require.NoError(t, err)
			assert.JSONEq(t, want, string(encoded), name)
		}
	})

	t.Run("keeps refusing an event with the Schema function of an embedded interface only", func(t *testing.T) {
		message := panicMessage(t, func() {
			architecturekit.NewState(0).Evolve(func(count int, _ shapeFramed) int { return count + 1 })
		})

		assert.Contains(t, message, `architecturekit: event type "io.thenativeweb.test.shape-framed": `+
			"architecturekit_test.shapeFramed has a Schema function only from its embedded field shape")
	})

	t.Run("is accepted and checked by the database", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		state := architecturekit.NewState(0).Evolve(func(count int, _ shapeDrawn) int { return count + 1 })
		require.NoError(t, architecturekit.RegisterSchemas(t.Context(), store, state.Schemas()))

		// A square and a circle are written as different objects, each of which
		// the Schema function of the other one refuses.
		writeRaw(t, subject,
			shapeDrawn{Shape: square{Side: 2}, Shapes: []shape{circle{Radius: 1}, nil}},
			shapeDrawn{Shape: circle{Radius: 3}},
			shapeDrawn{})

		_, err := rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{{
			Source:  "https://thenativeweb.io",
			Subject: subject,
			Type:    shapeDrawn{}.EventType(),
			Data:    map[string]any{"shape": square{Side: 2}, "shapes": nil, "unknown": true},
		}}, nil)
		assert.ErrorContains(t, err, "schema", "the database must still check the fields of the event")
	})
}

// remarked has fields of type json.RawMessage, which hold JSON that
// encoding/json writes as it is.
type remarked struct {
	Note  json.RawMessage   `json:"note"`
	Extra json.RawMessage   `json:"extra,omitempty"`
	Notes []json.RawMessage `json:"notes"`
}

func (remarked) EventType() string { return "io.thenativeweb.test.remarked" }

func remarkedState() *architecturekit.State[[]json.RawMessage] {
	return architecturekit.NewState([]json.RawMessage{}).
		Evolve(func(notes []json.RawMessage, event remarked) []json.RawMessage { return append(notes, event.Note) })
}

// details is declared from json.RawMessage, but does not take over its
// MarshalJSON function, so encoding/json writes it as a []byte.
type details json.RawMessage

// rawEmbedded has the MarshalJSON function of the json.RawMessage it embeds,
// which writes the embedded value alone.
type rawEmbedded struct {
	json.RawMessage
	Note string `json:"note"`
}

// signature is a []byte like json.RawMessage, but its MarshalJSON function
// writes something other than the bytes it holds.
type signature []byte

func (signature) MarshalJSON() ([]byte, error) { return []byte(`"signed"`), nil }

func TestRawMessages(t *testing.T) {
	t.Run("describes a field of type json.RawMessage as any value, wherever it appears", func(t *testing.T) {
		assert.JSONEq(t, `{"type": "object", "additionalProperties": false,
			"properties": {
				"raw": {},
				"optional": {},
				"zero": {},
				"quoted": {},
				"raws": {"anyOf": [{"type": "array", "items": {}}, {"type": "null"}]},
				"optionalRaws": {"type": "array", "items": {}},
				"fixed": {"type": "array", "items": {}, "minItems": 2, "maxItems": 2},
				"byName": {"anyOf": [{"type": "object", "additionalProperties": {}}, {"type": "null"}]},
				"pinned": {"anyOf": [{}, {"type": "null"}]},
				"optionalPinned": {}},
			"required": ["raw", "quoted", "raws", "fixed", "byName", "pinned"]}`,
			schemaJSON[struct {
				Raw      json.RawMessage `json:"raw"`
				Optional json.RawMessage `json:"optional,omitempty"`
				Zero     json.RawMessage `json:"zero,omitzero"`
				//lint:ignore SA5008 the test is about encoding/json ignoring the string option here
				Quoted         json.RawMessage            `json:"quoted,string"`
				Raws           []json.RawMessage          `json:"raws"`
				OptionalRaws   []json.RawMessage          `json:"optionalRaws,omitempty"`
				Fixed          [2]json.RawMessage         `json:"fixed"`
				ByName         map[string]json.RawMessage `json:"byName"`
				Pinned         *json.RawMessage           `json:"pinned"`
				OptionalPinned *json.RawMessage           `json:"optionalPinned,omitempty"`
			}](t))
	})

	t.Run("describes it exactly as a field of an interface type", func(t *testing.T) {
		assert.JSONEq(t,
			schemaJSON[struct {
				Raw            any            `json:"raw"`
				Optional       any            `json:"optional,omitempty"`
				Raws           []any          `json:"raws"`
				ByName         map[string]any `json:"byName"`
				Pinned         *any           `json:"pinned"`
				OptionalPinned *any           `json:"optionalPinned,omitempty"`
			}](t),
			schemaJSON[struct {
				Raw            json.RawMessage            `json:"raw"`
				Optional       json.RawMessage            `json:"optional,omitempty"`
				Raws           []json.RawMessage          `json:"raws"`
				ByName         map[string]json.RawMessage `json:"byName"`
				Pinned         *json.RawMessage           `json:"pinned"`
				OptionalPinned *json.RawMessage           `json:"optionalPinned,omitempty"`
			}](t))
	})

	t.Run("describes jsontext.Value of encoding/json/v2 the same way, since it is the same type", func(t *testing.T) {
		assert.Equal(t, reflect.TypeFor[json.RawMessage](), reflect.TypeFor[jsontext.Value]())
		assert.JSONEq(t, `{"type": "object", "additionalProperties": false,
			"properties": {"value": {}}, "required": ["value"]}`,
			schemaJSON[struct {
				Value jsontext.Value `json:"value"`
			}](t))
	})

	t.Run("describes json.RawMessage itself as any value", func(t *testing.T) {
		assert.JSONEq(t, `{}`, schemaJSON[json.RawMessage](t))
	})

	t.Run("keeps describing a type declared from json.RawMessage as a []byte, since it has no MarshalJSON function", func(t *testing.T) {
		encoded, err := json.Marshal(details(`{"page": 42}`))
		require.NoError(t, err)

		var text string
		require.NoError(t, json.Unmarshal(encoded, &text), "encoding/json must write it as a string")

		assert.JSONEq(t, `{"type": "object", "additionalProperties": false,
			"properties": {"details": {"anyOf": [{"type": "string"}, {"type": "null"}]}},
			"required": ["details"]}`,
			schemaJSON[struct {
				Details details `json:"details"`
			}](t))
	})

	t.Run("keeps refusing a struct that embeds json.RawMessage, since its MarshalJSON function writes the struct", func(t *testing.T) {
		message := panicMessage(t, func() { architecturekit.DeriveSchema[struct{ Embedded rawEmbedded }]() })
		assert.Contains(t, message, "architecturekit_test.rawEmbedded encodes itself with MarshalJSON or MarshalJSONTo")
	})

	t.Run("keeps refusing another []byte that encodes itself with MarshalJSON", func(t *testing.T) {
		message := panicMessage(t, func() { architecturekit.DeriveSchema[struct{ Signature signature }]() })
		assert.Contains(t, message, "architecturekit_test.signature encodes itself with MarshalJSON or MarshalJSONTo")
	})

	t.Run("derives the schema of an event with such a field in Evolve and in Ignore", func(t *testing.T) {
		want := `{"type": "object", "additionalProperties": false,
			"properties": {"note": {}, "extra": {}, "notes": {"anyOf": [{"type": "array", "items": {}}, {"type": "null"}]}},
			"required": ["note", "notes"]}`

		for name, state := range map[string]func() *architecturekit.State[int]{
			"Evolve": func() *architecturekit.State[int] {
				return architecturekit.NewState(0).Evolve(func(count int, _ remarked) int { return count + 1 })
			},
			"Ignore": func() *architecturekit.State[int] {
				return architecturekit.NewState(0).Ignore[remarked]()
			},
		} {
			schemas := state().Schemas()
			require.Len(t, schemas, 1, name)

			encoded, err := json.Marshal(schemas[0].Schema)
			require.NoError(t, err)
			assert.JSONEq(t, want, string(encoded), name)
		}
	})

	t.Run("is accepted and checked by the database", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		require.NoError(t, architecturekit.RegisterSchemas(t.Context(), store, remarkedState().Schemas()))

		// encoding/json writes a raw object and a raw number as they are, and a
		// nil raw message as null.
		writeRaw(t, subject,
			remarked{Note: json.RawMessage(`{"page": 42, "text": "dog-eared"}`), Notes: []json.RawMessage{json.RawMessage(`1.5e3`), nil}},
			remarked{Note: json.RawMessage(`42`), Extra: json.RawMessage(`null`)},
			remarked{})

		notes, err := architecturekit.Load(t.Context(), store, remarkedState(), subject)
		require.NoError(t, err)
		require.Len(t, notes, 3)
		assert.JSONEq(t, `{"page": 42, "text": "dog-eared"}`, string(notes[0]))
		assert.JSONEq(t, `42`, string(notes[1]))
		assert.JSONEq(t, `null`, string(notes[2]))

		_, err = rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{{
			Source:  "https://thenativeweb.io",
			Subject: subject,
			Type:    remarked{}.EventType(),
			Data:    map[string]any{"note": 42, "notes": nil, "unknown": true},
		}}, nil)
		assert.ErrorContains(t, err, "schema", "the database must still check the fields of the event")
	})
}

// The following types are exported, so that the test that compares with
// encoding/json can fill in their fields through reflection.

type Audit struct {
	By string `json:"by"`
}

type Stamp struct {
	At   time.Time `json:"at"`
	Note string    `json:"note,omitempty"`
}

type NameTagged struct {
	Name string `json:"Name"`
}

type NameUntagged struct {
	Name int
}

type ByText struct{ By string }

type ByNumber struct{ By int }

type Plain struct{ Value string }

type Wrapper struct {
	Audit
	Extra string `json:"extra"`
}

type First struct{ Plain }

type Second struct{ Plain }

// agreesWithEncodingJSON checks the derived schema of a struct against what
// encoding/json actually writes: the fields it writes for the zero value have
// to be the required ones, the fields it writes for a filled value have to be
// all properties, and a field it writes as null for the zero value has to
// allow null.
func agreesWithEncodingJSON[T any](t *testing.T) {
	t.Helper()

	schema := architecturekit.DeriveSchema[T]()
	properties := schema["properties"].(map[string]any)

	var zero T
	written := writtenFields(t, zero)

	var required []string
	for _, name := range schema["required"].([]any) {
		required = append(required, name.(string))
	}
	assert.ElementsMatch(t, keysOf(written), required, "the required fields must be those written for the zero value")

	filled := reflect.New(reflect.TypeFor[T]()).Elem()
	fill(filled)
	assert.ElementsMatch(t, keysOf(writtenFields(t, filled.Interface())), keysOf(properties),
		"the properties must be the fields written for a filled value")

	for name, value := range written {
		if string(value) != "null" {
			continue
		}

		encoded, err := json.Marshal(properties[name])
		require.NoError(t, err)
		assert.Contains(t, string(encoded), `{"type":"null"}`, "field %q is written as null, so it must allow null", name)
	}
}

func writtenFields(t *testing.T, value any) map[string]json.RawMessage {
	t.Helper()

	encoded, err := json.Marshal(value)
	require.NoError(t, err)

	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &fields))

	return fields
}

func keysOf[TValue any](values map[string]TValue) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}

	return keys
}

// fill sets every field that can be set to a value that is not empty.
func fill(value reflect.Value) {
	if value.Type() == reflect.TypeFor[time.Time]() {
		value.Set(reflect.ValueOf(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)))
		return
	}

	switch value.Kind() {
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value.SetUint(1)
	case reflect.Float32, reflect.Float64:
		value.SetFloat(1.5)
	case reflect.String:
		value.SetString("x")
	case reflect.Pointer:
		pointed := reflect.New(value.Type().Elem())
		fill(pointed.Elem())
		value.Set(pointed)
	case reflect.Slice:
		slice := reflect.MakeSlice(value.Type(), 1, 1)
		fill(slice.Index(0))
		value.Set(slice)
	case reflect.Map:
		entries := reflect.MakeMap(value.Type())
		key := reflect.New(value.Type().Key()).Elem()
		item := reflect.New(value.Type().Elem()).Elem()
		fill(key)
		fill(item)
		entries.SetMapIndex(key, item)
		value.Set(entries)
	case reflect.Struct:
		for i := range value.NumField() {
			if value.Field(i).CanSet() {
				fill(value.Field(i))
			}
		}
	default:
	}
}

func TestDeriveSchemaAgreesWithEncodingJSON(t *testing.T) {
	t.Run("on names, tags and options", func(t *testing.T) {
		agreesWithEncodingJSON[struct {
			Audit
			Title    string `json:"title"`
			Subtitle string `json:"subtitle,omitempty"`
			Edition  int    `json:"edition,omitzero"`
			Internal string `json:"-"`
			Plain    string
			Hyphen   string `json:"content-type"`
			Special  string `json:"$ref"`
		}](t)
	})

	t.Run("on nil slices, maps and pointers", func(t *testing.T) {
		agreesWithEncodingJSON[struct {
			Tags     []string       `json:"tags"`
			Optional []string       `json:"optional,omitempty"`
			Counts   map[string]int `json:"counts"`
			Note     *string        `json:"note"`
			Stamp    *Stamp         `json:"stamp,omitempty"`
			Data     []byte         `json:"data"`
		}](t)
	})

	t.Run("on an embedded pointer", func(t *testing.T) {
		agreesWithEncodingJSON[struct {
			*Stamp
			Title string `json:"title"`
		}](t)
	})

	t.Run("on an embedded struct with a tag", func(t *testing.T) {
		agreesWithEncodingJSON[struct {
			Audit `json:"audit"`
		}](t)
	})

	t.Run("on the least deeply embedded field", func(t *testing.T) {
		agreesWithEncodingJSON[struct {
			Audit
			By int `json:"by"`
		}](t)
	})

	t.Run("on the only tagged field among equally deep ones", func(t *testing.T) {
		agreesWithEncodingJSON[struct {
			NameUntagged
			NameTagged
		}](t)
	})

	t.Run("on a name that equally deep fields share", func(t *testing.T) {
		agreesWithEncodingJSON[struct {
			ByText
			ByNumber
			Title string `json:"title"`
		}](t)
	})

	t.Run("on a struct that is embedded again more deeply", func(t *testing.T) {
		agreesWithEncodingJSON[struct {
			Audit
			Wrapper
		}](t)
	})

	t.Run("on a struct embedded twice at the same depth", func(t *testing.T) {
		agreesWithEncodingJSON[struct {
			First
			Second
			Title string `json:"title"`
		}](t)
	})
}
