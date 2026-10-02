package architecturekit

import (
	"reflect"
	"time"
)

var timeType = reflect.TypeFor[time.Time]()

// isValueType reports whether copying a value of the type copies all of its
// data, so that two copies can never change each other. Slices, maps and
// pointers break that, and so do channels, functions and interfaces, which
// may hold any of them.
//
// time.Time holds a pointer to its location, but a location never changes, so
// a time counts as a value, as it does everywhere else in Go.
func isValueType(valueType reflect.Type) bool {
	if valueType == timeType {
		return true
	}

	switch valueType.Kind() {
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64,
		reflect.Complex64, reflect.Complex128,
		reflect.String:
		return true

	case reflect.Array:
		return isValueType(valueType.Elem())

	case reflect.Struct:
		for field := range valueType.Fields() {
			if !isValueType(field.Type) {
				return false
			}
		}
		return true

	default:
		return false
	}
}

// sharesData reports whether a copy of the value shares data with it that
// either of them can change, so that writing through one changes the other:
// a map, a pointer or a channel that is not nil, or a slice with room for
// elements, at any depth. Unlike isValueType, it looks at the value rather
// than at its type, so that a value whose maps, slices and pointers are all
// nil shares nothing.
//
// A slice without room for elements, that is with a capacity of zero, shares
// nothing either, even if it is not nil: it has no element to change, and
// appending to it allocates a new array. A function shares nothing that a
// copy could change, and a time counts as a value, as in isValueType.
func sharesData(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Map, reflect.Pointer, reflect.UnsafePointer, reflect.Chan:
		return !value.IsNil()

	case reflect.Slice:
		return value.Cap() > 0

	case reflect.Interface:
		// The value of a nil interface is invalid, and so shares nothing.
		return sharesData(value.Elem())

	case reflect.Array:
		for i := range value.Len() {
			if sharesData(value.Index(i)) {
				return true
			}
		}
		return false

	case reflect.Struct:
		if value.Type() == timeType {
			return false
		}
		for _, field := range value.Fields() {
			if sharesData(field) {
				return true
			}
		}
		return false

	default:
		return false
	}
}
