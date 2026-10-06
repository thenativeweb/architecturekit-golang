package architecturekit_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// These tests run a transactional projection against a real PostgreSQL, built
// the way the README shows: Begin starts a database transaction and builds the
// handlers on it, and Commit writes the checkpoint into the same transaction.
// The primary key of the table refuses an event that is applied twice, so a
// repetition does not go unnoticed.

// incrementTable is a transactional projection that writes every increment
// into a table, and the ID of the last event into another one.
type incrementTable struct {
	db        *sql.DB
	batchSize int
	commits   atomic.Int32
}

func (p *incrementTable) CatchUpBatchSize() int { return p.batchSize }

func (p *incrementTable) Checkpoint(ctx context.Context) (string, error) {
	var eventID string

	err := p.db.QueryRowContext(ctx, `SELECT event_id FROM checkpoint`).Scan(&eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}

	return eventID, err
}

func (p *incrementTable) Begin(ctx context.Context) (architecturekit.Tx, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}

	return &incrementTableTx{
		owner: p,
		tx:    tx,
		TypedProjection: architecturekit.NewTypedProjection().
			On(func(ctx context.Context, event architecturekit.Envelope[incremented]) error {
				_, err := tx.ExecContext(ctx, `INSERT INTO increments (event_id, by) VALUES ($1, $2)`,
					event.ID, event.Data.By)
				return err
			}),
	}, nil
}

type incrementTableTx struct {
	*architecturekit.TypedProjection
	owner *incrementTable
	tx    *sql.Tx
}

func (tx *incrementTableTx) Commit(ctx context.Context, lastEventID string) error {
	_, err := tx.tx.ExecContext(ctx, `
		INSERT INTO checkpoint (id, event_id) VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET event_id = excluded.event_id`, lastEventID)
	if err != nil {
		return errors.Join(err, tx.tx.Rollback())
	}

	if err := tx.tx.Commit(); err != nil {
		return err
	}
	tx.owner.commits.Add(1)

	return nil
}

func (tx *incrementTableTx) Rollback(context.Context) error {
	return tx.tx.Rollback()
}

// startPostgres starts a PostgreSQL for the test and returns the address to
// connect to it.
func startPostgres(t *testing.T) string {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	container, err := testcontainers.Run(t.Context(), "postgres:18-alpine",
		testcontainers.WithEnv(map[string]string{
			"POSTGRES_USER":     "test",
			"POSTGRES_PASSWORD": "secret",
			"POSTGRES_DB":       "test",
		}),
		testcontainers.WithExposedPorts("5432/tcp"),
		// The server starts twice, once to initialise the database and once for
		// real, and only the second one stays.
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(time.Minute)),
	)
	testcontainers.CleanupContainer(t, container)
	require.NoError(t, err, "failed to start postgres")

	address, err := container.PortEndpoint(t.Context(), "5432/tcp", "")
	require.NoError(t, err)

	return "postgres://test:secret@" + address + "/test?sslmode=disable"
}

// schemas counts the schemas created so far, to name the next one.
var schemas atomic.Int32

