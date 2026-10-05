package httpapi

import (
	"bytes"
	"cmp"
	"encoding"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"time"
)

// decodeFailure is a body that could not be decoded, told in words of its own
// rather than in those of the decoder, which name the types of Go, such as
// "Go struct field previewRequest.quantity of type int", that the caller does
// not know. It wraps the errors of the decoder, so that errors.As still finds
// them.
type decodeFailure struct {
	text   string
	causes []error
}

func (failure *decodeFailure) Error() string { return failure.text }

func (failure *decodeFailure) Unwrap() []error { return failure.causes }

// describeDecoding says what is wrong with a body that could not be decoded
// into a value of bodyType, given err, the error of encoding/json, and
// detailed, the error of decoding the body once more, reporting errors the way
// encoding/json/v2 does. An error that it has no words for comes back as it
// is.
func describeDecoding(bodyType reflect.Type, body []byte, err, detailed error) error {
	// encoding/json checks that the whole body is JSON before it decodes
	// anything, so a mistake in the JSON comes first, wherever it is.
	if _, isSyntax := errors.AsType[*json.SyntaxError](err); isSyntax {
		return describeSyntax(body, err, detailed)
	}

	if mismatch, isMismatch := errors.AsType[*json.UnmarshalTypeError](err); isMismatch {
		return describeMismatch(bodyType, mismatch, err)
	}

	// encoding/json names an unknown field in its text alone, while
	// encoding/json/v2 points to it. Both decode in the same order, so they
	// report the same field.
	if unknown, isUnknown := errors.AsType[*jsonv2.SemanticError](detailed); isUnknown && unknown.Err == jsonv2.ErrUnknownName {
		return &decodeFailure{
			text:   fmt.Sprintf("unknown field %q", unknown.JSONPointer.LastToken()),
			causes: []error{err, unknown},
		}
	}

	return err
}

// describeSyntax says what is wrong with a body that encoding/json refused
// as JSON. It reads the body as JSON text, by the same rules, without decoding
// it into a value, to tell a name that occurs twice and data after a complete
// value from any other mistake, which is invalid JSON.
//
// Two names that differ in case only are no mistake in the JSON text, but
// count as the same name while decoding, so encoding/json refuses them only
// then, which encoding/json/v2 points to.
//
// A body without any value, which is empty or holds nothing but whitespace,
// has no words of its own yet, so its error comes back as it is, and so does
// any other mistake found while decoding.
func describeSyntax(body []byte, err, detailed error) error {
	reader := jsontext.NewDecoder(bytes.NewReader(body), strictJSON)
	_, failure := reader.ReadValue()

	switch {
	case failure == nil && len(bytes.TrimLeft(body[reader.InputOffset():], " \t\r\n")) > 0:
		return &decodeFailure{text: "data after the JSON value", causes: []error{err}}
	case failure == nil:
		return describeDuplicate(err, detailed)
	case errors.Is(failure, io.EOF):
		return err
	case errors.Is(failure, jsontext.ErrDuplicateName):
		return describeDuplicate(err, failure)
	default:
		return &decodeFailure{text: "invalid JSON", causes: []error{err}}
	}
}

// describeDuplicate says which name occurs twice, which encoding/json does
// not say, but found does, if it is a name that occurs twice. Otherwise, err
// comes back as it is.
func describeDuplicate(err, found error) error {
	if !errors.Is(found, jsontext.ErrDuplicateName) {
		return err
	}

	// jsontext wraps ErrDuplicateName in a *jsontext.SyntacticError, which
	// points to the name.
	duplicate, _ := errors.AsType[*jsontext.SyntacticError](found)

	return &decodeFailure{
		text:   fmt.Sprintf("duplicate field %q", duplicate.JSONPointer.LastToken()),
		causes: []error{err, duplicate},
	}
}

// describeMismatch says which field holds a value of another kind than the
// one its type takes, naming it by its path, as encoding/json does.
//
// A value of the right kind that does not fit all the same, such as a number
// that is too large for its type, has no words of its own yet. Neither has a
// body that is of the wrong kind as a whole, which no field holds, a value
// whose type takes more than one kind, nor one of a field with the option
// string, which takes its number or boolean in a string. Their errors come
// back as they are.
func describeMismatch(bodyType reflect.Type, mismatch *json.UnmarshalTypeError, err error) error {
	expected, isKnown := kindOf(mismatch.Type)
	found, _, _ := strings.Cut(mismatch.Value, " ")

	isDescribable := mismatch.Field != "" && isKnown && kindsOfValues[found] != expected
	if !isDescribable || isQuoted(bodyType, mismatch.Field) {
		return err
	}

	return &decodeFailure{
		text:   fmt.Sprintf("%q must be %s", mismatch.Field, expected),
		causes: []error{err},
	}
}

