package architecturekit

import (
	"fmt"

	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

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

// invalidBound returns the option of a bound whose ID is not the ID of an
// event, or an empty string if both bounds are fine. FromLatestEvent has no ID,
// and leaves the lower bound of the database empty.
func (s readSettings) invalidBound() string {
	switch {
	case s.database.LowerBound != nil && !isEventID(s.database.LowerBound.ID):
		return s.lowerBound
	case s.database.UpperBound != nil && !isEventID(s.database.UpperBound.ID):
		return s.upperBound
	default:
		return ""
	}
}

// isEventID tells whether the ID can be the one of an event, which, unlike a
// revision, an empty ID can not.
func isEventID(id string) bool {
	_, err := ParseRevision(id)
	return err == nil
}
