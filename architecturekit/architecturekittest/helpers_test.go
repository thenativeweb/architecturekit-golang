package architecturekittest_test

import (
	"encoding/json"
	"iter"
)

// Small aliases so that the test files stay readable.

type iterSeq[T any] = iter.Seq[T]

func jsonUnmarshal(data []byte, target any) error { return json.Unmarshal(data, target) }
