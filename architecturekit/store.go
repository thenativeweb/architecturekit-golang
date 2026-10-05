package architecturekit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"reflect"
	"slices"
	"time"

	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// Store reads from and writes to the EventSourcingDB.
type Store struct {
	client *eventsourcingdb.Client
	source string

	// settings are what the options of NewStore set. They never change once
	// the store exists.
	settings storeSettings

	// states is nil unless the store was created with WithStateCache.
	states *stateCache
}

// The default delays of a projection run before it observes again, for a store
// without WithReconnectDelays.
const (
	defaultReconnectInitialDelay = 1 * time.Second
	defaultReconnectMaxDelay     = 1 * time.Minute
)

// StoreOption configures a store that NewStore creates. It sets the settings
// of the new store, before the store exists, so it can not change a store that
// is in use.
type StoreOption func(*storeSettings)

type storeSettings struct {
	// maxCachedSubjects is how many subjects the state cache holds, set by
	// WithStateCache. Without it, it is 0, and the store has no cache.
	maxCachedSubjects int

	// The reconnect settings apply to the projections started with
	// StartProjection and StartTransactionalProjection.
	reconnectInitialDelay time.Duration
	reconnectMaxDelay     time.Duration
	reconnectObserver     func(Reconnect)

	// skipsHashes is set by WithoutHashVerification, and verificationKey by
	// WithSignatureVerification. Checking a signature includes the hash.
	skipsHashes     bool
	verificationKey ed25519.PublicKey

	// conflictRetries is how often Execute decides again on a conflict, set
	// by WithConflictRetries.
	conflictRetries int
}

// WithStateCache keeps the states of the most recently used subjects in
// memory, up to the given number, so that the next command on one of them
// reads only the events written since. A number of 0 keeps none, which is the
// same as leaving out the option, so that a configuration can turn the cache
// off.
//
// Only states that consist of values, or that have a Clone function, are
// cached (see State.Clone). The cache is correct with several processes
// writing to the same subjects, because every command still reads all events
// after the ones it has cached.
//
// The cache tells states apart by their type, so a state that is built anew
// for every command is cached as well. Two different states that read the
// same subject need two different types. Of two states of the same type, the
// cache compares what it can see of how they are built: their initial values,
// the event types they have Evolve rules for, ignore, or have upcasters for,
// and FromLatest. If any of these differ on the same subject, reading fails
// with ErrPermanent. The cache can not compare functions, though, such as the
// Evolve functions themselves, so two states of the same type that are built
// alike, but compute something else, are not told apart: one of them gets the
// cached state of the other, without an error.
//
// A negative number of subjects is a programming error, so it panics.
func WithStateCache(maxSubjects int) StoreOption {
	if maxSubjects < 0 {
		panic(fmt.Sprintf("architecturekit: WithStateCache needs a number of subjects that is not negative, not %d", maxSubjects))
	}

	return func(settings *storeSettings) {
		settings.maxCachedSubjects = maxSubjects
	}
}

// WithConflictRetries has Execute decide again, up to the given number of
// times, if a command could not be written because something else was written
// to its subject after Execute had read the state. Execute then reads the
// state anew and the decider decides on it, as if the command had arrived a
// moment later. Without this option, Execute reports the conflict right away.
//
// This applies only to commands whose preconditions include OnStateRead, since
// only then does the conflict come from the state Execute read. A command that
// checks only a revision the caller hands over is never decided again: the
// caller has to learn about the conflict, and deciding again would fail the
// same way. A command that checks both is decided again, since the database
// does not say which precondition did not hold, so if the revision of the
// caller is outdated, every attempt fails the same way until the retries are
// used up. On the same subject, one of the two is enough: if the revision of
// the caller holds, so does OnStateRead.
//
// Once the retries are used up, Execute reports the conflict, an error of the
// category ErrConflict. Write never decides again, since it decides nothing.
//
// A negative number of retries is a programming error, so it panics.
func WithConflictRetries(retries int) StoreOption {
	if retries < 0 {
		panic(fmt.Sprintf("architecturekit: WithConflictRetries needs a number of retries that is not negative, not %d", retries))
	}

	return func(settings *storeSettings) {
		settings.conflictRetries = retries
	}
}

