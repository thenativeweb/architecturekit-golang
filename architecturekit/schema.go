package architecturekit

import (
	"encoding"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"unicode"
)

// describesSchema is a type that describes its JSON schema itself: an event, or
// the type of a field of one.
type describesSchema interface {
	Schema() map[string]any
}

var (
	describesSchemaType = reflect.TypeFor[describesSchema]()
	jsonMarshalerType   = reflect.TypeFor[json.Marshaler]()
	jsonMarshalerToType = reflect.TypeFor[jsonv2.MarshalerTo]()
	textMarshalerType   = reflect.TypeFor[encoding.TextMarshaler]()
	textAppenderType    = reflect.TypeFor[encoding.TextAppender]()
	numberType          = reflect.TypeFor[json.Number]()
)

// encodesAsJSON reports whether a type writes its JSON itself, with
// MarshalJSON, or with MarshalJSONTo of encoding/json/v2, which encoding/json
// calls as well. Either receiver counts, since what it writes can not be
// derived either way.
func encodesAsJSON(valueType reflect.Type) bool {
	pointerType := reflect.PointerTo(valueType)

	return valueType.Implements(jsonMarshalerType) || pointerType.Implements(jsonMarshalerType) ||
		valueType.Implements(jsonMarshalerToType) || pointerType.Implements(jsonMarshalerToType)
}

// encodesAsText reports whether a type writes itself as text, with MarshalText
// or AppendText, both of which encoding/json calls, with a value receiver.
func encodesAsText(valueType reflect.Type) bool {
	return valueType.Implements(textMarshalerType) || valueType.Implements(textAppenderType)
}

// encodesAsTextOnPointer reports whether a type writes itself as text only with
// a pointer receiver.
func encodesAsTextOnPointer(valueType reflect.Type) bool {
	pointerType := reflect.PointerTo(valueType)

	return !encodesAsText(valueType) &&
		(pointerType.Implements(textMarshalerType) || pointerType.Implements(textAppenderType))
}

// derivedSchemas holds the schemas derived so far, by type, since a state is
// often built anew for every command. It holds a *derivedSchema.
var derivedSchemas sync.Map

type derivedSchema struct {
	schema map[string]any
	err    error
}

// DeriveSchema derives the JSON schema of T from its structure, the way the
// kit does for an event without a Schema function. The schema describes
// exactly what encoding/json writes for T, and these rules do not change, since
// a registered schema can not change either:
//
//   - A struct is an object with the fields encoding/json writes: named by
//     their json tags, without fields tagged "-" and unexported ones, with the
//     fields of embedded structs in place of the embedded struct. Fields with
//     omitempty or omitzero, and fields of an embedded pointer, are optional,
//     all others are required, and no other fields are allowed.
//   - A string is a string, a bool a boolean, an integer an integer, and a
//     floating-point number a number. The string option of a json tag turns
//     such a field into a string.
//   - A slice is an array, a []byte a string, and an array an array of exactly
//     its length. A map is an object whose values all have the same schema.
//   - A pointer, a slice and a map may also be null, since encoding/json writes
//     null for nil, unless a field of such a type is optional and therefore
//     left out instead.
//   - A time.Time is a string in the date-time format, a json.Number a number,
//     unless the string option makes it a string, and a type with a
//     MarshalText or an AppendText function a string. An interface allows any
//     value.
//   - The type of a field that has a Schema function is described by it, like
//     an event is.
//
// T's own Schema function, if it has one, is not called, so that it can start
// from the derived schema. The returned schema belongs to the caller, and it
// has the shape encoding/json decodes JSON into: objects are map[string]any,
// arrays []any, and numbers float64.
//
// A type that encodes itself with MarshalJSON, or with MarshalJSONTo of
// encoding/json/v2, can not be derived, and neither can one with MarshalText or
// AppendText on a pointer receiver only, since encoding/json calls those only
// for a value it can take the address of. Nor can a recursive type, a channel,
// a function or a complex number. That is a programming error, so DeriveSchema
// panics, naming the type.
func DeriveSchema[T any]() map[string]any {
	schema, err := deriveStructure(reflect.TypeFor[T]())
	if err != nil {
		panic(fmt.Sprintf("architecturekit: %v", err))
	}

	return schema
}

// eventSchemaOf returns the schema of an event type: its own, if it has a
// Schema function, and the derived one otherwise.
func eventSchemaOf[TEvent Event]() (map[string]any, error) {
	eventType := reflect.TypeFor[TEvent]()
	if schema, hasOwn := ownSchema(eventType); hasOwn {
		return schema, nil
	}

	return deriveStructure(eventType)
}

