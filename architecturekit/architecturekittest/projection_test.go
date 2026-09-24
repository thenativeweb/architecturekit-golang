package architecturekittest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/architecturekittest"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// --- a view and a projection to drive ---

type owner struct {
	Name string
}

func ownerView() *architecturekit.ItemView[owner] {
	return architecturekit.NewItemView[owner]()
}

func ownerProjection(view *architecturekit.ItemView[owner]) architecturekit.Projection {
	return architecturekit.ProjectionFunc(func(_ context.Context, event eventsourcingdb.Event) error {
		if event.Type != (opened{}).EventType() {
			return nil
		}

		var payload opened
		if err := jsonUnmarshal(event.Data, &payload); err != nil {
			return err
		}

		view.Insert(owner{Name: payload.Owner})

		return nil
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

func TestStoredEventCarriesWhatADatabaseWould(t *testing.T) {
	stored := architecturekittest.StoredEvent("/account/1", "7", opened{Owner: "golo"})

	if stored.Subject != "/account/1" {
		t.Fatalf("got %q", stored.Subject)
	}
	if stored.ID != "7" {
		t.Fatalf("got %q", stored.ID)
	}
	if stored.Type != (opened{}).EventType() {
		t.Fatalf("got %q", stored.Type)
	}
	if string(stored.Data) != `{"owner":"golo"}` {
		t.Fatalf("got %s", stored.Data)
	}
	if stored.SpecVersion == "" || stored.DataContentType == "" {
		t.Fatalf("the stored shape has to look complete: %+v", stored)
	}
}

func TestStoredEventsNumbersFromZero(t *testing.T) {
	stored := architecturekittest.StoredEvents("/account/1",
		opened{Owner: "golo"}, closed{}, opened{Owner: "jane"})

	if len(stored) != 3 {
		t.Fatalf("got %d", len(stored))
	}
	for i, want := range []string{"0", "1", "2"} {
		if stored[i].ID != want {
			t.Fatalf("event %d: got id %q, want %q", i, stored[i].ID, want)
		}
	}
}

func TestStoredEventPanicsOnAnEventThatCannotBeMarshalled(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic, because such an event could never be stored")
		}
	}()

	architecturekittest.StoredEvent("/account/1", "0", unmarshallable{Channel: make(chan int)})
}

func TestProjectDrivesAProjection(t *testing.T) {
	view := ownerView()

	architecturekittest.Project(t, ownerProjection(view),
		architecturekittest.StoredEvents("/account/1",
			opened{Owner: "golo"}, closed{}, opened{Owner: "jane"})...)

	architecturekittest.ExpectItems(t, view, owner{Name: "golo"}, owner{Name: "jane"})
}

func TestProjectReportsARefusal(t *testing.T) {
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
}

func TestItemsOfReadsAView(t *testing.T) {
	view := ownerView()
	view.Insert(owner{Name: "golo"})

	items := architecturekittest.ItemsOf(t, view)

	if len(items) != 1 || items[0].Name != "golo" {
		t.Fatalf("got %v", items)
	}
}

func TestItemsOfReportsAnUnreadableView(t *testing.T) {
	recorder := &spy{}

	items := architecturekittest.ItemsOf(recorder, brokenView{})

	recorder.expectFailure(t, "reading the view")
	if items != nil {
		t.Fatalf("got %v", items)
	}
}

func TestExpectModeAcceptsTheRightMode(t *testing.T) {
	view := ownerView()

	architecturekittest.ExpectMode(t, ownerProjection(view), architecturekit.ModeRebuild)
	architecturekittest.ExpectMode(t, &resumingProjection{Projection: ownerProjection(view)},
		architecturekit.ModeResumable)
}

func TestExpectModeReportsTheWrongMode(t *testing.T) {
	recorder := &spy{}

	// A projection that keeps no checkpoint is driven in rebuild mode, so
	// expecting it to resume has to fail. This is the assertion that catches a
	// typo in an optional interface's method set.
	architecturekittest.ExpectMode(recorder, ownerProjection(ownerView()), architecturekit.ModeResumable)

	recorder.expectFailure(t, "runs in \"rebuild\" mode, want \"resumable\"")
}

func TestExpectItemsReportsTheWrongCount(t *testing.T) {
	recorder := &spy{}
	view := ownerView()
	view.Insert(owner{Name: "golo"})

	architecturekittest.ExpectItems(recorder, view, owner{Name: "golo"}, owner{Name: "jane"})

	recorder.expectFailure(t, "expected 2 item(s), got 1")
}

func TestExpectItemsReportsTheWrongContent(t *testing.T) {
	recorder := &spy{}
	view := ownerView()
	view.Insert(owner{Name: "golo"})

	architecturekittest.ExpectItems(recorder, view, owner{Name: "someone-else"})

	recorder.expectFailure(t, "someone-else")
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

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step %d: got %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestProjectTransactionalAppliesWithinOneTransaction(t *testing.T) {
	table := &ownerTable{}

	architecturekittest.ProjectTransactional(t, table,
		architecturekittest.StoredEvents("/account/1",
			opened{Owner: "golo"}, opened{Owner: "jane"})...)

	expectLog(t, table.log, "begin", "apply 0", "apply 1", "commit 1")
}

func TestProjectTransactionalBeginsNothingWithoutEvents(t *testing.T) {
	table := &ownerTable{}

	architecturekittest.ProjectTransactional(t, table)

	expectLog(t, table.log)
}

func TestProjectTransactionalRollsBackOnARefusal(t *testing.T) {
	recorder := &spy{}
	table := &ownerTable{rollbackErr: errors.New("rollback did not work either")}

	architecturekittest.ProjectTransactional(recorder, table,
		architecturekittest.StoredEvents("/account/1", opened{Owner: "golo"}, closed{})...)

	recorder.expectFailure(t, "projecting event 1")
	recorder.expectFailure(t, "rollback did not work either")
	expectLog(t, table.log, "begin", "apply 0", "rollback")
}

func TestProjectTransactionalReportsAFailingBegin(t *testing.T) {
	recorder := &spy{}

	architecturekittest.ProjectTransactional(recorder, &ownerTable{beginErr: errors.New("no connection")},
		architecturekittest.StoredEvent("/account/1", "0", opened{Owner: "golo"}))

	recorder.expectFailure(t, "beginning a transaction: no connection")
}

func TestProjectTransactionalReportsAFailingCommit(t *testing.T) {
	recorder := &spy{}

	architecturekittest.ProjectTransactional(recorder, &ownerTable{commitErr: errors.New("disk full")},
		architecturekittest.StoredEvent("/account/1", "0", opened{Owner: "golo"}))

	recorder.expectFailure(t, "committing the transaction: disk full")
}

func TestProjectRefusesAProjectionThatIsTransactionalAsWell(t *testing.T) {
	recorder := &spy{}
	table := &ownerTableWithApply{}

	architecturekittest.Project(recorder, table,
		architecturekittest.StoredEvent("/account/1", "0", opened{Owner: "golo"}))

	recorder.expectFailure(t, "is transactional, use ProjectTransactional")
	expectLog(t, table.log)
}
