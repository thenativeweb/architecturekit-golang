package architecturekittest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/architecturekittest"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// --- a view and a projection to drive ---

type owner struct {
	Name string
}

func ownerView() *architecturekit.InMemoryView[string, owner] {
	return architecturekit.NewInMemoryView(func(item owner) string { return item.Name })
}

func insertOwner(t *testing.T, view *architecturekit.InMemoryView[string, owner], eventID string, name string) {
	t.Helper()

	_, err := view.Insert(context.Background(), eventID, owner{Name: name})
	require.NoError(t, err, "failed to insert %q", name)
}

func ownerProjection(view *architecturekit.InMemoryView[string, owner]) architecturekit.Projection {
	return architecturekit.ProjectionFunc(func(ctx context.Context, event eventsourcingdb.Event) error {
		if event.Type != (opened{}).EventType() {
			return nil
		}

		var payload opened
		if err := jsonUnmarshal(event.Data, &payload); err != nil {
			return err
		}

		_, err := view.Insert(ctx, event.ID, owner{Name: payload.Owner})
		return err
	})
}

// resumingProjection also remembers where it stopped.
type resumingProjection struct {
	architecturekit.Projection
	checkpoint string
}

func (p *resumingProjection) Checkpoint(context.Context) (string, error) {
	return p.checkpoint, nil
}

func (p *resumingProjection) SaveCheckpoint(_ context.Context, eventID string) error {
	p.checkpoint = eventID
	return nil
}

// brokenView cannot be read, which an in-memory view never fails at.
type brokenView struct{}

func (brokenView) All(context.Context) (iterSeq[owner], error) {
	return nil, errors.New("the view is unavailable")
}

func TestStoredEvent(t *testing.T) {
	t.Run("carries what a database would", func(t *testing.T) {
		stored := architecturekittest.StoredEvent("/account/1", "7", opened{Owner: "golo"})

		assert.Equal(t, "/account/1", stored.Subject)
		assert.Equal(t, "7", stored.ID)
		assert.Equal(t, (opened{}).EventType(), stored.Type)
		assert.Equal(t, `{"owner":"golo"}`, string(stored.Data))
		assert.NotEmpty(t, stored.SpecVersion, "the stored shape has to look complete")
		assert.NotEmpty(t, stored.DataContentType, "the stored shape has to look complete")
	})

	t.Run("panics on an event that cannot be marshalled", func(t *testing.T) {
		assert.Panics(t, func() {
			architecturekittest.StoredEvent("/account/1", "0", unmarshallable{Channel: make(chan int)})
		}, "such an event could never be stored")
	})
}

func TestStoredEvents(t *testing.T) {
	t.Run("numbers from zero", func(t *testing.T) {
		stored := architecturekittest.StoredEvents("/account/1",
			opened{Owner: "golo"}, closed{}, opened{Owner: "jane"})

		require.Len(t, stored, 3)
		for i, want := range []string{"0", "1", "2"} {
			assert.Equal(t, want, stored[i].ID)
		}
	})
}

func TestStoredEventsAt(t *testing.T) {
	t.Run("numbers from the given ID and times a minute apart", func(t *testing.T) {
		noon := time.Date(2026, time.March, 2, 12, 0, 0, 0, time.UTC)

		stored := architecturekittest.StoredEventsAt("/account/1", 7, noon,
			opened{Owner: "golo"}, closed{}, opened{Owner: "jane"})

		require.Len(t, stored, 3)
		for i, want := range []struct {
			id   string
			time time.Time
		}{
			{"7", noon},
			{"8", noon.Add(time.Minute)},
			{"9", noon.Add(2 * time.Minute)},
		} {
			assert.Equal(t, want.id, stored[i].ID)
			assert.Equal(t, want.time, stored[i].Time)
			assert.Equal(t, "/account/1", stored[i].Subject)
		}

		assert.Equal(t, "test.account.closed", stored[1].Type, "the events keep their order and types")
		assert.JSONEq(t, `{"owner":"jane"}`, string(stored[2].Data))
	})
}