// ownSchema calls the Schema function of a type, if it has one, with either a
// value or a pointer receiver. A pointer has no schema of its own, since it
// only borrows the functions of the type it points to.
func ownSchema(valueType reflect.Type) (map[string]any, bool) {
	switch {
	case valueType.Kind() == reflect.Pointer:
		return nil, false
	case valueType.Implements(describesSchemaType):
		return reflect.Zero(valueType).Interface().(describesSchema).Schema(), true
	case reflect.PointerTo(valueType).Implements(describesSchemaType):
		return reflect.New(valueType).Interface().(describesSchema).Schema(), true
	default:
		return nil, false
	}
}

// deriveStructure derives the schema of a type from its structure, without
// calling its own Schema function, and returns a copy that belongs to the
// caller.
func deriveStructure(valueType reflect.Type) (map[string]any, error) {
	cached, isCached := derivedSchemas.Load(valueType)
	if !isCached {
		schema, err := (&deriver{inProgress: map[reflect.Type]bool{}}).structure(valueType)
		cached, _ = derivedSchemas.LoadOrStore(valueType, &derivedSchema{schema: schema, err: err})
	}

	derived := cached.(*derivedSchema)
	if derived.err != nil {
		return nil, derived.err
	}

	return copyOfSchema(valueType, derived.schema)
}

// copyOfSchema copies a schema by encoding and decoding it, which reaches
// every level, whatever the types a Schema function has used.
func copyOfSchema(valueType reflect.Type, schema map[string]any) (map[string]any, error) {
	var copied map[string]any

	encoded, err := json.Marshal(schema)
	if err == nil {
		err = json.Unmarshal(encoded, &copied)
	}
	if err != nil {
		return nil, fmt.Errorf("the schema of %v can not be encoded as JSON: %w", valueType, err)
	}

	return copied, nil
}

// deriver derives one schema. It remembers the types it is deriving, so that
// a recursive type fails instead of never ending.
type deriver struct {
	inProgress map[reflect.Type]bool
}

// schemaOf describes a type the way it appears as a field: by its own Schema
// function, if it has one, and by its structure otherwise.
func (d *deriver) schemaOf(valueType reflect.Type) (map[string]any, error) {
	if schema, hasOwn := ownSchema(valueType); hasOwn {
		if schema == nil {
			return nil, fmt.Errorf("the Schema function of %v returns nil", valueType)
		}

		return schema, nil
	}

	return d.structure(valueType)
}

func (d *deriver) structure(valueType reflect.Type) (map[string]any, error) {
	if d.inProgress[valueType] {
		return nil, fmt.Errorf("%v is recursive, so its schema can not be derived; give it a Schema function", valueType)
	}
	d.inProgress[valueType] = true
	defer delete(d.inProgress, valueType)

	// encoding/json writes null for a nil pointer, and otherwise what it writes
	// for the value the pointer points to.
	if valueType.Kind() == reflect.Pointer {
		element, err := d.schemaOf(valueType.Elem())
		if err != nil {
			return nil, err
		}
		return nullable(element), nil
	}

	// encoding/json asks a type to encode itself before anything else, and a
	// time.Time does so as a string.
	if valueType == timeType {
		return map[string]any{"type": "string", "format": "date-time"}, nil
	}

	// A json.Number encodes itself, but what it writes is known: the number
	// literal it holds, or 0 if it is empty.
	if valueType == numberType {
		return map[string]any{"type": "number"}, nil
	}

	if encodesAsJSON(valueType) {
		return nil, fmt.Errorf("%v encodes itself with MarshalJSON or MarshalJSONTo, so its schema can not be derived; give it a Schema function", valueType)
	}
	if encodesAsText(valueType) {
		return map[string]any{"type": "string"}, nil
	}
	if encodesAsTextOnPointer(valueType) {
		// encoding/json calls MarshalText and AppendText with a pointer receiver
		// only if the value is addressable, so whether it is written as a string
		// depends on where the value is.
		return nil, fmt.Errorf("%v encodes itself with MarshalText or AppendText on a pointer only, so its schema can not be derived; give it a Schema function", valueType)
	}

	switch valueType.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return map[string]any{"type": "integer"}, nil

	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil

	case reflect.String:
		return map[string]any{"type": "string"}, nil

	case reflect.Interface:
		return map[string]any{}, nil

	case reflect.Slice:
		// encoding/json writes a []byte as a base64 string, unless its element
		// type encodes itself.
		if valueType.Elem().Kind() == reflect.Uint8 && !encodesItself(valueType.Elem()) {
			return nullable(map[string]any{"type": "string"}), nil
		}

		items, err := d.schemaOf(valueType.Elem())
		if err != nil {
			return nil, err
		}
		return nullable(map[string]any{"type": "array", "items": items}), nil

	case reflect.Array:
		items, err := d.schemaOf(valueType.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"type":     "array",
			"items":    items,
			"minItems": valueType.Len(),
			"maxItems": valueType.Len(),
		}, nil

	case reflect.Map:
		if !isMapKey(valueType.Key()) {
			return nil, fmt.Errorf("%v has keys encoding/json can not write", valueType)
		}

		values, err := d.schemaOf(valueType.Elem())
		if err != nil {
			return nil, err
		}
		return nullable(map[string]any{"type": "object", "additionalProperties": values}), nil

	case reflect.Struct:
		return d.object(valueType)

	default:
		return nil, fmt.Errorf("%v can not be written by encoding/json, so its schema can not be derived", valueType)
	}
}

