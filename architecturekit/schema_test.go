package architecturekit_test

import (
	"encoding/json"
	"encoding/json/jsontext"
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

		require.NoError(t, store.RegisterSchemas(notedState().Schemas()))

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
