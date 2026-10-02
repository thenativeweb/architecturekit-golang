package architecturekit

import (
	"fmt"
	"strings"
	"time"
)

// Subjects names what a projection reads: a subject, and whether the subjects
// below it belong to it. Create it with SubjectTree or ExactSubject, so that
// every projection says which of the two it means, rather than relying on a
// default that differs from the one of the client SDK.
type Subjects struct {
	subject   string
	recursive bool
}

// SubjectTree is the given subject together with every subject below it, which
// is what a projection usually reads: every book below /books, or everything
// below /. It is what the database calls reading recursively.
//
// A subject that does not start with a slash is a programming error, so it
// panics.
func SubjectTree(subject string) Subjects {
	return subjectsOf(subject, true)
}

// ExactSubject is the given subject alone, without the subjects below it.
//
// A subject that does not start with a slash is a programming error, so it
// panics.
func ExactSubject(subject string) Subjects {
	return subjectsOf(subject, false)
}

func subjectsOf(subject string, recursive bool) Subjects {
	if !strings.HasPrefix(subject, "/") {
		panic(fmt.Sprintf("architecturekit: a subject starts with a slash, which %q does not", subject))
	}

	return Subjects{subject: subject, recursive: recursive}
}

// requireSubjects panics for subjects that were not created with SubjectTree
// or ExactSubject, which the zero value is not.
func requireSubjects(subjects Subjects) {
	if subjects.subject == "" {
		panic("architecturekit: a projection reads the subjects of SubjectTree or ExactSubject, not none")
	}
}

// ProjectionOption configures a projection that CatchUpProjection,
// StartProjection or their transactional counterparts drive.
type ProjectionOption func(*projectionSettings)

type projectionSettings struct {
	name string
}

// Named gives a projection a name, by which the observer of reconnects reports
// it (see WithReconnectObserver), and by which the health handlers of httpapi
// list it. The run returns it from Name.
//
// An empty name, or giving Named twice, is a programming error, so it panics.
func Named(name string) ProjectionOption {
	if name == "" {
		panic("architecturekit: Named needs a name, not an empty one")
	}

	return func(settings *projectionSettings) {
		if settings.name != "" {
			panic(fmt.Sprintf("architecturekit: the projection is named twice, as %q and as %q", settings.name, name))
		}

		settings.name = name
	}
}

func projectionSettingsOf(options []ProjectionOption) projectionSettings {
	var settings projectionSettings
	for _, option := range options {
		option(&settings)
	}

	return settings
}

// Reconnect describes an attempt of a projection to read again, after reading
// failed or the database ended the stream. The observer set with
// WithReconnectObserver receives one for every attempt.
type Reconnect struct {
	// Projection is the name the projection was given with Named, or empty if
	// it has none. Subject tells it apart then.
	Projection string

	// Subject is the subject the projection reads from.
	Subject string

	// Err is why the projection reads again. It is nil if the database ended
	// the stream.
	Err error

	// Delay is how long the projection waits before it reads again.
	Delay time.Duration

	// Attempt counts the attempts in a row in which the projection has not
	// got any further, starting at one. It grows together with Delay, and
	// starts over with it once the projection has applied an event.
	//
	// ProjectionStatus counts differently: its Attempts start over whenever the
	// projection has caught up, even if the stream ends again right after.
	Attempt int
}