// object describes a struct as the object encoding/json writes for it.
func (d *deriver) object(structType reflect.Type) (map[string]any, error) {
	properties := map[string]any{}
	var required []string

	fields, err := jsonFields(structType)
	if err != nil {
		return nil, err
	}

	for _, field := range fields {
		schema, err := d.fieldSchema(field)
		if err != nil {
			return nil, fmt.Errorf("field %s of %v: %w", field.goName, structType, err)
		}

		properties[field.name] = schema
		if !field.isOptional {
			required = append(required, field.name)
		}
	}

	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}

	return schema, nil
}

func (d *deriver) fieldSchema(field jsonField) (map[string]any, error) {
	if !field.isQuoted {
		// An optional field is left out instead of being written as null, so
		// null is only allowed where encoding/json writes it.
		if field.isOptional {
			return d.schemaOfPresent(field.valueType)
		}
		return d.schemaOf(field.valueType)
	}

	// The string option writes the value as a string, and a nil pointer as
	// null.
	if field.valueType.Kind() == reflect.Pointer && !field.isOptional {
		return nullable(map[string]any{"type": "string"}), nil
	}
	return map[string]any{"type": "string"}, nil
}

// schemaOfPresent describes a value that is written only if it is not empty,
// which rules out null for a pointer, a slice and a map.
func (d *deriver) schemaOfPresent(valueType reflect.Type) (map[string]any, error) {
	if _, hasOwn := ownSchema(valueType); hasOwn {
		return d.schemaOf(valueType)
	}

	switch valueType.Kind() {
	case reflect.Pointer:
		return d.schemaOf(valueType.Elem())
	case reflect.Slice, reflect.Map:
		schema, err := d.schemaOf(valueType)
		if err != nil {
			return nil, err
		}
		return withoutNull(schema), nil
	default:
		return d.schemaOf(valueType)
	}
}

// jsonField is a field as encoding/json writes it.
type jsonField struct {
	name       string
	goName     string
	valueType  reflect.Type
	index      []int
	isTagged   bool
	isOptional bool
	isQuoted   bool
}

