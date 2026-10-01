package httpapi

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func spelled(t *testing.T, value any) string {
	t.Helper()

	var text strings.Builder
	require.True(t, spellOut(&text, value), "could not spell out %#v", value)

	return text.String()
}

type unexported struct{ owner string }

type ignoredByJSON struct {
	Owner string `json:"-"`
}

type withPointer struct{ Limit *int }

type sameFields struct{ Owner string }

type otherFields struct{ Owner string }

type everyKind struct {
	Flag    bool
	Small   int8
	Big     uint64
	Ratio   float64
	Wave    complex128
	Name    string
	Tags    []string
	Pair    [2]int
	Lookup  map[string]int
	Any     any
	Pointer *string
}

func TestSpellOut(t *testing.T) {
	t.Run("tells values apart by fields that JSON leaves out", func(t *testing.T) {
		assert.NotEqual(t, spelled(t, unexported{owner: "alice"}), spelled(t, unexported{owner: "bob"}))
		assert.NotEqual(t, spelled(t, ignoredByJSON{Owner: "alice"}), spelled(t, ignoredByJSON{Owner: "bob"}))
	})

	t.Run("writes equal values the same", func(t *testing.T) {
		assert.Equal(t, spelled(t, unexported{owner: "alice"}), spelled(t, unexported{owner: "alice"}))
	})

	t.Run("follows a pointer to its value rather than writing its address", func(t *testing.T) {
		five, alsoFive, six := 5, 5, 6

		assert.Equal(t, spelled(t, withPointer{Limit: &five}), spelled(t, withPointer{Limit: &alsoFive}))
		assert.NotEqual(t, spelled(t, withPointer{Limit: &five}), spelled(t, withPointer{Limit: &six}))
		assert.NotEqual(t, spelled(t, withPointer{Limit: &five}), spelled(t, withPointer{}))
	})

	t.Run("writes a map the same whatever order it hands out its entries in", func(t *testing.T) {
		forward := map[string]int{}
		backward := map[string]int{}

		for i := range 50 {
			forward[strings.Repeat("k", i+1)] = i
			backward[strings.Repeat("k", 50-i)] = 49 - i
		}

		assert.Equal(t, spelled(t, forward), spelled(t, backward))
	})

	t.Run("tells types apart that hold the same", func(t *testing.T) {
		assert.NotEqual(t, spelled(t, sameFields{Owner: "alice"}), spelled(t, otherFields{Owner: "alice"}))
		assert.NotEqual(t, spelled(t, everyKind{Any: 1}), spelled(t, everyKind{Any: int64(1)}))
	})

	t.Run("tells an empty slice from none", func(t *testing.T) {
		assert.NotEqual(t, spelled(t, everyKind{Tags: []string{}}), spelled(t, everyKind{}))
	})

	t.Run("writes every kind of field", func(t *testing.T) {
		name := "pointed at"
		value := everyKind{
			Flag: true, Small: -3, Big: 7, Ratio: 0.5, Wave: complex(1, 2), Name: `a "name"`,
			Tags: []string{"x", "y"}, Pair: [2]int{1, 2}, Lookup: map[string]int{"b": 2, "a": 1},
			Any: sameFields{Owner: "alice"}, Pointer: &name,
		}

		assert.Equal(t,
			`httpapi.everyKind{Flag:true,Small:-3,Big:7,Ratio:0.5,Wave:(1+2i),Name:"a \"name\"",`+
				`Tags:["x","y"],Pair:[1,2],Lookup:{"a":1,"b":2},Any:httpapi.sameFields{Owner:"alice"},Pointer:&"pointed at"}`,
			spelled(t, value))
	})

	t.Run("writes nothing as nil", func(t *testing.T) {
		assert.Equal(t, "nil", spelled(t, nil))
		assert.Equal(t, `httpapi.everyKind{Flag:false,Small:0,Big:0,Ratio:0,Wave:(0+0i),Name:"",Tags:nil,Pair:[0,0],Lookup:nil,Any:nil,Pointer:nil}`,
			spelled(t, everyKind{}))
	})

	t.Run("refuses what has no value to write down", func(t *testing.T) {
		for name, value := range map[string]any{
			"a function":         func() {},
			"a channel":          make(chan int),
			"a function inside":  everyKind{Any: func() {}},
			"a channel in a map": map[string]any{"c": make(chan int)},
			"a channel as key":   map[any]int{make(chan int): 1},
			"a channel in a row": [1]any{make(chan int)},
		} {
			assert.False(t, spellOut(&strings.Builder{}, value), name)
		}
	})

	t.Run("refuses a value that contains itself", func(t *testing.T) {
		type node struct{ Next *node }

		loop := &node{}
		loop.Next = loop

		selfMap := map[string]any{}
		selfMap["self"] = selfMap

		selfSlice := []any{nil}
		selfSlice[0] = selfSlice

		for name, value := range map[string]any{"a pointer": loop, "a map": selfMap, "a slice": selfSlice} {
			assert.False(t, spellOut(&strings.Builder{}, value), name)
		}
	})

	t.Run("writes the same value twice on one path", func(t *testing.T) {
		// Only a value that contains itself is refused; one that merely appears
		// twice is written twice.
		shared := "shared"
		type twice struct{ First, Second *string }

		assert.Equal(t, `httpapi.twice{First:&"shared",Second:&"shared"}`, spelled(t, twice{First: &shared, Second: &shared}))
	})
}