// The kinds of JSON values, as the texts of decodeFailure name them.
const (
	kindString  = "a string"
	kindNumber  = "a number"
	kindBoolean = "a boolean"
	kindArray   = "an array"
	kindObject  = "an object"
)

// kindsOfValues maps the words that a *json.UnmarshalTypeError describes the
// kind of a value with to the kinds that the texts of decodeFailure name.
//
// Only a string or a number can be of the kind that its field takes and still
// not fit, such as a string that is no base64, or a number that is too large.
// The map names every kind all the same, so that no other such failure is
// ever taken for a value of the wrong kind.
var kindsOfValues = map[string]string{
	"string": kindString,
	"number": kindNumber,
	"bool":   kindBoolean,
	"array":  kindArray,
	"object": kindObject,
}

// kindOf returns the kind of JSON value that encoding/json decodes into a
// value of the type, which is the one it encodes the type as, and reports
// false if it can not tell, such as for a type that decodes itself. A slice of
// bytes is a string, in base64, a json.Number is a number, and a time is a
// string, whatever methods the type has.
func kindOf(valueType reflect.Type) (string, bool) {
	switch {
	case valueType == reflect.TypeFor[time.Time]():
		return kindString, true
	case valueType == reflect.TypeFor[json.Number]():
		return kindNumber, true
	case hasMethodsOf[json.Unmarshaler](valueType), hasMethodsOf[jsonv2.UnmarshalerFrom](valueType):
		return "", false
	case hasMethodsOf[encoding.TextUnmarshaler](valueType):
		return kindString, true
	}

	switch valueType.Kind() {
	case reflect.Pointer:
		return kindOf(valueType.Elem())
	case reflect.String:
		return kindString, true
	case reflect.Bool:
		return kindBoolean, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return kindNumber, true
	case reflect.Slice:
		if valueType.Elem().Kind() == reflect.Uint8 {
			return kindString, true
		}

		return kindArray, true
	case reflect.Array:
		return kindArray, true
	case reflect.Map, reflect.Struct:
		return kindObject, true
	default:
		return "", false
	}
}

// isQuoted reports whether the field at the path, which names it the way a
// *json.UnmarshalTypeError does, has the option string in its json tag, and
// reports false for a path that it can not follow. The option applies to the
// field itself only, not to the items of a list or the values of a map that
// it holds.
func isQuoted(valueType reflect.Type, path string) bool {
	hasOption := false

	for name := range strings.SplitSeq(path, ".") {
		for valueType.Kind() == reflect.Pointer {
			valueType = valueType.Elem()
		}

		switch valueType.Kind() {
		case reflect.Struct:
			field, isFound := fieldNamed(valueType, name)
			if !isFound {
				return false
			}

			_, options, _ := strings.Cut(field.Tag.Get("json"), ",")
			hasOption = slices.Contains(strings.Split(options, ","), "string")
			valueType = field.Type
		case reflect.Slice, reflect.Array, reflect.Map:
			hasOption = false
			valueType = valueType.Elem()
		default:
			return false
		}
	}

	return hasOption
}

// fieldNamed returns the field of a struct that encoding/json decodes a
// member of the given name into: the one with that name, or else one whose
// name differs in case only. The name of a field is the one in its json tag,
// or else that of the field itself. A field without a name of its own in an
// embedded struct counts as a field of the struct.
func fieldNamed(structType reflect.Type, name string) (reflect.StructField, bool) {
	var namesake reflect.StructField
	hasNamesake := false

	for _, field := range reflect.VisibleFields(structType) {
		tag := field.Tag.Get("json")
		tagName, _, _ := strings.Cut(tag, ",")

		if !field.IsExported() || tag == "-" || field.Anonymous && tagName == "" {
			continue
		}

		fieldName := cmp.Or(tagName, field.Name)
		if fieldName == name {
			return field, true
		}
		if !hasNamesake && strings.EqualFold(fieldName, name) {
			namesake, hasNamesake = field, true
		}
	}

	return namesake, hasNamesake
}

// hasMethodsOf reports whether a value of the type, or a pointer to it, has
// the methods of the interface TMethods.
func hasMethodsOf[TMethods any](valueType reflect.Type) bool {
	return reflect.PointerTo(valueType).Implements(reflect.TypeFor[TMethods]())
}
