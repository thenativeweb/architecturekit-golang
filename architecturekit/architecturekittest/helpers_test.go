package architecturekittest_test

import (
	"encoding/json"
	"iter"
)

// Small aliases so that the test files stay readable.

type iterSeq2[T any] = iter.Seq2[T, error]

func jsonUnmarshal(data []byte, target any) error { return json.Unmarshal(data, target) }