// WithReconnectDelays sets how long the projections started with
// StartProjection or StartTransactionalProjection wait before they read again
// after reading failed or the stream ended. The delay starts at initialDelay,
// doubles with every attempt in a row, and never exceeds maxDelay. It starts
// over once a projection has made progress, or has followed the stream for
// longer than initialDelay, even if no event arrived, as when a load balancer
// ends long-lived connections regularly. A database that fails before a
// projection has caught up, or within initialDelay after, is given ever more
// time. One that ends every stream later than that is tried again after
// initialDelay each time, since catching up succeeded. Without this option,
// the delays are 1 second and 1 minute.
//
// An initialDelay of zero or less is a programming error, since the
// projections would then read again without any pause, and so is a maxDelay
// below initialDelay. Both panic.
func WithReconnectDelays(initialDelay, maxDelay time.Duration) StoreOption {
	if initialDelay <= 0 {
		panic(fmt.Sprintf("architecturekit: WithReconnectDelays needs an initial delay that is positive, not %v", initialDelay))
	}
	if maxDelay < initialDelay {
		panic(fmt.Sprintf("architecturekit: WithReconnectDelays needs a maximum delay that is not below the initial delay of %v, not %v",
			initialDelay, maxDelay))
	}

	return func(settings *storeSettings) {
		settings.reconnectInitialDelay = initialDelay
		settings.reconnectMaxDelay = maxDelay
	}
}

// WithReconnectObserver calls observe every time a projection started with
// StartProjection or StartTransactionalProjection is about to wait before
// reading again. It learns which projection, why, how long it waits, and the
// how-manyth attempt in a row this is (see Reconnect). Use it to log, since the
// kit itself does not; give the projections names with Named, so that the log
// tells them apart. A panic in observe ends the run, as a panic in the
// projection does (see StartProjection).
func WithReconnectObserver(observe func(Reconnect)) StoreOption {
	return func(settings *storeSettings) {
		settings.reconnectObserver = observe
	}
}

// WithoutHashVerification turns off checking the hash of every event the store
// reads, which a store does unless told otherwise. Checking a hash takes about
// a microsecond per event, so there is rarely a reason to, and an event that
// was changed after it had been written goes unnoticed then.
//
// It contradicts WithSignatureVerification, since checking a signature
// includes checking the hash, so a store with both panics.
func WithoutHashVerification() StoreOption {
	return func(settings *storeSettings) {
		settings.skipsHashes = true
	}
}

// WithSignatureVerification checks the signature of every event the store
// reads, besides its hash, against the verification key of the database. An
// event without a signature, or with one that does not match the key, makes
// reading fail with ErrUnverified.
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

	return func(settings *storeSettings) {
		settings.verificationKey = verificationKey
	}
}

// NewStore creates a store that writes events with the given source.
//
// The store checks the hash of every event it reads, before any upcaster,
// Evolve rule, or projection sees it, and an event whose hash does not match
// its content makes reading fail with ErrUnverified. This applies to Execute,
// Load, Read, and every kind of projection. The events that Execute has just
// written are not checked, since they are not read. To turn this off, hand
// over WithoutHashVerification.
//
// A nil client is a programming error, so NewStore panics, rather than the
// store failing once it first reads or writes.
func NewStore(client *eventsourcingdb.Client, source string, options ...StoreOption) *Store {
	if client == nil {
		panic("architecturekit: NewStore needs a client, not nil")
	}

	settings := storeSettings{
		reconnectInitialDelay: defaultReconnectInitialDelay,
		reconnectMaxDelay:     defaultReconnectMaxDelay,
	}
	for _, option := range options {
		option(&settings)
	}

	if settings.skipsHashes && settings.verificationKey != nil {
		panic("architecturekit: WithoutHashVerification contradicts WithSignatureVerification, which checks the hash as well")
	}

	store := &Store{client: client, source: source, settings: settings}
	if settings.maxCachedSubjects > 0 {
		store.states = newStateCache(settings.maxCachedSubjects)
	}

	return store
}

// Load reads the state of a subject exactly the way Execute does before it
// decides, including the state cache of the store. Use it for a query that
// needs the state of a single subject, rather than a view across many.
//
// If the context ends before the state is read completely, Load fails with
// the context's error rather than handing out part of the state.
func Load[TState any](
	ctx context.Context,
	store *Store,
	state *State[TState],
	subject string,
) (TState, error) {
	current, _, err := fold(ctx, store, subject, state)
	return current, err
}

