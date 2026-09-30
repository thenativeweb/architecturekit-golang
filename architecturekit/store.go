package architecturekit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// Store reads from and writes to the EventSourcingDB.
type Store struct {
	client *eventsourcingdb.Client
	source string

	// states is nil unless the store was created with WithStateCache.
	states *stateCache

	// The reconnect settings apply to RunProjection and
	// RunTransactionalProjection.
	reconnectInitialDelay time.Duration
	reconnectMaxDelay     time.Duration
	reconnectObserver     func(err error, delay time.Duration)

	// verifiesHashes and verificationKey are set by WithHashVerification and
	// WithSignatureVerification. A verification key implies checking hashes.
	verifiesHashes  bool
	verificationKey ed25519.PublicKey
}

// The default delays of RunProjection before it observes again, for a store
// without WithReconnectDelays.
const (
	defaultReconnectInitialDelay = 1 * time.Second
	defaultReconnectMaxDelay     = 1 * time.Minute
)

// StoreOption configures a store.
type StoreOption func(*Store)

// WithStateCache keeps the states of the most recently used subjects in
// memory, up to the given number, so that the next command on one of them
// reads only the events written since. Values below 1 count as 1.
//
// Only states that consist of values, or that have a Clone function, are
// cached (see State.Clone). The cache is correct with several processes
// writing to the same subjects, because every command still reads all events
// after the ones it has cached.
//
// The cache tells states apart by their type, so a state that is built anew
// for every command is cached as well. Two different states that read the
// same subject need two different types: if two states of the same type
// differ in how they are built, reading fails with ErrPermanent.
func WithStateCache(maxSubjects int) StoreOption {
	return func(store *Store) {
		store.states = newStateCache(maxSubjects)
	}
}

// WithReconnectDelays sets how long RunProjection and
// RunTransactionalProjection wait before they read again after reading failed
// or the stream ended. The delay starts at initialDelay, doubles with every
// attempt in a row, and never exceeds maxDelay. It starts over once a
// projection has made progress. Without this option, the delays are 1 second
// and 1 minute.
func WithReconnectDelays(initialDelay, maxDelay time.Duration) StoreOption {
	return func(store *Store) {
		store.reconnectInitialDelay = initialDelay
		store.reconnectMaxDelay = max(maxDelay, initialDelay)
	}
}

// WithReconnectObserver calls observe every time RunProjection or
// RunTransactionalProjection is about to wait before reading again, with the
// reason and the delay. The reason is nil if the database ended the stream.
// Use it to log, since the kit itself does not.
func WithReconnectObserver(observe func(err error, delay time.Duration)) StoreOption {
	return func(store *Store) {
		store.reconnectObserver = observe
	}
}

// WithHashVerification checks the hash of every event the store reads, before
// any upcaster, Evolve rule, or projection sees it. An event whose hash does
// not match its content makes reading fail with ErrUnverified.
//
// This applies to Execute, Load, and every kind of projection. The events that
// Execute has just written are not checked, since they are not read.
func WithHashVerification() StoreOption {
	return func(store *Store) {
		store.verifiesHashes = true
	}
}

// WithSignatureVerification checks the hash and the signature of every event
// the store reads, like WithHashVerification, against the verification key of
// the database. An event without a signature, or with one that does not match
// the key, makes reading fail with ErrUnverified.
//
// The database signs events only if it runs with a signing key, and it signs
// them when handing them out, with the key it has at that moment. After the key
// is rotated, hand over the new verification key.
//
// A key of the wrong length is a programming error, so it panics.
func WithSignatureVerification(verificationKey ed25519.PublicKey) StoreOption {
	if len(verificationKey) != ed25519.PublicKeySize {
		panic(fmt.Sprintf("architecturekit: a verification key needs %d bytes, not %d",
			ed25519.PublicKeySize, len(verificationKey)))
	}

	return func(store *Store) {
		store.verifiesHashes = true
		store.verificationKey = verificationKey
	}
}

// NewStore creates a store that writes events with the given source.
func NewStore(client *eventsourcingdb.Client, source string, options ...StoreOption) *Store {
	store := &Store{
		client:                client,
		source:                source,
		reconnectInitialDelay: defaultReconnectInitialDelay,
		reconnectMaxDelay:     defaultReconnectMaxDelay,
	}
	for _, option := range options {
		option(store)
	}

	return store
}

// Load reads the state of a subject exactly the way Execute does before it
// decides, including the state cache of the store. Use it for a query that
// needs the state of a single subject, rather than a view across many.
func Load[TState any](
	ctx context.Context,
	store *Store,
	state *State[TState],
	subject string,
) (TState, error) {
	current, _, err := fold(ctx, store, subject, state)
	return current, err
}

