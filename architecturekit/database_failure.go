package architecturekit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/thenativeweb/eventsourcingdb-client-golang/v2/eventsourcingdb"
)

// answerOf returns the answer of the database that the client reports as a
// DBAPIError, with its status, its code, and the description the database
// gave, or nil if the failure came without an answer, e.g. because the
// database is unreachable or the connection broke.
func answerOf(err error) *eventsourcingdb.DBAPIError {
	var answer *eventsourcingdb.DBAPIError
	if !errors.As(err, &answer) {
		return nil
	}

	return answer
}

// statusOf returns the status the database answered with, or 0 if the failure
// came without an answer.
func statusOf(err error) int {
	answer := answerOf(err)
	if answer == nil {
		return 0
	}

	return answer.Status
}

// codeOf returns the code of the error the database reported, or an empty
// code if the failure came without an answer, or with an answer without a
// code, as from a database that only offers the v1 API for an error the
// client can not match to a code (see databaseFailure).
func codeOf(err error) string {
	answer := answerOf(err)
	if answer == nil {
		return ""
	}

	return answer.Code
}

// contextEnded reports that reading stopped, or that writing did not begin,
// because the context ended, and keeps the context's error, so that a caller
// can tell it from a failure of the database with errors.Is. It belongs to no
// category: nothing went wrong that trying again could fix, and nothing the
// request could have avoided: the caller stopped waiting, or ran out of time.
//
// Whatever the client reports once the context has ended is a consequence of
// that, so it is the context's error that counts, not the client's.
func contextEnded(ctx context.Context, doing string) error {
	return fmt.Errorf("architecturekit: %s: %w", doing, ctx.Err())
}

// readFailure is what a read that failed reports: the end of the context if
// that is what stopped it, and the failure of the database otherwise. Every
// read of the kit goes through it.
//
// A read that the context cut short has seen only some of the events, and the
// client reports that as a failure, so that it never looks complete: a state
// built from part of the history would let a command decide on it.
//
// Reading has no preconditions, so a 409 without a code, which only a
// database that offers just the v1 API sends, is a refusal of the request
// like any other, which is permanent, whatever description the database
// gives. Every other answer is sorted as databaseFailure sorts it, the one a
// read with FromLatestEvent beyond its upper bound gets included, which
// carries the code range-unsatisfiable from either API. Either way, the
// failure of the client is wrapped after the category (see causeOf).
func readFailure(ctx context.Context, err error, doing string) error {
	if ctx.Err() != nil {
		return contextEnded(ctx, doing)
	}

	if codeOf(err) == "" && statusOf(err) == http.StatusConflict {
		return fmt.Errorf("%w: %s: %w", ErrPermanent, doing, causeOf(err))
	}

	return databaseFailure(err, doing)
}

// writeFailure is what a write that failed reports. If the client reports
// that the write may have been stored nonetheless, it is reported as
// ErrOutcomeUnknown, which wraps the failure of the client after it, as a
// category does (see causeOf). The client decides that, by the same rule in
// every SDK of EventSourcingDB: a write was certainly not stored only if the
// client could not connect to the database at all, or if the database refused
// it. Everything else leaves its outcome open: the connection broke after it
// was established, the answer was cut off, the database stalled, it failed
// unexpectedly, or the answer does not come from an EventSourcingDB, such as a
// 502 or 504 of a proxy in front of it, which may answer so after the
// database has stored the events.
//
// A write that was certainly not stored keeps the category that
// databaseFailure sorts it into, so that trying it again unchanged can not
// store the events twice.
func writeFailure(err error, doing string) error {
	if !errors.Is(err, eventsourcingdb.ErrOutcomeUnknown) {
		return databaseFailure(err, doing)
	}

	if errors.Is(err, eventsourcingdb.ErrInvalidServerHeader) {
		return fmt.Errorf("%w: %s: the answer does not come from an EventSourcingDB: %w",
			ErrOutcomeUnknown, doing, causeOf(err))
	}

	return fmt.Errorf("%w: %s: %w", ErrOutcomeUnknown, doing, causeOf(err))
}

