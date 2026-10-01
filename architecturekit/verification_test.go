package architecturekit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// databaseVerificationKey is the key the test database signs with. It skips
// the test while no database is running.
func databaseVerificationKey(t *testing.T) ed25519.PublicKey {
	t.Helper()

	rawClient(t)

	return testVerificationKey
}

// anotherVerificationKey is a key the database did not sign with.
func anotherVerificationKey(t *testing.T) ed25519.PublicKey {
	t.Helper()

	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	return publicKey
}

func verifyingStore(t *testing.T, options ...architecturekit.StoreOption) *architecturekit.Store {
	t.Helper()

	return architecturekit.NewStore(rawClient(t), "https://thenativeweb.io", options...)
}

func expectUnverified(t *testing.T, err error) {
	t.Helper()

	require.ErrorIs(t, err, architecturekit.ErrUnverified)
	require.ErrorIs(t, err, architecturekit.ErrPermanent, "ErrUnverified is permanent")
}

// The fake database hands out events without any signatures, which is what an
// unsigned database looks like, and, if asked to, with hashes that do not match,
// which is what a tampered one looks like.

func TestSignatureVerification(t *testing.T) {
	t.Run("accepts what the database signed", func(t *testing.T) {
		store := verifyingStore(t, architecturekit.WithSignatureVerification(databaseVerificationKey(t)))
		subject := subjectFor(t)

		for range 2 {
			// The second command reads, and so verifies, the event of the first.
			_, err := architecturekit.Execute(context.Background(), store, counterDecider(),
				increment{subject: subject, By: 1})
			require.NoError(t, err)
		}

		current, err := architecturekit.Load(context.Background(), store, counterState(), subject)
		require.NoError(t, err)
		assert.Equal(t, 2, current.Total)

		target := &collector{}
		require.NoError(t, architecturekit.CatchUpProjection(context.Background(), store, subject, false, target))
		assert.Len(t, target.IDs(), 2)
	})

	t.Run("rejects another key", func(t *testing.T) {
		store := verifyingStore(t, architecturekit.WithSignatureVerification(anotherVerificationKey(t)))
		subject := subjectFor(t)
		seed(t, subject, 1)

		_, err := architecturekit.Load(context.Background(), store, counterState(), subject)
		expectUnverified(t, err)

		_, err = architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: subject, By: 1})
		expectUnverified(t, err)

		// Nothing was written, because the command was never decided on.
		current, err := architecturekit.Load(context.Background(), requireStore(t), counterState(), subject)
		require.NoError(t, err)
		assert.Equal(t, 1, current.Total, "want the seeded 1")

		err = architecturekit.CatchUpProjection(context.Background(), store, subject, false, &collector{})
		expectUnverified(t, err)
	})

	t.Run("rejects a missing signature", func(t *testing.T) {
		client := newFakeDatabase(t, &fakeDatabase{events: []int{0}})
		store := architecturekit.NewStore(client, "https://thenativeweb.io",
			architecturekit.WithSignatureVerification(anotherVerificationKey(t)))

		_, err := architecturekit.Load(context.Background(), store, counterState(), "/test")

		expectUnverified(t, err)
		assert.ErrorContains(t, err, "signature", "the error has to name the missing signature")
	})
}

func TestHashVerification(t *testing.T) {
	t.Run("accepts what the database stored", func(t *testing.T) {
		store := verifyingStore(t)
		subject := subjectFor(t)
		seed(t, subject, 2)

		current, err := architecturekit.Load(context.Background(), store, counterState(), subject)
		require.NoError(t, err)
		assert.Equal(t, 2, current.Total)
	})

	t.Run("accepts the events of the fake database", func(t *testing.T) {
		// The counterpart to the cases below: the fake computes its hashes the
		// way the database does, so only tampering makes them fail.
		client := newFakeDatabase(t, &fakeDatabase{events: []int{0, 1, 2}})
		store := architecturekit.NewStore(client, "https://thenativeweb.io")

		current, err := architecturekit.Load(context.Background(), store, counterState(), "/test")
		require.NoError(t, err)
		assert.Equal(t, 3, current.Total)
	})

	t.Run("rejects a hash that does not match without being asked to", func(t *testing.T) {
		client := newFakeDatabase(t, &fakeDatabase{events: []int{0}, tampered: true})
		store := architecturekit.NewStore(client, "https://thenativeweb.io")

		_, err := architecturekit.Load(context.Background(), store, counterState(), "/test")

		expectUnverified(t, err)
		assert.ErrorContains(t, err, `event 0 on "/test"`, "the error has to name the event and its subject")
	})

	t.Run("accepts a hash that does not match if turned off", func(t *testing.T) {
		client := newFakeDatabase(t, &fakeDatabase{events: []int{0, 1}, tampered: true})
		store := architecturekit.NewStore(client, "https://thenativeweb.io",
			architecturekit.WithoutHashVerification())

		current, err := architecturekit.Load(context.Background(), store, counterState(), "/test")
		require.NoError(t, err)
		assert.Equal(t, 2, current.Total)
	})
}