func TestProject(t *testing.T) {
	t.Run("drives a projection", func(t *testing.T) {
		view := ownerView()

		architecturekittest.Project(t, ownerProjection(view),
			architecturekittest.StoredEvents("/account/1",
				opened{Owner: "golo"}, closed{}, opened{Owner: "jane"})...)

		architecturekittest.ExpectItems(t, view, owner{Name: "golo"}, owner{Name: "jane"})
	})

	t.Run("reports a refusal", func(t *testing.T) {
		recorder := &spy{}
		view := ownerView()

		// Data that does not match the payload makes the projection refuse.
		architecturekittest.Project(recorder, ownerProjection(view), eventsourcingdb.Event{
			Subject: "/account/1",
			Type:    (opened{}).EventType(),
			ID:      "0",
			Data:    []byte(`{"owner":42}`),
		})

		recorder.expectFailure(t, "projecting event 0")
	})

	t.Run("refuses a projection that is transactional as well", func(t *testing.T) {
		recorder := &spy{}
		table := &ownerTableWithApply{}

		architecturekittest.Project(recorder, table,
			architecturekittest.StoredEvent("/account/1", "0", opened{Owner: "golo"}))

		recorder.expectFailure(t, "is transactional, use ProjectTransactional")
		expectLog(t, table.log)
	})

	t.Run("drives a typed projection", func(t *testing.T) {
		view := ownerView()

		projection := architecturekit.NewProjection().
			On(func(ctx context.Context, event architecturekit.Envelope[opened]) error {
				_, err := view.Insert(ctx, event.ID, owner{Name: event.Data.Owner})
				return err
			})

		architecturekittest.Project(t, projection,
			architecturekittest.StoredEvents("/account/1",
				opened{Owner: "golo"}, closed{}, opened{Owner: "jane"})...)

		architecturekittest.ExpectItems(t, view, owner{Name: "golo"}, owner{Name: "jane"})
	})
}

func TestItemsOf(t *testing.T) {
	t.Run("reads a view", func(t *testing.T) {
		view := ownerView()
		insertOwner(t, view, "1", "golo")

		items := architecturekittest.ItemsOf(t, view)

		assert.Equal(t, []owner{{Name: "golo"}}, items)
	})

	t.Run("reports an unreadable view", func(t *testing.T) {
		recorder := &spy{}

		items := architecturekittest.ItemsOf(recorder, brokenView{})

		recorder.expectFailure(t, "reading the view")
		assert.Nil(t, items)
	})
}

func TestExpectMode(t *testing.T) {
	t.Run("accepts the right mode", func(t *testing.T) {
		view := ownerView()

		architecturekittest.ExpectMode(t, ownerProjection(view), architecturekit.ModeRebuild)
		architecturekittest.ExpectMode(t, &resumingProjection{Projection: ownerProjection(view)},
			architecturekit.ModeResumable)
	})

	t.Run("reports the wrong mode", func(t *testing.T) {
		recorder := &spy{}

		// A projection that keeps no checkpoint is driven in rebuild mode, so
		// expecting it to resume has to fail. This is the assertion that catches a
		// typo in an optional interface's method set.
		architecturekittest.ExpectMode(recorder, ownerProjection(ownerView()), architecturekit.ModeResumable)

		recorder.expectFailure(t, "runs in \"rebuild\" mode, want \"resumable\"")
	})
}

func TestExpectItems(t *testing.T) {
	t.Run("reports the wrong count", func(t *testing.T) {
		recorder := &spy{}
		view := ownerView()
		insertOwner(t, view, "2", "golo")

		architecturekittest.ExpectItems(recorder, view, owner{Name: "golo"}, owner{Name: "jane"})

		recorder.expectFailure(t, "expected 2 item(s), got 1")
	})

	t.Run("reports the wrong content", func(t *testing.T) {
		recorder := &spy{}
		view := ownerView()
		insertOwner(t, view, "3", "golo")

		architecturekittest.ExpectItems(recorder, view, owner{Name: "someone-else"})

		recorder.expectFailure(t, "someone-else")
	})
}

// ownerTable is a transactional projection: it applies events only within a
// transaction, and the transaction writes down what happens to it.
type ownerTable struct {
	log         []string
	beginErr    error
	commitErr   error
	rollbackErr error
}

func (o *ownerTable) Checkpoint(context.Context) (string, error) { return "", nil }

func (o *ownerTable) Begin(context.Context) (architecturekit.Tx, error) {
	if o.beginErr != nil {
		return nil, o.beginErr
	}
	o.log = append(o.log, "begin")
	return &ownerTx{owner: o}, nil
}

