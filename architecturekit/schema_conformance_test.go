package architecturekit_test

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// The types below cover the rules of DeriveSchema, one area each, so that a
// value of every one of them can be written and checked by the database.

type conformScalars struct {
	Flag     bool    `json:"flag"`
	Int      int     `json:"int"`
	Int8     int8    `json:"int8"`
	Int64    int64   `json:"int64"`
	Uint     uint    `json:"uint"`
	Uint8    uint8   `json:"uint8"`
	Uint64   uint64  `json:"uint64"`
	Float32  float32 `json:"float32"`
	Float64  float64 `json:"float64"`
	Text     string  `json:"text"`
	Untagged string
}

type conformNested struct {
	Name  string `json:"name"`
	Count *int   `json:"count"`
}

type conformContainers struct {
	Strings  []string             `json:"strings"`
	Ints     []int                `json:"ints"`
	Matrix   [][]string           `json:"matrix"`
	Fixed    [3]bool              `json:"fixed"`
	Empty    [0]int               `json:"empty"`
	ByName   map[string]int       `json:"byName"`
	ByNumber map[int]string       `json:"byNumber"`
	BySmall  map[uint8]bool       `json:"bySmall"`
	Lists    map[string][]float64 `json:"lists"`
	Nested   []conformNested      `json:"nested"`
	Pointers []*int               `json:"pointers"`
	Bytes    []byte               `json:"bytes"`
	ByteRow  [4]byte              `json:"byteRow"`
}

type conformPointers struct {
	Int     *int           `json:"int"`
	Text    *string        `json:"text"`
	Twice   **int          `json:"twice"`
	Nested  *conformNested `json:"nested"`
	Time    *time.Time     `json:"time"`
	Number  *json.Number   `json:"number"`
	Payload *any           `json:"payload"`
}

type conformOptions struct {
	OmitText   string          `json:"omitText,omitempty"`
	OmitInt    int             `json:"omitInt,omitempty"`
	OmitSlice  []string        `json:"omitSlice,omitempty"`
	OmitMap    map[string]bool `json:"omitMap,omitempty"`
	OmitPtr    *string         `json:"omitPtr,omitempty"`
	ZeroTime   time.Time       `json:"zeroTime,omitzero"`
	ZeroNested conformNested   `json:"zeroNested,omitzero"`
	ZeroInt    int             `json:"zeroInt,omitzero"`
	QuotedInt  int             `json:"quotedInt,string"`
	QuotedF    float64         `json:"quotedF,string"`
	QuotedBool bool            `json:"quotedBool,string"`
	QuotedText string          `json:"quotedText,string"`
	QuotedPtr  *int            `json:"quotedPtr,string"`
	QuotedOmit int             `json:"quotedOmit,omitempty,string"`
	Skipped    string          `json:"-"`
}

type ConformBase struct {
	ID    string `json:"id"`
	Shade string `json:"shade"`
}

type ConformExtra struct {
	Note  string `json:"note"`
	Shade string `json:"shade"`
}

type ConformDeep struct {
	ConformBase
	Level int `json:"level"`
}

type conformEmbedding struct {
	ConformDeep
	*ConformExtra
	Tagged ConformBase `json:"tagged"`
	Shade  string      `json:"shade"`
}

type conformSpecial struct {
	At       time.Time              `json:"at"`
	Amount   json.Number            `json:"amount"`
	Amounts  []json.Number          `json:"amounts"`
	Quoted   json.Number            `json:"quoted,string"`
	Level    level                  `json:"level"`
	Levels   map[level]level        `json:"levels"`
	Count    appended               `json:"count"`
	Counts   map[appended]string    `json:"counts"`
	Payload  any                    `json:"payload"`
	Payloads map[string]any         `json:"payloads"`
	Anon     struct{ Inner string } `json:"anon"`
}