// transientCodes are the codes with which the database refuses a request for
// now: it is shutting down, its store refuses to be written for the time
// being, or it waits for somebody to do something, such as to free space on
// the disk, to restart it after its store failed, or to give it a license
// that covers more. Trying the same request again later may succeed, so they
// are transient. A store that has reached the capacity of its license
// belongs with them: trying again after reading the state anew, as for a
// conflict, would never help.
var transientCodes = []string{
	eventsourcingdb.CodeServerShuttingDown,
	eventsourcingdb.CodeStoreReadOnly,
	eventsourcingdb.CodeIndexesRebuilding,
	eventsourcingdb.CodeRestoreInProgress,
	eventsourcingdb.CodeDiskFull,
	eventsourcingdb.CodeStoreFailed,
	eventsourcingdb.CodeLicenseInvalid,
	eventsourcingdb.CodeCapacityExceeded,
}

// databaseFailure sorts a failure the database reported into a category, by
// what its answer means. The rules apply to writing, and to reading as well,
// apart from a 409 without a code, which readFailure sorts before it gets
// here. A write gets here only if it certainly stored nothing (see
// writeFailure).
//
// An error the database reports with a code is sorted by its code alone,
// since a database that offers the v2 API reports the errors of a request
// with the status 200, and the client gives the errors of the v1 API the code
// of the v2 API wherever it clearly matches one:
//
//   - precondition-violated means that a precondition did not hold, which is a
//     conflict.
//   - The codes in transientCodes are transient.
//   - Every other code means that the request itself is wrong, which is
//     permanent, such as a subject, an event, or a bound that the database
//     does not accept, or an event that does not match its schema. So is a
//     code the kit does not know, from a database newer than the client:
//     the database reports an error with a code only when it refused the
//     request, so it is not the outcome of a write that is unknown, and the
//     kit can not tell that the same request may succeed later.
//
// A failure without a code is sorted by its status:
//
//   - Without an answer, the database is unreachable or the connection broke,
//     which is transient. The client reports data it can not encode without an
//     answer as well, which is why candidateFor encodes the data of every event
//     before it is written.
//   - An answer from a server that is not an EventSourcingDB is transient as
//     well. It may come from a wrong address, but also from a proxy in front of
//     the database, which answers on its own while the database restarts, and
//     the client checks who answered before it checks the status. A wrong
//     address fails on start anyway, so it is named rather than made
//     permanent.
//   - An error within a stream, which a database that offers just the v1 API
//     reports with the status 200 and without a code, is transient. It sends
//     one only when reading is disturbed, e.g. because it is shutting down,
//     rebuilds its indexes, or restores a backup.
//   - 408 means that the request did not arrive in time, 429 asks to slow
//     down, and 5xx means that the database is unable to answer for now, e.g.
//     because it is shutting down. All of them are transient.
//   - 409, which only a database that offers just the v1 API sends without a
//     code, means, when writing, that a precondition did not hold, which is a
//     conflict, or that an event does not match its schema, which is
//     permanent. Only the description the database gives tells them apart.
//     When reading, which has no preconditions, a 409 is permanent (see
//     readFailure).
//   - Every other status, such as 400, 401 or 413, means that the request
//     itself is wrong, which is permanent. A rejected API token is named, as it
//     is the one to expect in production, e.g. after the token was rotated.
//
// Every category wraps the failure of the client after it (see causeOf).
func databaseFailure(err error, doing string) error {
	if code := codeOf(err); code != "" {
		switch {
		case code == eventsourcingdb.CodePreconditionViolated:
			return fmt.Errorf("%w: %s: %w", ErrConflict, doing, causeOf(err))
		case slices.Contains(transientCodes, code):
			return fmt.Errorf("%w: %s: %w", ErrTransient, doing, causeOf(err))
		default:
			return fmt.Errorf("%w: %s: %w", ErrPermanent, doing, causeOf(err))
		}
	}

	status := statusOf(err)

	switch {
	case status == 0:
		if errors.Is(err, eventsourcingdb.ErrInvalidServerHeader) {
			return fmt.Errorf("%w: %s: the answer does not come from an EventSourcingDB: %w",
				ErrTransient, doing, causeOf(err))
		}
		return fmt.Errorf("%w: %s: %w", ErrTransient, doing, causeOf(err))

	case status == http.StatusOK,
		status == http.StatusRequestTimeout,
		status == http.StatusTooManyRequests,
		status >= http.StatusInternalServerError:
		return fmt.Errorf("%w: %s: %w", ErrTransient, doing, causeOf(err))

	case status == http.StatusConflict:
		if strings.HasPrefix(answerOf(err).Description, "schema conflict") {
			return fmt.Errorf("%w: %s: %w", ErrPermanent, doing, causeOf(err))
		}
		return fmt.Errorf("%w: %s: %w", ErrConflict, doing, causeOf(err))

	case status == http.StatusUnauthorized:
		return fmt.Errorf("%w: %s: the database rejected the API token: %w", ErrPermanent, doing, causeOf(err))

	default:
		return fmt.Errorf("%w: %s: %w", ErrPermanent, doing, causeOf(err))
	}
}

