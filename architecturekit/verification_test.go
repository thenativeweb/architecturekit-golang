package architecturekit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"

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
	if err != nil {
		t.Fatalf("failed to generate a key: %v", err)
	}

	return publicKey
}

func verifyingStore(t *testing.T, options ...architecturekit.StoreOption) *architecturekit.Store {
	t.Helper()

	return architecturekit.NewStore(rawClient(t), "https://thenativeweb.io", options...)
}

func expectUnverified(t *testing.T, err error) {
	t.Helper()

	if !errors.Is(err, architecturekit.ErrUnverified) {
		t.Fatalf("expected ErrUnverified, got %v", err)
	}
	if !errors.Is(err, architecturekit.ErrPermanent) {
		t.Fatalf("ErrUnverified is permanent, got %v", err)
	}
}

func TestSignatureVerificationAcceptsWhatTheDatabaseSigned(t *testing.T) {
	store := verifyingStore(t, architecturekit.WithSignatureVerification(databaseVerificationKey(t)))
	subject := subjectFor(t)

	for range 2 {
		// The second command reads, and so verifies, the event of the first.
		if _, err := architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: subject, By: 1}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	current, err := architecturekit.Load(context.Background(), store, counterState(), subject)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if current.Total != 2 {
		t.Fatalf("got %d, want 2", current.Total)
	}

	target := &collector{}
	if err := architecturekit.CatchUpProjection(context.Background(), store, subject, false, target); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(target.IDs()) != 2 {
		t.Fatalf("got %d events, want 2", len(target.IDs()))
	}
}

func TestHashVerificationAcceptsWhatTheDatabaseStored(t *testing.T) {
	store := verifyingStore(t, architecturekit.WithHashVerification())
	subject := subjectFor(t)
	seed(t, subject, 2)

	current, err := architecturekit.Load(context.Background(), store, counterState(), subject)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if current.Total != 2 {
		t.Fatalf("got %d, want 2", current.Total)
	}
}

func TestSignatureVerificationRejectsAnotherKey(t *testing.T) {
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if current.Total != 1 {
		t.Fatalf("got %d, want the seeded 1", current.Total)
	}

	err = architecturekit.CatchUpProjection(context.Background(), store, subject, false, &collector{})
	expectUnverified(t, err)
}

func TestExecuteDoesNotVerifyTheEventsItWrites(t *testing.T) {
	store := verifyingStore(t, architecturekit.WithSignatureVerification(anotherVerificationKey(t)))

	// On a pristine subject, there is nothing to read, and so nothing to verify.
	written, err := architecturekit.Execute(context.Background(), store, counterDecider(),
		increment{subject: subjectFor(t), By: 1})
	if err != nil {
		t.Fatalf("the written events must not be verified, got %v", err)
	}
	if len(written) != 1 {
		t.Fatalf("got %d events, want 1", len(written))
	}
}

func TestVerificationRunsBeforeTheUpcasters(t *testing.T) {
	store := verifyingStore(t, architecturekit.WithSignatureVerification(databaseVerificationKey(t)))
	subject := subjectFor(t)

	_, err := rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{{
		Source:  "https://thenativeweb.io",
		Subject: subject,
		Type:    "io.thenativeweb.test.outdated",
		Data:    map[string]any{"amount": 5},
	}}, nil)
	if err != nil {
		t.Fatalf("failed to seed the stream: %v", err)
	}

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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if current.Total != 5 {
		t.Fatalf("got %d, want 5", current.Total)
	}

	total := 0
	projection := architecturekit.NewProjection().
		UpcastWith(upcasters).
		On(func(_ context.Context, event architecturekit.Envelope[incremented]) error {
			total += event.Data.By
			return nil
		})

	if err := architecturekit.CatchUpProjection(context.Background(), store, subject, false, projection); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 5 {
		t.Fatalf("got %d, want 5", total)
	}
}

func TestRunProjectionEndsOnAnEventSignedWithAnotherKey(t *testing.T) {
	subject := subjectFor(t)
	seed(t, subject, 1)

	observed := &reconnects{}
	store := verifyingStore(t,
		architecturekit.WithSignatureVerification(anotherVerificationKey(t)),
		architecturekit.WithReconnectObserver(observed.observe),
	)

	err := architecturekit.RunProjection(context.Background(), store, subject, false, &collector{})

	expectUnverified(t, err)
	if observed.count() != 0 {
		t.Fatalf("an unverified event must not be retried, got %d attempts", observed.count())
	}
}

// The fake database hands out events with made-up hashes and without any
// signatures, which is what a tampered or unsigned database looks like.

func TestHashVerificationRejectsAHashThatDoesNotMatch(t *testing.T) {
	client := newFakeDatabase(t, &fakeDatabase{events: []int{0}})
	store := architecturekit.NewStore(client, "https://thenativeweb.io", architecturekit.WithHashVerification())

	_, err := architecturekit.Load(context.Background(), store, counterState(), "/test")

	expectUnverified(t, err)
	if !strings.Contains(err.Error(), `event 0 on "/test"`) {
		t.Fatalf("the error has to name the event and its subject, got %q", err.Error())
	}
}

func TestSignatureVerificationRejectsAMissingSignature(t *testing.T) {
	client := newFakeDatabase(t, &fakeDatabase{events: []int{0}})
	store := architecturekit.NewStore(client, "https://thenativeweb.io",
		architecturekit.WithSignatureVerification(anotherVerificationKey(t)))

	_, err := architecturekit.Load(context.Background(), store, counterState(), "/test")

	expectUnverified(t, err)
	if !strings.Contains(err.Error(), "signature") {
		t.Fatalf("the error has to name the missing signature, got %q", err.Error())
	}
}

func TestRunProjectionEndsOnAnUnverifiedEventWithoutRetrying(t *testing.T) {
	database := &fakeDatabase{
		events:       []int{0, 1},
		endObserving: func(int) bool { return true },
	}
	observed := &reconnects{}
	store := architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io",
		architecturekit.WithHashVerification(),
		architecturekit.WithReconnectObserver(observed.observe),
	)
	target := &collector{}

	err := architecturekit.RunProjection(context.Background(), store, "/test", false, target)

	expectUnverified(t, err)
	if observed.count() != 0 {
		t.Fatalf("an unverified event must not be retried, got %d attempts", observed.count())
	}
	if len(target.IDs()) != 0 {
		t.Fatalf("an unverified event must not be applied, got %v", target.IDs())
	}
}

func TestWithSignatureVerificationPanicsOnAKeyOfTheWrongLength(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for a key of the wrong length")
		}
	}()

	architecturekit.WithSignatureVerification(ed25519.PublicKey("too short"))
}