// fold reads the stream and folds it into a state as it goes. Events are not
// collected, so even long streams need constant memory only.
//
// With a state cache, it continues from the cached state and reads only the
// events after the one the state was built from.
//
// It also returns the ID of the last event the state was built from, which is
// empty for a subject without events.
func fold[TState any](
	ctx context.Context,
	store *Store,
	subject string,
	state *State[TState],
) (TState, string, error) {
	current := state.initial
	lastEventID := ""

	isCached := store.states != nil && state.isCopyable()
	key := stateCacheKey{stateType: reflect.TypeFor[TState](), subject: subject}

	var shape stateShape
	options := eventsourcingdb.ReadEventsOptions{Recursive: false}

	if isCached {
		shape = shapeOf(state)

		if entry, isFound := store.states.get(key); isFound {
			if !entry.shape.equals(shape) {
				return current, "", fmt.Errorf("%w: two different states of type %v read %q; "+
					"give them different types to cache them", ErrPermanent, key.stateType, subject)
			}

			current = state.copyOf(entry.state.(TState))
			lastEventID = entry.lastEventID
			options.LowerBound = boundAfter(lastEventID)
		}
	}

	if options.LowerBound == nil && state.fromLatest != "" {
		options.FromLatestEvent = &eventsourcingdb.ReadFromLatestEvent{
			Subject:          subject,
			Type:             state.fromLatest,
			IfEventIsMissing: eventsourcingdb.ReadEverythingIfEventIsMissing,
		}
	}

	for event, err := range store.client.ReadEvents(ctx, subject, options) {
		if err != nil {
			return current, "", databaseFailure(err, fmt.Sprintf("reading %q", subject))
		}

		if err := store.verify(event); err != nil {
			return current, "", err
		}

		upcasted, err := state.upcasters.apply(event)
		if err != nil {
			return current, "", err
		}

		for _, event := range upcasted {
			evolve, isKnown := state.evolve[event.Type]
			if !isKnown {
				// An unexpected event type in an aggregate's stream points to a wrong
				// subject or a missing rule. Skipping it silently would hide that.
				return current, "", fmt.Errorf("%w: no rule for event type %q on subject %q",
					ErrPermanent, event.Type, subject)
			}

			current, err = evolve(current, event.Data)
			if err != nil {
				return current, "", err
			}
		}

		lastEventID = event.ID
	}

	// The cache gets a copy, so that the caller can not change what the cache
	// holds.
	if isCached && lastEventID != "" {
		store.states.put(key, shape, state.copyOf(current), lastEventID)
	}

	return current, lastEventID, nil
}

// verify checks an event the store has read, as far as the store was told to
// with WithHashVerification or WithSignatureVerification. It has to run on the
// event as stored, before any upcaster changes it.
func (s *Store) verify(event eventsourcingdb.Event) error {
	var err error

	switch {
	case s.verificationKey != nil:
		err = event.VerifySignature(s.verificationKey)
	case s.verifiesHashes:
		err = event.VerifyHash()
	}

	if err != nil {
		return fmt.Errorf("%w: event %s on %q: %v", ErrUnverified, event.ID, event.Subject, err)
	}

	return nil
}

// write appends the events under the given preconditions and returns them as
// the database recorded them, including their IDs.
func (s *Store) write(
	subject string,
	events []Event,
	preconditions []eventsourcingdb.Precondition,
) ([]eventsourcingdb.Event, error) {
	candidates := make([]eventsourcingdb.EventCandidate, len(events))
	for i, event := range events {
		candidates[i] = eventsourcingdb.EventCandidate{
			Source:  s.source,
			Subject: subject,
			Type:    event.EventType(),
			Data:    event,
		}
	}

	written, err := s.client.WriteEvents(candidates, preconditions)
	if err != nil {
		return nil, databaseFailure(err, fmt.Sprintf("writing %q", subject))
	}

	return written, nil
}

