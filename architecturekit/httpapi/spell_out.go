package httpapi

import (
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// spellOut writes a value field by field, so that two values come out the
// same exactly when they are equal in every field. It is what an entity tag
// learns about the query it answers, so a field that is left out would let two
// callers share a tag, and one of them see the answer of the other.
//
// That is why it does not use JSON, which leaves out unexported fields and
// those tagged with "-", nor the %#v verb of fmt, which writes the address of
// a pointer rather than what it points at: an address says nothing about the
// value, and may come up again for a different one. Instead it reads every
// field, unexported ones too, sorts maps by their keys, and follows pointers.
//
// Functions and channels can not be spelled out, and neither can a value that
// contains itself. For those it reports false, and the answer goes without a
// tag rather than with one that might match where it should not.
func spellOut(w io.Writer, value any) bool {
	written := reflect.ValueOf(value)
	if !written.IsValid() {
		_, _ = io.WriteString(w, "nil")
		return true
	}

	_, _ = io.WriteString(w, written.Type().String())

	return spell(w, written, map[uintptr]bool{})
}

// spell writes a single value. The values on the path to it are kept in
// onPath, so that a value which contains itself ends in false rather than in
// an endless recursion.
func spell(w io.Writer, value reflect.Value, onPath map[uintptr]bool) bool {
	switch value.Kind() {
	case reflect.Bool:
		_, _ = io.WriteString(w, strconv.FormatBool(value.Bool()))

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		_, _ = io.WriteString(w, strconv.FormatInt(value.Int(), 10))

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		_, _ = io.WriteString(w, strconv.FormatUint(value.Uint(), 10))

	case reflect.Float32, reflect.Float64:
		_, _ = io.WriteString(w, strconv.FormatFloat(value.Float(), 'g', -1, 64))

	case reflect.Complex64, reflect.Complex128:
		_, _ = io.WriteString(w, strconv.FormatComplex(value.Complex(), 'g', -1, 128))

	case reflect.String:
		_, _ = io.WriteString(w, strconv.Quote(value.String()))

	case reflect.Pointer:
		if value.IsNil() {
			_, _ = io.WriteString(w, "nil")
			return true
		}

		_, _ = io.WriteString(w, "&")

		return spellReferenced(w, value, onPath, func() bool {
			return spell(w, value.Elem(), onPath)
		})

	case reflect.Interface:
		if value.IsNil() {
			_, _ = io.WriteString(w, "nil")
			return true
		}

		// The same value may stand for different types, such as int and int64,
		// so the type the interface holds is part of what is written.
		_, _ = io.WriteString(w, value.Elem().Type().String())

		return spell(w, value.Elem(), onPath)

	case reflect.Struct:
		_, _ = io.WriteString(w, "{")

		for i := range value.NumField() {
			if i > 0 {
				_, _ = io.WriteString(w, ",")
			}

			_, _ = io.WriteString(w, value.Type().Field(i).Name+":")

			if !spell(w, value.Field(i), onPath) {
				return false
			}
		}

		_, _ = io.WriteString(w, "}")

	case reflect.Slice:
		if value.IsNil() {
			_, _ = io.WriteString(w, "nil")
			return true
		}

		return spellReferenced(w, value, onPath, func() bool {
			return spellElements(w, value, onPath)
		})

	case reflect.Array:
		return spellElements(w, value, onPath)

	case reflect.Map:
		if value.IsNil() {
			_, _ = io.WriteString(w, "nil")
			return true
		}

		return spellReferenced(w, value, onPath, func() bool {
			return spellEntries(w, value, onPath)
		})

	default:
		// Functions, channels and unsafe pointers have no value that could be
		// written down.
		return false
	}

	return true
}

// spellReferenced writes what a pointer, a slice or a map refers to, unless it
// is already on the path, which means that the value contains itself.
func spellReferenced(w io.Writer, value reflect.Value, onPath map[uintptr]bool, write func() bool) bool {
	address := value.Pointer()
	if onPath[address] {
		return false
	}

	onPath[address] = true
	defer delete(onPath, address)

	return write()
}

func spellElements(w io.Writer, value reflect.Value, onPath map[uintptr]bool) bool {
	_, _ = io.WriteString(w, "[")

	for i := range value.Len() {
		if i > 0 {
			_, _ = io.WriteString(w, ",")
		}

		if !spell(w, value.Index(i), onPath) {
			return false
		}
	}

	_, _ = io.WriteString(w, "]")

	return true
}

// spellEntries writes the entries of a map ordered by how their keys are
// written, since the order in which a map hands them out changes every time.
func spellEntries(w io.Writer, value reflect.Value, onPath map[uintptr]bool) bool {
	entries := make([][2]string, 0, value.Len())

	for iterator := value.MapRange(); iterator.Next(); {
		var key, element strings.Builder

		if !spell(&key, iterator.Key(), onPath) || !spell(&element, iterator.Value(), onPath) {
			return false
		}

		entries = append(entries, [2]string{key.String(), element.String()})
	}

	slices.SortFunc(entries, func(left, right [2]string) int {
		return strings.Compare(left[0], right[0])
	})

	_, _ = io.WriteString(w, "{")

	for i, entry := range entries {
		if i > 0 {
			_, _ = io.WriteString(w, ",")
		}

		_, _ = io.WriteString(w, entry[0]+":"+entry[1])
	}

	_, _ = io.WriteString(w, "}")

	return true
}
