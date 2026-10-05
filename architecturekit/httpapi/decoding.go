package httpapi

import (
	"bytes"
	"cmp"
	"encoding"
	"encoding/base64"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"slices"
	"strconv"
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

// exampleTime is a time as encoding/json takes it for a time.Time, in RFC
// 3339, which the text for a value that is no such time shows.
const exampleTime = "2026-10-05T12:00:00Z"

// describeDecoding says what is wrong with a body that could not be decoded
// into a value of bodyType, given err, the error of encoding/json, and
// detailed, the error of decoding the body once more, reporting errors the way
// encoding/json/v2 does. An error that it has no words for comes back as it
// is.
func describeDecoding(bodyType reflect.Type, body []byte, err, detailed error) error {
	// A string field with the option string holds JSON text of its own, a
	// string in quotes, whose mistakes encoding/json reports as a value that
	// does not fit, wrapping a *json.SyntaxError. So that comes first.
	if mismatch, isMismatch := errors.AsType[*json.UnmarshalTypeError](err); isMismatch {
		return describeMismatch(bodyType, body, mismatch, err, detailed)
	}

	// encoding/json checks that the whole body is JSON before it decodes
	// anything, so a mistake in the JSON comes first, wherever it is.
	if syntax, isSyntax := errors.AsType[*json.SyntaxError](err); isSyntax {
		return describeSyntax(body, syntax, err, detailed)
	}

	// encoding/json names an unknown field in its text alone, and hands back
	// the error of parsing a time as it is, while encoding/json/v2 points to
	// both. Both decode in the same order, so they report the same failure,
	// unless encoding/json/v2 refuses the option string for a type that
	// encoding/json ignores it for, such as a time. Then its failure is of
	// another kind, and the error comes back as it is.
	failure, isFailure := errors.AsType[*jsonv2.SemanticError](detailed)

	switch {
	case !isFailure:
		return err
	case failure.Err == jsonv2.ErrUnknownName:
		return &decodeFailure{
			text:   fmt.Sprintf("unknown field %q", failure.JSONPointer.LastToken()),
			causes: []error{err, failure},
		}
	case failure.GoType == reflect.TypeFor[time.Time]() && errors.As(failure.Err, new(*time.ParseError)):
		return describeTime(body, failure, err)
	default:
		return err
	}
}

// describeSyntax says what is wrong with a body that encoding/json refused
// as JSON. It reads the body as JSON text, by the same rules, without decoding
// it into a value, to tell a body without any value, which is empty or holds
// nothing but whitespace, a name that occurs twice, and data after a complete
// value from any other mistake, which is invalid JSON.
func describeSyntax(body []byte, syntax *json.SyntaxError, err, detailed error) error {
	reader := jsontext.NewDecoder(bytes.NewReader(body), strictJSON)
	_, failure := reader.ReadValue()

	switch {
	case failure == nil && len(bytes.TrimLeft(body[reader.InputOffset():], " \t\r\n")) > 0:
		return &decodeFailure{text: "data after the JSON value", causes: []error{err}}
	case failure == nil:
		return describeNamesake(body, syntax, err, detailed)
	case errors.Is(failure, io.EOF):
		return &decodeFailure{text: "empty body", causes: []error{err}}
	case errors.Is(failure, jsontext.ErrDuplicateName):
		return describeDuplicate(err, failure)
	default:
		return &decodeFailure{text: "invalid JSON", causes: []error{err}}
	}
}

// describeDuplicate says which name occurs twice, which encoding/json does
// not say, but found, an error for a name that occurs twice, does.
func describeDuplicate(err, found error) error {
	// jsontext wraps ErrDuplicateName in a *jsontext.SyntacticError, which
	// points to the name.
	duplicate, _ := errors.AsType[*jsontext.SyntacticError](found)

	return &decodeFailure{
		text:   fmt.Sprintf("duplicate field %q", duplicate.JSONPointer.LastToken()),
		causes: []error{err, duplicate},
	}
}

// describeNamesake says which name encoding/json refused for differing in
// case only from an earlier one of the same object, so that both match the
// same field. They are no mistake in the JSON text, so encoding/json refuses
// them only while decoding, which encoding/json/v2 points to, unless it
// stopped at an earlier failure that encoding/json passes over, such as a
// value of the wrong kind. Then the name is where the error of encoding/json
// says that it starts.
//
// Any other error comes back as it is, such as a *json.SyntaxError of a type
// that decodes itself, which points into JSON text of its own.
func describeNamesake(body []byte, syntax *json.SyntaxError, err, detailed error) error {
	if errors.Is(detailed, jsontext.ErrDuplicateName) {
		return describeDuplicate(err, detailed)
	}

	if syntax.Error() != jsontext.ErrDuplicateName.Error() {
		return err
	}

	name, isName := nameAt(body, syntax.Offset)
	if !isName {
		return err
	}

	return &decodeFailure{
		text:   fmt.Sprintf("duplicate field %q", name),
		causes: []error{err},
	}
}

// describeMismatch says which value does not fit its type, and why: it is of
// another kind than the one its type takes, a number out of the range of its
// type, one that is no integer for an integer, or a string that is no base64
// for a slice of bytes. A field with the option string takes its number,
// boolean, or string in a string, and the key of a map whose keys are numbers
// has to be one.
//
// A value of a type that takes more than one kind of value, or none, has no
// words of its own, and neither has the key of a map whose keys are booleans,
// which encoding/json takes for no key at all. Their errors come back as they
// are.
func describeMismatch(bodyType reflect.Type, body []byte, mismatch *json.UnmarshalTypeError, err, detailed error) error {
	failure, isFailure := errors.AsType[*jsonv2.SemanticError](detailed)

	// encoding/json takes the key of a map for a value, while encoding/json/v2
	// points to where the key starts.
	if isFailure && isNameAt(body, failure.ByteOffset) {
		return describeKey(failure.JSONPointer.Parent(), mismatch.Type, err)
	}

	path := mismatch.Field
	if path == "" {
		// encoding/json has no path for the body as a whole, and leaves it out
		// for a json.Number, wherever it is. encoding/json/v2 points to both,
		// as long as it reports a failure of the same type.
		if !isFailure || failure.GoType != mismatch.Type {
			return err
		}

		path = pathOf(failure.JSONPointer)
	}

	expected, isKnown := kindOf(mismatch.Type)
	if !isKnown {
		return err
	}

	// encoding/json writes the value after its kind, if at all: a number as it
	// is, and a string in quotes, except for the text in the string of a field
	// with the option string, which it writes as if it were a number.
	found, text, _ := strings.Cut(mismatch.Value, " ")
	isNumber := isNumberText(text)
	subject := subjectOf(path)

	if isQuoted(bodyType, path) && isQuotable(mismatch.Type) {
		if isNumber {
			return describeNumber(subject, mismatch.Type, text, err)
		}

		return &decodeFailure{
			text:   fmt.Sprintf("%s must be a string that holds %s", subject, expected),
			causes: []error{err},
		}
	}

	switch {
	case kindsOfValues[found] != expected:
		return &decodeFailure{
			text:   fmt.Sprintf("%s must be %s", subject, expected),
			causes: []error{err},
		}
	case isNumber:
		return describeNumber(subject, mismatch.Type, text, err)
	case errors.As(err, new(base64.CorruptInputError)):
		return &decodeFailure{
			text:   fmt.Sprintf("%s must be base64", subject),
			causes: []error{err},
		}
	default:
		return err
	}
}

// describeNumber says why a number, written as text, does not fit numberType,
// a type of integers or floating-point numbers: it is out of range, which a
// negative number is for unsigned integers, or it is not written as an
// integer, which encoding/json refuses for integers, even for 1.0 or 1e2. A
// number written as an integer that does not fit is out of range, wherever a
// float64 rounds it to.
func describeNumber(subject string, numberType reflect.Type, text string, err error) error {
	value, _ := strconv.ParseFloat(text, 64)
	isWrittenAsInteger := !strings.ContainsAny(text, ".eE")

	// A floating-point number that does not fit is out of range, whatever it
	// looks like.
	isInRange := false

	switch numberType.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		bound := math.Ldexp(1, numberType.Bits()-1)
		isInRange = -bound <= value && value < bound
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		isInRange = 0 <= value && value < math.Ldexp(1, numberType.Bits())
	}

	if isWrittenAsInteger || !isInRange {
		return &decodeFailure{text: subject + " is out of range", causes: []error{err}}
	}

	return &decodeFailure{text: subject + " must be an integer", causes: []error{err}}
}