// newIncrementTable creates the tables in a schema of their own, so that the
// tests do not see each other's rows, and returns a projection on them.
func newIncrementTable(t *testing.T, connection string, batchSize int) *incrementTable {
	t.Helper()

	schema := fmt.Sprintf("test_%d", schemas.Add(1))

	admin, err := sql.Open("pgx", connection)
	require.NoError(t, err)
	defer admin.Close()

	_, err = admin.ExecContext(t.Context(), fmt.Sprintf(`
		CREATE SCHEMA %[1]s;
		CREATE TABLE %[1]s.increments (event_id TEXT PRIMARY KEY, by INTEGER NOT NULL);
		CREATE TABLE %[1]s.checkpoint (id INTEGER PRIMARY KEY, event_id TEXT NOT NULL);`, schema))
	require.NoError(t, err)

	db, err := sql.Open("pgx", connection+"&search_path="+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	return &incrementTable{db: db, batchSize: batchSize}
}

// rowsIn counts the increments in the table.
func (p *incrementTable) rowsIn(t *testing.T) int {
	t.Helper()

	var count int
	require.NoError(t, p.db.QueryRowContext(t.Context(), `SELECT count(*) FROM increments`).Scan(&count))

	return count
}

// storedIDsOf reads the IDs of the events of the subject, in order.
func storedIDsOf(t *testing.T, subject string) []string {
	t.Helper()

	var ids []string
	for event, err := range rawClient(t).ReadEvents(t.Context(), subject, eventsourcingdb.ReadEventsOptions{}) {
		require.NoError(t, err)
		ids = append(ids, event.ID)
	}

	return ids
}

func TestTransactionalProjectionWithPostgres(t *testing.T) {
	store := requireStore(t)
	connection := startPostgres(t)

	t.Run("catching up commits rows and checkpoint together a batch at a time", func(t *testing.T) {
		subject := subjectFor(t)
		seed(t, subject, 250)
		ids := storedIDsOf(t, subject)

		table := newIncrementTable(t, connection, 100)
		require.NoError(t, architecturekit.CatchUpTransactionalProjection(t.Context(), store,
			architecturekit.ExactSubject(subject), table))

		assert.Equal(t, 250, table.rowsIn(t))
		checkpoint, err := table.Checkpoint(t.Context())
		require.NoError(t, err)
		assert.Equal(t, ids[249], checkpoint)
		assert.Equal(t, int32(3), table.commits.Load(), "two full batches, then the incomplete final one")
	})

	t.Run("a failure rolls back the batch and catching up again goes on from its checkpoint", func(t *testing.T) {
		subject := subjectFor(t)
		seed(t, subject, 250)
		ids := storedIDsOf(t, subject)

		table := newIncrementTable(t, connection, 100)

		// A row that is already there makes the insert of event 150 fail, in the
		// middle of the second batch.
		_, err := table.db.ExecContext(t.Context(), `INSERT INTO increments (event_id, by) VALUES ($1, 0)`, ids[150])
		require.NoError(t, err)

		err = architecturekit.CatchUpTransactionalProjection(t.Context(), store, architecturekit.ExactSubject(subject), table)
		require.ErrorContains(t, err, "duplicate key")

		checkpoint, err := table.Checkpoint(t.Context())
		require.NoError(t, err)
		assert.Equal(t, ids[99], checkpoint, "the checkpoint of the first batch")
		assert.Equal(t, 100+1, table.rowsIn(t), "the rows of the first batch and the one in the way, none of the second")

		_, err = table.db.ExecContext(t.Context(), `DELETE FROM increments WHERE event_id = $1`, ids[150])
		require.NoError(t, err)

		// Had the second batch left rows behind, or did catching up start over,
		// the primary key would refuse them now.
		require.NoError(t, architecturekit.CatchUpTransactionalProjection(t.Context(), store,
			architecturekit.ExactSubject(subject), table))

		assert.Equal(t, 250, table.rowsIn(t))
		checkpoint, err = table.Checkpoint(t.Context())
		require.NoError(t, err)
		assert.Equal(t, ids[249], checkpoint)
	})

	t.Run("a restart resumes from the checkpoint", func(t *testing.T) {
		subject := subjectFor(t)
		seed(t, subject, 3)

		table := newIncrementTable(t, connection, 100)
		require.NoError(t, architecturekit.CatchUpTransactionalProjection(t.Context(), store,
			architecturekit.ExactSubject(subject), table))

		seed(t, subject, 2)

		restarted := &incrementTable{db: table.db, batchSize: 100}
		require.NoError(t, architecturekit.CatchUpTransactionalProjection(t.Context(), store,
			architecturekit.ExactSubject(subject), restarted))

		assert.Equal(t, 5, table.rowsIn(t))
		assert.Equal(t, int32(1), restarted.commits.Load(), "only the two new events")
	})

	t.Run("live events are committed at once whatever the batch size", func(t *testing.T) {
		subject := subjectFor(t)
		seed(t, subject, 3)

		table := newIncrementTable(t, connection, 100)

		ctx, cancel := context.WithCancel(t.Context())
		run := architecturekit.StartTransactionalProjection(ctx, store, architecturekit.ExactSubject(subject), table)
		t.Cleanup(func() { cancel(); <-run.Done() })

		select {
		case <-run.CaughtUp():
		case <-time.After(5 * time.Second):
			require.FailNow(t, "the projection did not catch up")
		}
		assert.Equal(t, 3, table.rowsIn(t))

		seed(t, subject, 1)
		ids := storedIDsOf(t, subject)

		// Another connection only sees the row once it is committed.
		waitFor(t, func() bool { return table.rowsIn(t) == 4 })
		checkpoint, err := table.Checkpoint(t.Context())
		require.NoError(t, err)
		assert.Equal(t, ids[3], checkpoint)

		// A batch that waited to fill up would keep its transaction open in
		// between.
		var open int
		require.NoError(t, table.db.QueryRowContext(t.Context(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND state LIKE 'idle in transaction%'`).Scan(&open))
		assert.Zero(t, open, "no transaction may stay open while the projection waits for events")

		cancel()
		<-run.Done()
		assert.NoError(t, run.Err())
	})
}