func TestWithoutHashVerification(t *testing.T) {
	t.Run("panics together with signature verification, in either order", func(t *testing.T) {
		client := newFakeDatabase(t, &fakeDatabase{})
		signatures := architecturekit.WithSignatureVerification(anotherVerificationKey(t))
		withoutHashes := architecturekit.WithoutHashVerification()

		assert.PanicsWithValue(t,
			"architecturekit: WithoutHashVerification contradicts WithSignatureVerification, which checks the hash as well",
			func() { architecturekit.NewStore(client, "https://thenativeweb.io", withoutHashes, signatures) })
		assert.Panics(t,
			func() { architecturekit.NewStore(client, "https://thenativeweb.io", signatures, withoutHashes) })
	})
}

func TestExecuteWithVerification(t *testing.T) {
	t.Run("does not verify the events it writes", func(t *testing.T) {
		store := verifyingStore(t, architecturekit.WithSignatureVerification(anotherVerificationKey(t)))

		// On a pristine subject, there is nothing to read, and so nothing to verify.
		written, err := architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: subjectFor(t), By: 1})
		require.NoError(t, err, "the written events must not be verified")
		assert.Len(t, written, 1)
	})
}

func TestVerification(t *testing.T) {
	t.Run("runs before the upcasters", func(t *testing.T) {
		store := verifyingStore(t, architecturekit.WithSignatureVerification(databaseVerificationKey(t)))
		subject := subjectFor(t)

		_, err := rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{{
			Source:  "https://thenativeweb.io",
			Subject: subject,
			Type:    "io.thenativeweb.test.outdated",
			Data:    map[string]any{"amount": 5},
		}}, nil)
		require.NoError(t, err)

		// The upcaster changes the data, which would no longer match the hash if
		// it ran before the verification.
		upcasters := architecturekit.NewUpcasters().
			Upcast("io.thenativeweb.test.outdated",
				func(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
					event.Type = (incremented{}).EventType()
					event.Data = json.RawMessage(`{"by":5}`)
					return []eventsourcingdb.Event{event}, nil
				})

		current, err := architecturekit.Load(context.Background(), store,
			counterState().UpcastWith(upcasters), subject)
		require.NoError(t, err)
		assert.Equal(t, 5, current.Total)

		total := 0
		projection := architecturekit.NewProjection().
			UpcastWith(upcasters).
			On(func(_ context.Context, event architecturekit.Envelope[incremented]) error {
				total += event.Data.By
				return nil
			})

		require.NoError(t, architecturekit.CatchUpProjection(context.Background(), store, subject, false, projection))
		assert.Equal(t, 5, total)
	})
}

func TestRunProjectionWithVerification(t *testing.T) {
	t.Run("ends on an event signed with another key", func(t *testing.T) {
		subject := subjectFor(t)
		seed(t, subject, 1)

		observed := &reconnects{}
		store := verifyingStore(t,
			architecturekit.WithSignatureVerification(anotherVerificationKey(t)),
			architecturekit.WithReconnectObserver(observed.observe),
		)

		err := architecturekit.RunProjection(context.Background(), store, subject, false, &collector{})

		expectUnverified(t, err)
		assert.Zero(t, observed.count(), "an unverified event must not be retried")
	})

	t.Run("ends on an unverified event without retrying", func(t *testing.T) {
		database := &fakeDatabase{
			events:       []int{0, 1},
			endObserving: func(int) bool { return true },
			tampered:     true,
		}
		observed := &reconnects{}
		store := architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io",
			architecturekit.WithReconnectObserver(observed.observe),
		)
		target := &collector{}

		// Without the verification, the run would follow the stream forever,
		// so the deadline turns that into a failure rather than a hang.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := architecturekit.RunProjection(ctx, store, "/test", false, target)

		expectUnverified(t, err)
		assert.Zero(t, observed.count(), "an unverified event must not be retried")
		assert.Empty(t, target.IDs(), "an unverified event must not be applied")
	})
}

func TestWithSignatureVerification(t *testing.T) {
	t.Run("panics on a key of the wrong length", func(t *testing.T) {
		assert.Panics(t, func() {
			architecturekit.WithSignatureVerification(ed25519.PublicKey("too short"))
		})
	})
}