func TestDerivedSchemasAcceptWhatEncodingJSONWrites(t *testing.T) {
	// The other side of DeriveSchema: whatever encoding/json writes for a value,
	// the database accepts under the schema derived for its type. The values
	// are random, but from a fixed seed, so that a failure can be repeated.
	t.Run("scalars", conforms[conformScalars])
	t.Run("containers", conforms[conformContainers])
	t.Run("pointers", conforms[conformPointers])
	t.Run("options", conforms[conformOptions])
	t.Run("embedding", conforms[conformEmbedding])
	t.Run("special types", conforms[conformSpecial])
}

func TestDerivedSchemasRefuseWhatEncodingJSONDoesNotWrite(t *testing.T) {
	client := rawClient(t)
	eventType := registerDerived[conformNested](t, client, "refuse")

	write := func(data any) error {
		_, err := client.WriteEvents([]eventsourcingdb.EventCandidate{{
			Source: "https://thenativeweb.io", Subject: "/conform/refuse", Type: eventType, Data: data,
		}}, nil)
		return err
	}

	require.NoError(t, write(map[string]any{"name": "a", "count": nil}), "what encoding/json writes")
	assert.Error(t, write(map[string]any{"name": "a"}), "a required field is missing")
	assert.Error(t, write(map[string]any{"name": "a", "count": nil, "other": 1}), "a field encoding/json never writes")
	assert.Error(t, write(map[string]any{"name": 1, "count": nil}), "a field of the wrong type")
}

// conforms writes the zero value and a hundred random values of T, and wants
// the database to accept every one of them under the schema derived for T.
func conforms[T any](t *testing.T) {
	client := rawClient(t)
	name := strings.ToLower(reflect.TypeFor[T]().Name())
	eventType := registerDerived[T](t, client, name)

	random := rand.New(rand.NewPCG(1, 2))

	var zero T
	values := []T{zero}
	for range 100 {
		value := reflect.New(reflect.TypeFor[T]()).Elem()
		randomize(random, value, 0)
		values = append(values, value.Interface().(T))
	}

	candidates := make([]eventsourcingdb.EventCandidate, len(values))
	for i, value := range values {
		_, err := json.Marshal(value)
		require.NoError(t, err, "value %d can not be encoded", i)

		candidates[i] = eventsourcingdb.EventCandidate{
			Source:  "https://thenativeweb.io",
			Subject: "/conform/" + name,
			Type:    eventType,
			Data:    value,
		}
	}

	// All of them go in one write, since the database may limit how often it
	// is written to. Only if that fails are they written one by one, to name
	// the value that was refused.
	if _, err := client.WriteEvents(candidates, nil); err == nil {
		return
	}

	for i, candidate := range candidates {
		if _, err := client.WriteEvents([]eventsourcingdb.EventCandidate{candidate}, nil); err != nil {
			encoded, _ := json.Marshal(candidate.Data)
			assert.Failf(t, "the database refused a value", "value %d: %s: %v", i, encoded, err)
			return
		}
	}
}

// registerDerived registers the derived schema of T under an event type of its
// own, since a registered schema can not change.
func registerDerived[T any](t *testing.T, client *eventsourcingdb.Client, name string) string {
	t.Helper()

	eventType := fmt.Sprintf("io.thenativeweb.test.conform-%s-%d", name, time.Now().UnixNano())
	require.NoError(t, client.RegisterEventSchema(eventType, architecturekit.DeriveSchema[T]()))

	return eventType
}

var (
	timeKind   = reflect.TypeFor[time.Time]()
	numberKind = reflect.TypeFor[json.Number]()
)

