package architecturekit

import (
	"fmt"
	"strings"
	"time"
)

// Subjects names what a projection or Read reads: a subject, and whether the
// subjects below it belong to it. Create it with SubjectTree or ExactSubject,
// so that every projection and every read says which of the two it means,
// rather than relying on a default that differs from the one of the client
// SDK.
//
// The zero value names no subject, which is a programming error, so
// StartProjection, the other functions that run a projection, and Read panic
// for it.
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
	requireLeadingSlash(subject)

	return Subjects{subject: subject, recursive: recursive}
}

// requireLeadingSlash panics for a subject that does not start with a slash.
func requireLeadingSlash(subject string) {
	if !strings.HasPrefix(subject, "/") {
		panic(fmt.Sprintf("architecturekit: a subject starts with a slash, which %q does not", subject))
	}
}

// requireSubjects panics for subjects that were not created with SubjectTree
// or ExactSubject, which the zero value is not.
func requireSubjects(subjects Subjects) {
	if subjects.subject == "" {
		panic("architecturekit: reading needs the subjects of SubjectTree or ExactSubject, not none")
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

// projectionSettingsOf applies the options of the function of the kit with
// the given name, and panics, naming the function, for a nil option.
func projectionSettingsOf(function string, options []ProjectionOption) projectionSettings {
	var settings projectionSettings
	applyOptions(function, &settings, options)

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

	// Attempt counts the attempts in a row, starting at one. It grows together
	// with Delay, and starts over with it once the projection has applied an
	// event, or has followed the stream for longer than the initial delay,
	// even if no event arrived. So it stays at one while every stream ends
	// only after that, as when a load balancer ends long-lived connections
	// regularly.
	//
	// ProjectionStatus counts differently: its Attempts start over whenever the
	// projection has caught up, even if the stream ends again within the
	// initial delay.
	Attempt int
}
