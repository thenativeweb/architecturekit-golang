package architecturekit_test

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// batchedResumable is a resumable projection that catches up in batches of a
// hundred. It records every event it applies and every checkpoint it saves,
// and it fails on the first attempts at one event with a transient failure. It
// is safe to read while a run writes it.
type batchedResumable struct {
	mutex        sync.Mutex
	seen         []string
	checkpoints  []string
	flakyID      string
	failuresLeft int
}

func (r *batchedResumable) CatchUpBatchSize() int { return 100 }

func (r *batchedResumable) Apply(_ context.Context, event eventsourcingdb.Event) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if event.ID == r.flakyID && r.failuresLeft > 0 {
		r.failuresLeft--
		return fmt.Errorf("%w: the target is unavailable", architecturekit.ErrTransient)
	}

	r.seen = append(r.seen, event.ID)

	return nil
}

func (r *batchedResumable) Checkpoint(context.Context) (string, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if len(r.checkpoints) == 0 {
		return "", nil
	}

	return r.checkpoints[len(r.checkpoints)-1], nil
}

func (r *batchedResumable) SaveCheckpoint(_ context.Context, eventID string) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.checkpoints = append(r.checkpoints, eventID)

	return nil
}

func (r *batchedResumable) IDs() []string {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return append([]string(nil), r.seen...)
}

func (r *batchedResumable) Checkpoints() []string {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return append([]string(nil), r.checkpoints...)
}

// largeBatchTransactionalCollector is transactional and catches up in batches
// of a hundred.
type largeBatchTransactionalCollector struct {
	transactionalCollector
}

func (c *largeBatchTransactionalCollector) CatchUpBatchSize() int { return 100 }

// idsFrom returns the IDs from first to last, as strings.
func idsFrom(first, last int) []string {
	ids := make([]string, 0, last-first+1)
	for id := first; id <= last; id++ {
		ids = append(ids, strconv.Itoa(id))
	}

	return ids
}

// intsFrom returns the numbers from first to last.
func intsFrom(first, last int) []int {
	ids := make([]int, 0, last-first+1)
	for id := first; id <= last; id++ {
		ids = append(ids, id)
	}

	return ids
}

func TestBatching(t *testing.T) {
	t.Run("saves the checkpoint once per batch while catching up", func(t *testing.T) {
		database := &fakeDatabase{events: intsFrom(0, 249)}
		target := &batchedResumable{}

		store := architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io")
		require.NoError(t, architecturekit.CatchUpProjection(t.Context(), store, architecturekit.ExactSubject("/test"), target))

		assert.Equal(t, idsFrom(0, 249), target.IDs())
		assert.Equal(t, []string{"99", "199", "249"}, target.Checkpoints(),
			"two full batches, then the incomplete final one")
	})

	t.Run("saves the checkpoint after every live event, whatever the batch size", func(t *testing.T) {
		database := &fakeDatabase{
			events:       intsFrom(0, 2),
			endObserving: func(int) bool { return false },
		}
		target := &batchedResumable{}

		stop := runInBackground(t, reconnectingStore(newFakeDatabase(t, database), &reconnects{}), target)

		waitFor(t, func() bool { return slices.Equal(target.Checkpoints(), []string{"2"}) })

		// Live, a batch would wait for 99 more events, and the checkpoint for
		// this one would never be saved.
		database.add(3)
		waitFor(t, func() bool { return slices.Equal(target.Checkpoints(), []string{"2", "3"}) })

		database.add(4, 5)
		waitFor(t, func() bool { return slices.Equal(target.Checkpoints(), []string{"2", "3", "4", "5"}) })

		assert.NoError(t, stop(t))
		assert.Equal(t, idsFrom(0, 5), target.IDs())
	})

	t.Run("commits every live event of a transactional projection at once, whatever the batch size", func(t *testing.T) {
		database := &fakeDatabase{
			events:       intsFrom(0, 2),
			endObserving: func(int) bool { return false },
		}
		target := &largeBatchTransactionalCollector{}

		ctx, cancel := context.WithCancel(t.Context())
		run := architecturekit.StartTransactionalProjection(ctx,
			reconnectingStore(newFakeDatabase(t, database), &reconnects{}), architecturekit.ExactSubject("/test"), target)
		t.Cleanup(func() { cancel(); <-run.Done() })

		waitFor(t, func() bool { return len(target.IDs()) == 3 })

		// The events of a transaction only become visible with its commit, and
		// live, a batch would wait for 99 more events before committing.
		database.add(3)
		waitFor(t, func() bool { return len(target.IDs()) == 4 })

		checkpoint, err := target.Checkpoint(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "3", checkpoint)

		cancel()
		select {
		case <-run.Done():
		case <-time.After(5 * time.Second):
			require.Fail(t, "the run did not end after its context ended")
		}
		assert.NoError(t, run.Err())
	})

	t.Run("applies no event twice when a transient failure cuts a batch short", func(t *testing.T) {
		database := &fakeDatabase{
			events:       intsFrom(0, 9),
			endObserving: func(int) bool { return false },
		}
		observed := &reconnects{}
		target := &batchedResumable{flakyID: "5", failuresLeft: 2}

		stop := runInBackground(t, reconnectingStore(newFakeDatabase(t, database), observed), target)

		waitFor(t, func() bool { return slices.Contains(target.Checkpoints(), "9") })
		assert.NoError(t, stop(t))

		// The events before the failing one were applied, but their checkpoint
		// was not saved yet. Reading again goes on after them, not from the
		// last checkpoint.
		assert.Equal(t, idsFrom(0, 9), target.IDs(), "want every event exactly once")
		assert.Equal(t, 2, observed.count(), "want two retries")
	})
}
