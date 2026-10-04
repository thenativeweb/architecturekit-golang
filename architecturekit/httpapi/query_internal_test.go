package httpapi

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsNil(t *testing.T) {
	// Every kind that can be nil, once nil and once not, and kinds that can not
	// be nil at all, which reflect would panic on if asked.
	for _, test := range []struct {
		name  string
		value any
		isNil bool
	}{
		{"nil", nil, true},
		{"a nil pointer", (*int)(nil), true},
		{"a nil map", map[string]int(nil), true},
		{"a nil slice", []string(nil), true},
		{"a nil function", Volatile(nil), true},
		{"a nil channel", (chan string)(nil), true},
		{"a pointer", new(int), false},
		{"a map", map[string]int{}, false},
		{"a slice", []string{}, false},
		{"a function", Volatile(func(*http.Request) string { return "" }), false},
		{"a channel", make(chan string), false},
		{"a struct", querySettings{}, false},
		{"a number", 0, false},
		{"a string", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.isNil, isNil(test.value))
		})
	}
}