// RegisterSchemas registers the schemas of the given events with the database.
// It is meant to be called on every start: for an event type the database
// knows already, it checks that the registered schema is the given one.
//
// A registered schema cannot change. If it differs from the given one, or if
// the database refuses the given one, e.g. because stored events of the type
// do not match it, RegisterSchemas fails permanently. To change the shape of
// an event, introduce a new event type and an upcaster instead.
func (s *Store) RegisterSchemas(schemas ...[]EventSchema) error {
	given, err := collectSchemas(schemas)
	if err != nil {
		return err
	}

	registered, err := s.readRegisteredSchemas()
	if err != nil {
		return err
	}

	for _, schema := range given {
		current, isRegistered := registered[schema.EventType]

		if !isRegistered {
			refusal := s.client.RegisterEventSchema(schema.EventType, schema.Schema)
			if refusal == nil {
				continue
			}
			if statusCodeOf(refusal) != http.StatusConflict {
				return databaseFailure(refusal, fmt.Sprintf("registering schema for %q", schema.EventType))
			}

			// Either another instance has registered the schema in the
			// meantime, or the database refused it, e.g. because stored events
			// of the type do not match it. Only the registered schemas tell the
			// two apart.
			registered, err = s.readRegisteredSchemas()
			if err != nil {
				return err
			}

			current, isRegistered = registered[schema.EventType]
			if !isRegistered {
				return fmt.Errorf("%w: the database refused the schema of %q: %v", ErrPermanent, schema.EventType, refusal)
			}
		}

		if err := checkRegisteredSchema(schema, current); err != nil {
			return err
		}
	}

	return nil
}

// collectSchemas flattens the given groups, leaving out event types that
// appear more than once with the same schema.
func collectSchemas(groups [][]EventSchema) ([]EventSchema, error) {
	var collected []EventSchema
	seen := map[string]map[string]any{}

	for _, group := range groups {
		for _, schema := range group {
			if schema.Schema == nil {
				return nil, fmt.Errorf("%w: event type %q has no schema", ErrPermanent, schema.EventType)
			}

			if earlier, isSeen := seen[schema.EventType]; isSeen {
				same, err := isSameSchema(earlier, schema.Schema)
				if err != nil {
					return nil, fmt.Errorf("%w: comparing schemas for %q: %v", ErrPermanent, schema.EventType, err)
				}
				if !same {
					return nil, fmt.Errorf("%w: event type %q has two different schemas", ErrPermanent, schema.EventType)
				}
				continue
			}

			seen[schema.EventType] = schema.Schema
			collected = append(collected, schema)
		}
	}

	return collected, nil
}

// readRegisteredSchemas reads the schemas the database holds, by event type.
//
// It reads all event types to the end, and deliberately does not ask for a
// single one: after reading a single event type that further event types
// follow, EventSourcingDB 1.2.0 stops answering writes.
func (s *Store) readRegisteredSchemas() (map[string]map[string]any, error) {
	registered := map[string]map[string]any{}

	for eventType, err := range s.client.ReadEventTypes(context.Background()) {
		if err != nil {
			return nil, databaseFailure(err, "reading the registered schemas")
		}
		if eventType.Schema != nil {
			registered[eventType.EventType] = *eventType.Schema
		}
	}

	return registered, nil
}

// checkRegisteredSchema fails unless the registered schema of an event type is
// exactly the given one.
func checkRegisteredSchema(schema EventSchema, registered map[string]any) error {
	same, err := isSameSchema(schema.Schema, registered)
	if err != nil {
		return fmt.Errorf("%w: comparing schemas for %q: %v", ErrPermanent, schema.EventType, err)
	}
	if !same {
		return fmt.Errorf("%w: the schema of %q differs from the registered one, which cannot change; "+
			"introduce a new event type and an upcaster instead", ErrPermanent, schema.EventType)
	}

	return nil
}

// isSameSchema compares two schemas by their JSON form. A schema from the code
// may hold other Go types, such as []string, than the same schema decoded from
// the database, but both encode alike, since maps are encoded with sorted keys.
func isSameSchema(left, right map[string]any) (bool, error) {
	encodedLeft, err := json.Marshal(left)
	if err != nil {
		return false, err
	}
	encodedRight, err := json.Marshal(right)
	if err != nil {
		return false, err
	}

	return bytes.Equal(encodedLeft, encodedRight), nil
}

// Execute loads the state, lets the decider decide, and appends the resulting
// events. It does not retry: a conflict is reported, and the caller decides
// what to do about it.
//
// All preconditions come from the command, which has to declare at least one.
// OnStateRead is filled in with the last event Execute has read, so that the
// events are only written if the state they were decided on still holds.
func Execute[TCommand Command, TState any](
	ctx context.Context,
	store *Store,
	decider Decider[TCommand, TState],
	cmd TCommand,
) ([]eventsourcingdb.Event, error) {
	subject := cmd.Subject()

	declared, err := checkPreconditions(cmd)
	if err != nil {
		return nil, err
	}

	state, lastEventID, err := fold(ctx, store, subject, decider.State)
	if err != nil {
		return nil, err
	}

	events, err := decider.Decide(ctx, cmd, state)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, nil
	}

	return store.write(subject, events, resolvePreconditions(subject, declared, lastEventID))
}