// describeKey says that the keys of the map that the pointer points to have
// to be numbers, if their type takes numbers. A key of any other type has no
// words of its own, and its error comes back as it is.
func describeKey(mapPointer jsontext.Pointer, keyType reflect.Type, err error) error {
	if kind, _ := kindOf(keyType); kind != kindNumber {
		return err
	}

	return &decodeFailure{
		text:   fmt.Sprintf("the keys of %s must be numbers", subjectOf(pathOf(mapPointer))),
		causes: []error{err},
	}
}

// describeTime says which value is no time in the form that encoding/json
// takes for a time.Time, and shows one that is, given the failure that
// encoding/json/v2 reports, which points to the value. The value may also be
// the key of a map.
func describeTime(body []byte, failure *jsonv2.SemanticError, err error) error {
	text := fmt.Sprintf("%s must be a time such as %q", subjectOf(pathOf(failure.JSONPointer)), exampleTime)
	if isNameAt(body, failure.ByteOffset) {
		text = fmt.Sprintf("the keys of %s must be times such as %q", subjectOf(pathOf(failure.JSONPointer.Parent())), exampleTime)
	}

	return &decodeFailure{text: text, causes: []error{err, failure}}
}

// nameAt returns the name of a member that starts at the offset of the body,
// and reports whether there is one, which a colon follows. The offset may lie
// beyond the body, if it comes from an error of a type that decodes itself.
func nameAt(body []byte, offset int64) (string, bool) {
	rest := body[min(offset, int64(len(body))):]

	reader := jsontext.NewDecoder(bytes.NewReader(rest), strictJSON)
	token, err := reader.ReadToken()
	if err != nil {
		return "", false
	}

	after := bytes.TrimLeft(rest[reader.InputOffset():], " \t\r\n")

	return token.String(), bytes.HasPrefix(after, []byte(":"))
}

// isNameAt reports whether the name of a member starts at the offset of the
// body.
func isNameAt(body []byte, offset int64) bool {
	_, isName := nameAt(body, offset)

	return isName
}

// isNumberText reports whether text is a number, written as JSON writes one,
// with nothing around it.
func isNumberText(text string) bool {
	value := jsontext.Value(text)

	return value.Kind() == '0' && value.IsValid() && strings.TrimSpace(text) == text
}

// pathOf returns the path that the pointer points to, the way encoding/json
// names it, with a dot between the names of fields and the indexes of items,
// such as "items.0.bookId".
func pathOf(pointer jsontext.Pointer) string {
	return strings.Join(slices.Collect(pointer.Tokens()), ".")
}

// subjectOf names the value at the path, in quotes, or as the body, if the
// path is empty.
func subjectOf(path string) string {
	if path == "" {
		return "the body"
	}

	return strconv.Quote(path)
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

// isQuotable reports whether the option string applies to a value of the
// type, which encoding/json limits to booleans, numbers, and strings, and
// ignores for any other type.
func isQuotable(valueType reflect.Type) bool {
	switch valueType.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return true
	default:
		return false
	}
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