// jsonFields returns the fields encoding/json writes for a struct, in the
// order it writes them, following its rules for embedded structs: of several
// fields with the same name, the least deeply embedded one wins, and among
// those, the only tagged one. If there is no single winner, encoding/json
// leaves the name out, which is also what happens to the fields of a struct
// that is embedded twice at the same depth.
//
// A name encoding/json considers invalid is an error, since encoding/json
// reads it differently depending on the Go version the application declares.
func jsonFields(structType reflect.Type) ([]jsonField, error) {
	type embedding struct {
		structType       reflect.Type
		index            []int
		isBehindAPointer bool
	}

	var candidates []jsonField
	next := []embedding{{structType: structType}}
	var count map[reflect.Type]int
	nextCount := map[reflect.Type]int{structType: 1}
	visited := map[reflect.Type]bool{}

	for len(next) > 0 {
		current := next
		next = nil
		count, nextCount = nextCount, map[reflect.Type]int{}

		for _, embedded := range current {
			if visited[embedded.structType] {
				continue
			}
			visited[embedded.structType] = true

			for i := range embedded.structType.NumField() {
				structField := embedded.structType.Field(i)
				index := append(slices.Clone(embedded.index), i)

				fieldType := structField.Type
				isPointer := fieldType.Name() == "" && fieldType.Kind() == reflect.Pointer
				if isPointer {
					fieldType = fieldType.Elem()
				}

				if structField.Anonymous {
					if !structField.IsExported() && fieldType.Kind() != reflect.Struct {
						continue
					}
				} else if !structField.IsExported() {
					continue
				}

				tag := structField.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, options, _ := strings.Cut(tag, ",")
				if name != "" && !isValidTagName(name) {
					return nil, fmt.Errorf("field %s of %v has the json tag name %q, which encoding/json reads "+
						"differently depending on the Go version; use a valid name", structField.Name, embedded.structType, name)
				}

				if name == "" && structField.Anonymous && fieldType.Kind() == reflect.Struct {
					nextCount[fieldType]++
					if nextCount[fieldType] == 1 {
						next = append(next, embedding{
							structType:       fieldType,
							index:            index,
							isBehindAPointer: embedded.isBehindAPointer || isPointer,
						})
					}
					continue
				}

				field := jsonField{
					name:       name,
					goName:     structField.Name,
					valueType:  structField.Type,
					index:      index,
					isTagged:   name != "",
					isOptional: embedded.isBehindAPointer || hasOption(options, "omitempty") || hasOption(options, "omitzero"),
					isQuoted:   hasOption(options, "string") && isQuotable(structField.Type) && (isNumber(structField.Type) || !encodesItself(structField.Type)),
				}
				if field.name == "" {
					field.name = structField.Name
				}

				candidates = append(candidates, field)
				if count[embedded.structType] > 1 {
					// A struct embedded twice at the same depth yields every
					// field twice, which leaves no single winner.
					candidates = append(candidates, field)
				}
			}
		}
	}

	byName := map[string][]jsonField{}
	var names []string
	for _, candidate := range candidates {
		if _, isKnown := byName[candidate.name]; !isKnown {
			names = append(names, candidate.name)
		}
		byName[candidate.name] = append(byName[candidate.name], candidate)
	}

	var fields []jsonField
	for _, name := range names {
		if field, isDominant := dominantField(byName[name]); isDominant {
			fields = append(fields, field)
		}
	}

	slices.SortFunc(fields, func(left, right jsonField) int {
		return slices.Compare(left.index, right.index)
	})

	return fields, nil
}

// isValidTagName reports whether encoding/json accepts a name from a json tag.
func isValidTagName(name string) bool {
	for _, character := range name {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", character):
		case unicode.IsLetter(character), unicode.IsDigit(character):
		default:
			return false
		}
	}

	return true
}

// dominantField picks the field encoding/json writes among several with the
// same name, which are given from the least deeply embedded one on.
func dominantField(fields []jsonField) (jsonField, bool) {
	depth := len(fields[0].index)
	var shallowest []jsonField
	for _, field := range fields {
		if len(field.index) == depth {
			shallowest = append(shallowest, field)
		}
	}

	if len(shallowest) == 1 {
		return shallowest[0], true
	}

	var tagged []jsonField
	for _, field := range shallowest {
		if field.isTagged {
			tagged = append(tagged, field)
		}
	}
	if len(tagged) == 1 {
		return tagged[0], true
	}

	return jsonField{}, false
}

func hasOption(options, option string) bool {
	for candidate := range strings.SplitSeq(options, ",") {
		if candidate == option {
			return true
		}
	}

	return false
}

// isQuotable reports whether the string option applies to a type, which it
// does for strings, numbers and booleans, also behind an unnamed pointer.
func isQuotable(valueType reflect.Type) bool {
	if valueType.Name() == "" && valueType.Kind() == reflect.Pointer {
		valueType = valueType.Elem()
	}

	switch valueType.Kind() {
	case reflect.Bool, reflect.String, reflect.Float32, reflect.Float64,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return true
	default:
		return false
	}
}

func isMapKey(keyType reflect.Type) bool {
	switch keyType.Kind() {
	case reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return true
	default:
		return encodesAsText(keyType)
	}
}

func encodesItself(valueType reflect.Type) bool {
	return encodesAsJSON(valueType) || encodesAsText(valueType) || encodesAsTextOnPointer(valueType)
}

// isNumber reports whether a type is a json.Number, also behind an unnamed
// pointer. It encodes itself, but encoding/json quotes it for the string option
// all the same, as it does for the numbers it writes.
func isNumber(valueType reflect.Type) bool {
	if valueType.Name() == "" && valueType.Kind() == reflect.Pointer {
		valueType = valueType.Elem()
	}

	return valueType == numberType
}

// nullable allows null in addition to what the given schema allows.
func nullable(schema map[string]any) map[string]any {
	return map[string]any{"anyOf": []any{schema, map[string]any{"type": "null"}}}
}

// withoutNull undoes nullable.
func withoutNull(schema map[string]any) map[string]any {
	anyOf, isNullable := schema["anyOf"].([]any)
	if !isNullable || len(schema) != 1 || len(anyOf) != 2 {
		return schema
	}

	return anyOf[0].(map[string]any)
}