// Read hands out the events of the given subjects as they are stored, for a
// read that neither Load nor a projection fits: one page of a long stream, the
// events up to a certain one, or a single event. Every event is verified like
// everything else the store reads, before the caller sees it, and a failure is
// sorted into a category like with Load. Neither upcasters nor rules apply,
// since there is no state; to get at the data of an event, use Decode.
//
// The subjects come from SubjectTree or ExactSubject, as for a projection, and
// their zero value panics. Without options, Read hands out every event of the
// subjects, oldest first. FromEvent, AfterEvent, UpToEvent, and BeforeEvent
// bound the IDs, which the database hands out as one ascending sequence
// across all subjects, NewestFirst turns the order around, and FromLatestEvent
// starts from the latest event of a type. Options that contradict each other
// panic (see ReadOption). For anything else, use ReadEvents of the client SDK.
//
// A bound whose ID is not the ID of an event, such as an empty one, ends the
// iteration with an error that wraps ErrNotARevision, before the database is
// asked. Otherwise, the iteration ends with the first error, and stops reading
// as soon as the caller stops iterating. If the context ends first, it ends
// with the context's error, so that a read that was cut short never looks
// complete.
func Read(
	ctx context.Context,
	store *Store,
	subjects Subjects,
	options ...ReadOption,
) iter.Seq2[eventsourcingdb.Event, error] {
	requireSubjects(subjects)
	settings := readSettingsOf(subjects, options)

	return func(yield func(eventsourcingdb.Event, error) bool) {
		doing := fmt.Sprintf("reading %q", subjects.subject)

		if option := settings.invalidBound(); option != "" {
			yield(eventsourcingdb.Event{}, fmt.Errorf("%w: %s: %s needs the ID of an event",
				ErrNotARevision, doing, option))
			return
		}

		for event, err := range store.client.ReadEvents(ctx, subjects.subject, settings.database) {
			if err != nil {
				yield(eventsourcingdb.Event{}, readFailure(ctx, err, doing))
				return
			}

			if err := store.verify(event); err != nil {
				yield(eventsourcingdb.Event{}, err)
				return
			}

			if !yield(event, nil) {
				return
			}
		}
	}
}