// isSchemaConflict reports whether the database refused to register a schema
// because the event type has one already, or because stored events of the
// type do not match it, which only the schemas registered afterwards tell
// apart (see RegisterSchemas). A database that offers just the v1 API answers
// both with 409, which the client gives the matching code, and any other 409
// of it is taken for one of them, too.
func isSchemaConflict(refusal error) bool {
	switch codeOf(refusal) {
	case eventsourcingdb.CodeEventSchemaAlreadyRegistered, eventsourcingdb.CodeEventSchemaViolated:
		return true
	case "":
		return statusOf(refusal) == http.StatusConflict
	default:
		return false
	}
}

// schemaRefusal reports that the database refused the schema of an event
// type, e.g. because stored events of the type do not match it, which is
// permanent, whatever the reason. Like every other failure of the database,
// it wraps the refusal of the client after the category (see causeOf).
func schemaRefusal(eventType string, refusal error) error {
	return fmt.Errorf("%w: the database refused the schema of %q: %w", ErrPermanent, eventType, causeOf(refusal))
}

// causeOf returns what a failure of the database wraps after its category:
// the failure the client reported, so that errors.As reaches an
// *eventsourcingdb.DBAPIError with the status, the code, and the description
// the database gave, and errors.Is reaches eventsourcingdb.ErrOutcomeUnknown,
// eventsourcingdb.ErrInvalidServerHeader, and
// eventsourcingdb.ErrConnectionStalled. The category comes first, and it is the
// category that decides what the failure means.
//
// A failure of the client that errors.Is takes for context.Canceled or
// context.DeadlineExceeded keeps its message, and errors.Is and errors.As still
// reach whatever it holds, apart from those two (see withoutContextEnd). Both
// mean that the context ended (see contextEnded), which a read checks before
// it gets here, and which neither a write nor the registration of a schema
// can run into, since the client gets their context without its end (see
// Store.write). So such a failure comes from the client or the network: a
// connection that stalled, which the client reports as
// context.DeadlineExceeded as well, or connecting to the database that timed
// out, which the standard library reports so. Reaching them, it would make a
// failure of the database look like the end of the context, and
// httpapi.StatusFor, which asks for the end of the context before it asks for
// ErrPermanent, would answer a permanent failure with 499 or 503 rather than
// 500.
func causeOf(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return withoutContextEnd{err: err}
	}

	return err
}

// withoutContextEnd keeps a failure of the client, with its message, for
// errors.Is and errors.As, apart from context.Canceled and
// context.DeadlineExceeded, which only the end of the context is to match (see
// causeOf). It has no Unwrap, so that errors.Is can not reach either of them
// through it.
type withoutContextEnd struct {
	err error
}

func (w withoutContextEnd) Error() string {
	return w.err.Error()
}

func (w withoutContextEnd) Is(target error) bool {
	if target == context.Canceled || target == context.DeadlineExceeded {
		return false
	}

	return errors.Is(w.err, target)
}

func (w withoutContextEnd) As(target any) bool {
	return errors.As(w.err, target)
}
