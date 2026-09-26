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