// fold reads the stream and folds it into a state as it goes, starting from a
// copy of the initial value. Events are not collected, so even long streams
// need constant memory only.
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
	current, err := state.copyOfInitial()
	if err != nil {
		return current, "", err
	}
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
			IfEventIsMissing: eventsourcingdb.ReadIfEventIsMissingReadEverything,
		}
	}

	for event, err := range store.client.ReadEvents(ctx, subject, options) {
		if err != nil {
			return current, "", readFailure(ctx, err, fmt.Sprintf("reading %q", subject))
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

// verify checks an event the store has read: its hash, unless the store was
// created with WithoutHashVerification, and its signature as well, if it was
// created with WithSignatureVerification. It has to run on the event as stored,
// before any upcaster changes it.
func (s *Store) verify(event eventsourcingdb.Event) error {
	var err error

	switch {
	case s.settings.verificationKey != nil:
		err = event.VerifySignature(s.settings.verificationKey)
	case !s.settings.skipsHashes:
		err = event.VerifyHash()
	}

	if err != nil {
		return fmt.Errorf("%w: event %s on %q: %v", ErrUnverified, event.ID, event.Subject, err)
	}

	return nil
}

// candidateFor turns an event into what the client writes, with the source of
// the store and with its data encoded already. Data that can not be encoded,
// such as a float NaN, fails permanently, naming the event type, the subject
// and the reason.
//
// The client would encode the data itself, but it reports data it can not
// encode like a database it can not reach, which counts as transient, although
// trying again never helps. So the data is encoded here, with encoding/json, as
// the client does, and handed over as a json.RawMessage, which the client
// writes as it is. The database receives exactly the JSON the client would
// have written itself.
//
// The test fixture of architecturekittest refuses such data with the same
// error, which it words itself, so a test compares the two.
func (s *Store) candidateFor(subject string, event Event) (eventsourcingdb.EventCandidate, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return eventsourcingdb.EventCandidate{}, fmt.Errorf("%w: refusing to write an event of type %q to %q, "+
			"since its data can not be encoded as JSON: %v", ErrPermanent, event.EventType(), subject, err)
	}

	return eventsourcingdb.EventCandidate{
		Source:  s.source,
		Subject: subject,
		Type:    event.EventType(),
		Data:    json.RawMessage(data),
	}, nil
}

// write appends the candidates to a subject under the given preconditions and
// returns them as the database recorded them, including their IDs.
func (s *Store) write(
	subject string,
	candidates []eventsourcingdb.EventCandidate,
	preconditions []eventsourcingdb.Precondition,
) ([]eventsourcingdb.Event, error) {
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
//
// If the context ends first, RegisterSchemas fails with the context's error.
// The client registers a schema without a context, so a registration that has
// begun is finished, but none begins once the context has ended.
func RegisterSchemas(ctx context.Context, store *Store, schemas ...[]EventSchema) error {
	given, err := collectSchemas(schemas)
	if err != nil {
		return err
	}

	registered, err := store.readRegisteredSchemas(ctx)
	if err != nil {
		return err
	}

	for _, schema := range given {
		current, isRegistered := registered[schema.EventType]

		if !isRegistered {
			if ctx.Err() != nil {
				return contextEnded(ctx, fmt.Sprintf("registering schema for %q", schema.EventType))
			}

			refusal := store.client.RegisterEventSchema(schema.EventType, schema.Schema)
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
			registered, err = store.readRegisteredSchemas(ctx)
			if err != nil {
				return err
			}

			current, isRegistered = registered[schema.EventType]
			if !isRegistered {
				return schemaRefusal(schema.EventType, refusal)
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
func (s *Store) readRegisteredSchemas(ctx context.Context) (map[string]map[string]any, error) {
	registered := map[string]map[string]any{}

	for eventType, err := range s.client.ReadEventTypes(ctx) {
		if err != nil {
			return nil, readFailure(ctx, err, "reading the registered schemas")
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
// events. A conflict is reported, and the caller decides what to do about it,
// unless the store was created with WithConflictRetries and the command
// decides on the state read, in which case Execute decides again first.
//
// All preconditions come from the command, which has to declare at least one.
// OnStateRead is filled in with the last event Execute has read, so that the
// events are only written if the state they were decided on still holds.
//
// If one of the events the decider returns is nil, Execute writes none of
// them and fails with an error of the category ErrPermanent that names the
// index of the event, since an event that is nil has neither a type nor data.
// A nil pointer of a concrete type counts as nil.
//
// Every event the decider returns needs a rule on the state of the decider,
// an Evolve rule or Ignore, since Execute writes it to the subject that the
// same state reads for the next command. If one of them has none, Execute
// writes none of them and fails with an error of the category ErrPermanent
// that names the event type, rather than leave a subject the state can not
// read any more.
//
// The same goes for an event whose data can not be encoded as JSON, for
// example because it holds a float NaN: Execute writes none of the events and
// fails with an error of the category ErrPermanent that names the event type
// and the reason, since trying again would fail the same way.
//
// Execute checks all events for nil first, then all of them for a rule, and
// encodes them last.
//
// If the context ends before the events are written, Execute writes nothing
// and fails with the context's error, also if it ends while the decider
// decides. Once the write has begun, it is finished, since the client writes
// without a context.
//
// The zero Decider, one that was not made with NewDecider, is a programming
// error, so Execute panics, before it looks at the command, and names the
// mistake rather than a nil pointer.
func Execute[TCommand Command, TState any](
	ctx context.Context,
	store *Store,
	decider Decider[TCommand, TState],
	cmd TCommand,
) ([]eventsourcingdb.Event, error) {
	if decider.State() == nil {
		panic("architecturekit: Execute needs a decider made with NewDecider, not the zero Decider")
	}

	declared, err := checkPreconditions(cmd)
	if err != nil {
		return nil, err
	}

	retries := 0
	if slices.ContainsFunc(declared, Precondition.IsOnStateRead) {
		retries = store.settings.conflictRetries
	}

	for attempt := 0; ; attempt++ {
		written, err := executeOnce(ctx, store, decider, cmd, declared)
		if !errors.Is(err, ErrConflict) || attempt == retries || ctx.Err() != nil {
			return written, err
		}
	}
}

// executeOnce reads the state, decides, and writes, once.
func executeOnce[TCommand Command, TState any](
	ctx context.Context,
	store *Store,
	decider Decider[TCommand, TState],
	cmd TCommand,
	declared []Precondition,
) ([]eventsourcingdb.Event, error) {
	subject := cmd.Subject()

	state, lastEventID, err := fold(ctx, store, subject, decider.State())
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

	// An event that is nil can neither be checked nor written, so then none of
	// the events is written, and nothing else about them is checked.
	if err := checkNotNil(subject, events); err != nil {
		return nil, err
	}

	// An event the state has no rule for would leave a subject the state can
	// not read any more, so then none of the events is written.
	if err := decider.State().checkRules(subject, events); err != nil {
		return nil, err
	}

	// Nor is any of them written if the data of one of them can not be
	// encoded.
	candidates := make([]eventsourcingdb.EventCandidate, len(events))
	for i, event := range events {
		if candidates[i], err = store.candidateFor(subject, event); err != nil {
			return nil, err
		}
	}

	// The client writes without a context, so a context that has ended by now,
	// for example while deciding, must not lead to a write anyway.
	if ctx.Err() != nil {
		return nil, contextEnded(ctx, fmt.Sprintf("writing to %q", subject))
	}

	return store.write(subject, candidates, resolvePreconditions(subject, declared, lastEventID))
}

// checkNotNil fails permanently if one of the events a decider returned for a
// subject is nil, also a nil pointer of a concrete type (see isNil), naming
// its index, since such an event has neither a type nor data.
//
// The test fixture of architecturekittest refuses such an event with the same
// error, which it words itself, so a test compares the two.
func checkNotNil(subject string, events []Event) error {
	for i, event := range events {
		if isNil(event) {
			return fmt.Errorf("%w: refusing to write to %q, since event %d that the decider returned is nil",
				ErrPermanent, subject, i)
		}
	}

	return nil
}