// randomize sets a value to something random that encoding/json can write,
// field by field, and leaves alone what it can not set, such as unexported
// fields. Pointers, slices and maps are sometimes nil, and the depth is
// limited, so that recursive values end.
func randomize(random *rand.Rand, value reflect.Value, depth int) {
	switch value.Type() {
	case timeKind:
		seconds := random.Int64N(253402300799) // up to the end of the year 9999
		value.Set(reflect.ValueOf(time.Unix(seconds, random.Int64N(1e9)).In(randomZone(random))))
		return
	case numberKind:
		value.SetString(randomNumberLiteral(random))
		return
	}

	switch value.Kind() {
	case reflect.Bool:
		value.SetBool(random.IntN(2) == 1)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		bits := value.Type().Bits()
		value.SetInt(random.Int64() >> (64 - bits))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		bits := value.Type().Bits()
		value.SetUint(random.Uint64() >> (64 - bits))
	case reflect.Float32:
		value.SetFloat(float64(float32(randomFloat(random, math.MaxFloat32))))
	case reflect.Float64:
		value.SetFloat(randomFloat(random, math.MaxFloat64))
	case reflect.String:
		value.SetString(randomText(random))
	case reflect.Pointer:
		if random.IntN(3) == 0 || depth > 3 {
			value.Set(reflect.Zero(value.Type()))
			return
		}
		pointed := reflect.New(value.Type().Elem())
		randomize(random, pointed.Elem(), depth+1)
		value.Set(pointed)
	case reflect.Slice:
		if random.IntN(4) == 0 || depth > 3 {
			value.Set(reflect.Zero(value.Type()))
			return
		}
		slice := reflect.MakeSlice(value.Type(), random.IntN(4), random.IntN(4)+4)
		for i := range slice.Len() {
			randomize(random, slice.Index(i), depth+1)
		}
		value.Set(slice)
	case reflect.Array:
		for i := range value.Len() {
			randomize(random, value.Index(i), depth+1)
		}
	case reflect.Map:
		if random.IntN(4) == 0 || depth > 3 {
			value.Set(reflect.Zero(value.Type()))
			return
		}
		entries := reflect.MakeMap(value.Type())
		for range random.IntN(4) {
			key := reflect.New(value.Type().Key()).Elem()
			randomize(random, key, depth+1)
			entry := reflect.New(value.Type().Elem()).Elem()
			randomize(random, entry, depth+1)
			entries.SetMapIndex(key, entry)
		}
		value.Set(entries)
	case reflect.Struct:
		for i := range value.NumField() {
			if value.Field(i).CanSet() {
				randomize(random, value.Field(i), depth+1)
			}
		}
	case reflect.Interface:
		if any := randomJSON(random, depth); any != nil {
			value.Set(reflect.ValueOf(any))
		}
	}
}

func randomFloat(random *rand.Rand, limit float64) float64 {
	switch random.IntN(4) {
	case 0:
		return 0
	case 1:
		return float64(random.IntN(2000) - 1000)
	case 2:
		return (random.Float64()*2 - 1) * limit
	default:
		return (random.Float64()*2 - 1) * math.Pow(10, float64(random.IntN(40)-20))
	}
}

func randomNumberLiteral(random *rand.Rand) string {
	switch random.IntN(4) {
	case 0:
		return ""
	case 1:
		return fmt.Sprint(random.Int64N(1e12) - 5e11)
	case 2:
		return fmt.Sprintf("%d.%d", random.IntN(1000), random.IntN(1000))
	default:
		return fmt.Sprintf("%de%d", random.IntN(100), random.IntN(20)-10)
	}
}

func randomText(random *rand.Rand) string {
	alphabet := []rune("abcXYZ019 \"\\/\n\t\u0000\u001féüß€😀<>&'")

	var text strings.Builder
	for range random.IntN(12) {
		text.WriteRune(alphabet[random.IntN(len(alphabet))])
	}

	return text.String()
}

func randomZone(random *rand.Rand) *time.Location {
	if random.IntN(2) == 0 {
		return time.UTC
	}

	return time.FixedZone("random", (random.IntN(28)-14)*3600)
}

// randomJSON returns a random value of the kinds encoding/json decodes into an
// interface, or nil.
func randomJSON(random *rand.Rand, depth int) any {
	switch kind := random.IntN(6); {
	case kind == 0:
		return nil
	case kind == 1:
		return random.IntN(2) == 1
	case kind == 2:
		return randomFloat(random, math.MaxFloat64)
	case kind == 3:
		return randomText(random)
	case kind == 4 && depth < 3:
		values := make([]any, random.IntN(3))
		for i := range values {
			values[i] = randomJSON(random, depth+1)
		}
		return values
	case depth < 3:
		values := map[string]any{}
		for range random.IntN(3) {
			values[randomText(random)] = randomJSON(random, depth+1)
		}
		return values
	default:
		return nil
	}
}
