package architecturekit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// answerOf returns the answer of the database that the client reports as a
// DBAPIError, with its status code and the reason the database gave, or nil
// if the failure came without an answer, e.g. because the database is
// unreachable or the connection broke.
func answerOf(err error) *eventsourcingdb.DBAPIError {
	var answer *eventsourcingdb.DBAPIError
	if !errors.As(err, &answer) {
		return nil
	}

	return answer
}

// statusCodeOf returns the status code the database answered with, or 0 if
// the failure came without an answer.
func statusCodeOf(err error) int {
	answer := answerOf(err)
	if answer == nil {
		return 0
	}

	return answer.StatusCode
}

// contextEnded reports that reading or writing stopped because the context
// ended, and keeps the context's error, so that a caller can tell it from a
// failure of the database with errors.Is. It belongs to no category: nothing
// went wrong that trying again could fix, and nothing the request could have
// avoided: the caller stopped waiting, or ran out of time.
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
// Reading has no preconditions, so a 409 is a refusal of the request like any
// other, which is permanent, whatever reason the database gives. The database
// answers a read with it if the latest event of the type that FromLatestEvent
// names comes after the upper bound, and trying again never helps, since new
// events only move the latest one further away. Every other answer is sorted
// as databaseFailure sorts it.
func readFailure(ctx context.Context, err error, doing string) error {
	if ctx.Err() != nil {
		return contextEnded(ctx, doing)
	}

	if statusCodeOf(err) == http.StatusConflict {
		return fmt.Errorf("%w: %s: %v", ErrPermanent, doing, err)
	}

	return databaseFailure(err, doing)
}

// databaseFailure sorts a failure the database reported into a category, by
// what its answer means. The rules apply to writing, and to reading as well,
// apart from 409, which readFailure sorts before it gets here:
//
//   - Without an answer, the database is unreachable or the connection broke,
//     which is transient. The client reports data it can not encode without an
//     answer as well, which is why candidateFor encodes the data of every event
//     before it is written.
//   - An answer from a server that is not an EventSourcingDB is transient as
//     well. It may come from a wrong address, but also from a proxy in front of
//     the database, which answers on its own while the database restarts, and
//     the client checks who answered before it checks the status code. A wrong
//     address fails on start anyway, so it is named rather than made
//     permanent.
//   - 429 asks to slow down, and 5xx means that the database is unable to
//     answer for now, e.g. because it is shutting down. Both are transient.
//   - 409 means, when writing, that a precondition did not hold, which is a
//     conflict, or that an event does not match its schema, which is
//     permanent. Only the reason the database gives tells them apart. When
//     reading, which has no preconditions, a 409 is permanent (see
//     readFailure).
//   - Every other status, such as 400, 401 or 413, means that the request
//     itself is wrong, which is permanent. A rejected API token is named, as it
//     is the one to expect in production, e.g. after the token was rotated.
func databaseFailure(err error, doing string) error {
	status := statusCodeOf(err)

	switch {
	case status == 0:
		if errors.Is(err, eventsourcingdb.ErrInvalidServerHeader) {
			return fmt.Errorf("%w: %s: the answer does not come from an EventSourcingDB: %v", ErrTransient, doing, err)
		}
		return fmt.Errorf("%w: %s: %v", ErrTransient, doing, err)

	case status == http.StatusTooManyRequests, status >= http.StatusInternalServerError:
		return fmt.Errorf("%w: %s: %v", ErrTransient, doing, err)

	case status == http.StatusConflict:
		if strings.HasPrefix(answerOf(err).Reason, "schema conflict") {
			return fmt.Errorf("%w: %s: %v", ErrPermanent, doing, err)
		}
		return fmt.Errorf("%w: %s: %v", ErrConflict, doing, err)

	case status == http.StatusUnauthorized:
		return fmt.Errorf("%w: %s: the database rejected the API token: %v", ErrPermanent, doing, err)

	default:
		return fmt.Errorf("%w: %s: %v", ErrPermanent, doing, err)
	}
}