type ownerTx struct{ owner *ownerTable }

func (tx *ownerTx) Apply(_ context.Context, event eventsourcingdb.Event) error {
	if event.Type != (opened{}).EventType() {
		return errors.New("only openings, please")
	}
	tx.owner.log = append(tx.owner.log, "apply "+event.ID)
	return nil
}

func (tx *ownerTx) Commit(_ context.Context, lastEventID string) error {
	tx.owner.log = append(tx.owner.log, "commit "+lastEventID)
	return tx.owner.commitErr
}

func (tx *ownerTx) Rollback(context.Context) error {
	tx.owner.log = append(tx.owner.log, "rollback")
	return tx.owner.rollbackErr
}

// ownerTableWithApply is transactional, but has an Apply as well, which is
// not what runs in production.
type ownerTableWithApply struct {
	ownerTable
}

func (*ownerTableWithApply) Apply(context.Context, eventsourcingdb.Event) error { return nil }

func expectLog(t *testing.T, got []string, want ...string) {
	t.Helper()

	require.Len(t, got, len(want), "want %v", want)
	for i := range want {
		require.Equal(t, want[i], got[i], "step %d (full: %v)", i, got)
	}
}

func TestProjectTransactional(t *testing.T) {
	t.Run("applies within one transaction", func(t *testing.T) {
		table := &ownerTable{}

		architecturekittest.ProjectTransactional(t, table,
			architecturekittest.StoredEvents("/account/1",
				opened{Owner: "golo"}, opened{Owner: "jane"})...)

		expectLog(t, table.log, "begin", "apply 0", "apply 1", "commit 1")
	})

	t.Run("begins nothing without events", func(t *testing.T) {
		table := &ownerTable{}

		architecturekittest.ProjectTransactional(t, table)

		expectLog(t, table.log)
	})

	t.Run("rolls back on a refusal", func(t *testing.T) {
		recorder := &spy{}
		table := &ownerTable{rollbackErr: errors.New("rollback did not work either")}

		architecturekittest.ProjectTransactional(recorder, table,
			architecturekittest.StoredEvents("/account/1", opened{Owner: "golo"}, closed{})...)

		recorder.expectFailure(t, "projecting event 1")
		recorder.expectFailure(t, "rollback did not work either")
		expectLog(t, table.log, "begin", "apply 0", "rollback")
	})

	t.Run("reports a failing begin", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.ProjectTransactional(recorder, &ownerTable{beginErr: errors.New("no connection")},
			architecturekittest.StoredEvent("/account/1", "0", opened{Owner: "golo"}))

		recorder.expectFailure(t, "beginning a transaction: no connection")
	})

	t.Run("reports a failing commit", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.ProjectTransactional(recorder, &ownerTable{commitErr: errors.New("disk full")},
			architecturekittest.StoredEvent("/account/1", "0", opened{Owner: "golo"}))

		recorder.expectFailure(t, "committing the transaction: disk full")
	})

	t.Run("drives typed handlers built per transaction", func(t *testing.T) {
		table := &typedOwnerTable{}

		architecturekittest.ProjectTransactional(t, table,
			architecturekittest.StoredEvents("/account/1",
				opened{Owner: "golo"}, closed{}, opened{Owner: "jane"})...)

		expectLog(t, table.committed, "golo", "jane")
	})
}

// typedOwnerTable builds the handlers of every transaction anew, so that they
// write into that transaction.
type typedOwnerTable struct {
	committed []string
}

func (o *typedOwnerTable) Checkpoint(context.Context) (string, error) { return "", nil }

func (o *typedOwnerTable) Begin(context.Context) (architecturekit.Tx, error) {
	tx := &typedOwnerTx{table: o}
	tx.TypedProjection = architecturekit.NewProjection().
		On(func(_ context.Context, event architecturekit.Envelope[opened]) error {
			tx.pending = append(tx.pending, event.Data.Owner)
			return nil
		})

	return tx, nil
}

type typedOwnerTx struct {
	*architecturekit.TypedProjection
	table   *typedOwnerTable
	pending []string
}

func (tx *typedOwnerTx) Commit(context.Context, string) error {
	tx.table.committed = append(tx.table.committed, tx.pending...)
	return nil
}

func (tx *typedOwnerTx) Rollback(context.Context) error { return nil }
