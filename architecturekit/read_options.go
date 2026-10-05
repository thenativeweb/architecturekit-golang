package architecturekit

import (
	"errors"
	"fmt"

	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// ErrEmptyRange means that the bounds of a read leave no room for an event,
// such as one before "0", one after "0" and before "1", or one after the
// largest revision, 2^63-1. No event can ever lie in such a range, so it is a
// mistake of whoever chose the bounds, which usually come from a request. A
// range that is only empty for now, such as the one after the last event
// written so far, is no such mistake, since events can still come there, so
// Read hands out no events for it, without an error.
var ErrEmptyRange = errors.New("empty range")

// ReadOption narrows down which events Read hands out, or turns around the
// order it hands them out in. Create one with FromEvent, AfterEvent,
// UpToEvent, BeforeEvent, NewestFirst, or FromLatestEvent.
//
// A read has at most one lower bound, one upper bound, and one order, and
// FromLatestEvent counts as a lower bound. Options that contradict each other,
// such as FromEvent together with AfterEvent, are a programming error, so Read
// panics for them, naming both.
type ReadOption func(*readSettings)

type readSettings struct {
	// lowerBound, upperBound and order hold the option that set them, as it is
	// written in the code, such as FromEvent("3"), so that a contradiction can
	// name both options. They stay empty while no option sets them.
	lowerBound string
	upperBound string
	order      string

	database eventsourcingdb.ReadEventsOptions
}

// FromEvent reads the events from the one with the given ID on, including it.
//
// The ID is a string, as everywhere else in the kit. One that is not the ID of
// an event, such as an empty one, makes Read fail with ErrNotARevision.
func FromEvent(id string) ReadOption {
	return lowerBound("FromEvent", id, eventsourcingdb.BoundTypeInclusive)
}

// AfterEvent reads the events after the one with the given ID, leaving it out.
// Hand over the ID of the last event of a page to read the next one.
//
// The ID is a string, as everywhere else in the kit. One that is not the ID of
// an event, such as an empty one, makes Read fail with ErrNotARevision.
func AfterEvent(id string) ReadOption {
	return lowerBound("AfterEvent", id, eventsourcingdb.BoundTypeExclusive)
}

// UpToEvent reads the events up to the one with the given ID, including it.
//
// The ID is a string, as everywhere else in the kit. One that is not the ID of
// an event, such as an empty one, makes Read fail with ErrNotARevision.
func UpToEvent(id string) ReadOption {
	return upperBound("UpToEvent", id, eventsourcingdb.BoundTypeInclusive)
}

// BeforeEvent reads the events before the one with the given ID, leaving it
// out.
//
// The ID is a string, as everywhere else in the kit. One that is not the ID of
// an event, such as an empty one, makes Read fail with ErrNotARevision.
func BeforeEvent(id string) ReadOption {
	return upperBound("BeforeEvent", id, eventsourcingdb.BoundTypeExclusive)
}

func lowerBound(name, id string, boundType eventsourcingdb.BoundType) ReadOption {
	option := fmt.Sprintf("%s(%q)", name, id)

	return func(settings *readSettings) {
		settings.set(&settings.lowerBound, "lower bound", option)
		settings.database.LowerBound = &eventsourcingdb.Bound{ID: id, Type: boundType}
	}
}

func upperBound(name, id string, boundType eventsourcingdb.BoundType) ReadOption {
	option := fmt.Sprintf("%s(%q)", name, id)

	return func(settings *readSettings) {
		settings.set(&settings.upperBound, "upper bound", option)
		settings.database.UpperBound = &eventsourcingdb.Bound{ID: id, Type: boundType}
	}
}

// NewestFirst hands out the newest event first, rather than the oldest one.
// The bounds stay what they are, so together with BeforeEvent, it reads the
// events before the given one, newest first, which pages backwards.
func NewestFirst() ReadOption {
	return func(settings *readSettings) {
		settings.set(&settings.order, "order", "NewestFirst()")
		settings.database.Order = eventsourcingdb.OrderAntichronological()
	}
}

// IfLatestEventIsMissing says what FromLatestEvent reads if the subject has no
// event of the type yet: ReadEverything or ReadNothing.
//
// The zero value says neither, which is a programming error, so
// FromLatestEvent panics for it.
type IfLatestEventIsMissing int

const (
	// ReadEverything reads the events as if FromLatestEvent were not given, so
	// from the first one on, or up to the upper bound, if there is one.
	ReadEverything IfLatestEventIsMissing = iota + 1

	// ReadNothing reads no event at all.
	ReadNothing
)

// FromLatestEvent starts reading at the latest event of the given type on the
// given subject, including it, as FromLatest does for a state. The database
// looks for it on that subject alone, not below it, and the subject does not
// have to be one of those that are read. If it has no event of the type,
// ifMissing says whether to read everything or nothing. If the latest event
// of the type comes after the upper bound, the database refuses the read, and
// Read fails with an error of the category ErrPermanent, since trying again
// never helps.
//
// It sets the lower bound, so it contradicts FromEvent and AfterEvent, and the
// database reads from the latest event of a type only oldest first, so it
// contradicts NewestFirst as well: Read panics for each of them. A subject
// that does not start with a slash, or an ifMissing other than ReadEverything
// and ReadNothing, is a programming error as well, so it panics.
func FromLatestEvent(subject, eventType string, ifMissing IfLatestEventIsMissing) ReadOption {
	requireLeadingSlash(subject)

	var missing eventsourcingdb.ReadIfEventIsMissing
	switch ifMissing {
	case ReadEverything:
		missing = eventsourcingdb.ReadIfEventIsMissingReadEverything
	case ReadNothing:
		missing = eventsourcingdb.ReadIfEventIsMissingReadNothing
	default:
		panic(fmt.Sprintf("architecturekit: FromLatestEvent needs ReadEverything or ReadNothing, "+
			"not IfLatestEventIsMissing(%d)", ifMissing))
	}

	option := fmt.Sprintf("FromLatestEvent(%q, %q)", subject, eventType)

	return func(settings *readSettings) {
		settings.set(&settings.lowerBound, "lower bound", option)
		settings.database.FromLatestEvent = &eventsourcingdb.ReadFromLatestEvent{
			Subject:          subject,
			Type:             eventType,
			IfEventIsMissing: missing,
		}
	}
}

// set records the option that sets one of the settings, and panics if another
// option has set it already.
func (s *readSettings) set(setting *string, what, option string) {
	if *setting != "" {
		panic(fmt.Sprintf("architecturekit: a read has one %s, but got %s and %s", what, *setting, option))
	}

	*setting = option
}

func readSettingsOf(subjects Subjects, options []ReadOption) readSettings {
	settings := readSettings{
		database: eventsourcingdb.ReadEventsOptions{Recursive: subjects.recursive},
	}

	for _, option := range options {
		option(&settings)
	}

	if settings.database.FromLatestEvent != nil && settings.order != "" {
		panic(fmt.Sprintf("architecturekit: the database reads from the latest event of a type only oldest first, "+
			"but got %s and %s", settings.lowerBound, settings.order))
	}

	return settings
}

// checkBounds returns the error of ParseRevision for the first bound whose ID
// is not the ID of an event, which, unlike a revision, an empty ID is not, the
// lower bound first. If both IDs are fine, it fails with ErrEmptyRange if the
// bounds leave no room for an event, which includes the range after the
// largest revision without an upper bound, and returns nil otherwise.
// FromLatestEvent has no ID, and leaves the lower bound of the database empty,
// so that the lower bound is not known, and only an upper bound that leaves no
// room for an event on its own, before "0", is refused then.
func (s readSettings) checkBounds() error {
	lower, upper := s.database.LowerBound, s.database.UpperBound

	// The range holds the IDs from first on, and up to, but not including,
	// end. FromEvent(n) starts it at n and AfterEvent(n) at n+1, and
	// UpToEvent(n) ends it after n and BeforeEvent(n) at n. Counting the end
	// that way needs no number below 0 for BeforeEvent("0"), and ParseRevision
	// ends at 2^63-1, so n+1 always fits. Without an upper bound, the range
	// ends after the largest revision, so that no event can lie after it.
	var first uint64
	end := uint64(1) << 63

	if lower != nil {
		id, err := ParseRevision(lower.ID)
		if err != nil {
			return err
		}

		first = id
		if lower.Type == eventsourcingdb.BoundTypeExclusive {
			first++
		}
	}

	if upper != nil {
		id, err := ParseRevision(upper.ID)
		if err != nil {
			return err
		}

		end = id
		if upper.Type == eventsourcingdb.BoundTypeInclusive {
			end++
		}
	}

	if first >= end {
		return fmt.Errorf("%w: no event can lie %s", ErrEmptyRange, describeRange(lower, upper))
	}

	return nil
}

// describeRange names the bounds of a range by their values only, since they
// usually come from a request, whose caller knows neither the options of Read
// nor the subjects: from "2" up to "1", from "1" and before "1", or after "0"
// and before "1", before "0" without a lower bound, and after the largest
// revision without an upper bound, the only range that is empty then.
func describeRange(lower, upper *eventsourcingdb.Bound) string {
	if upper == nil {
		return fmt.Sprintf("after %q", lower.ID)
	}

	ending := fmt.Sprintf("before %q", upper.ID)
	if upper.Type == eventsourcingdb.BoundTypeInclusive {
		ending = fmt.Sprintf("up to %q", upper.ID)
	}

	switch {
	case lower == nil:
		return ending
	case lower.Type == eventsourcingdb.BoundTypeExclusive:
		return fmt.Sprintf("after %q and %s", lower.ID, ending)
	case upper.Type == eventsourcingdb.BoundTypeInclusive:
		// From one up to another reads naturally without "and".
		return fmt.Sprintf("from %q %s", lower.ID, ending)
	default:
		return fmt.Sprintf("from %q and %s", lower.ID, ending)
	}
}
