package architecturekit

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// statusCodePattern finds the status code the database answered with. The
// client exports no typed error, which leaves nothing but its error text.
var statusCodePattern = regexp.MustCompile(`HTTP status code '(\d{3})'`)

// statusCodeOf returns the status code the database answered with, or 0 if
// the failure came without an answer, e.g. because the database is
// unreachable or the connection broke.
func statusCodeOf(err error) int {
	match := statusCodePattern.FindStringSubmatch(err.Error())
	if match == nil {
		return 0
	}

	status, _ := strconv.Atoi(match[1])

	return status
}

// databaseFailure sorts a failure the database reported into a category, by
// what its answer means. The same rules apply to reading and to writing:
//
//   - Without an answer, the database is unreachable or the connection broke,
//     which is transient.
//   - An answer from a server that is not an EventSourcingDB is transient as
//     well. It may come from a wrong address, but also from a proxy in front of
//     the database, which answers on its own while the database restarts, and
//     the client checks who answered before it checks the status code. A wrong
//     address fails on start anyway, so it is named rather than made
//     permanent.
//   - 429 asks to slow down, and 5xx means that the database is unable to
//     answer for now, e.g. because it is shutting down. Both are transient.
//   - 409 means that a precondition did not hold, which is a conflict, or that
//     an event does not match its schema, which is permanent. Only the reason
//     inside the error text tells them apart.
//   - Every other status, such as 400, 401 or 413, means that the request
//     itself is wrong, which is permanent. A rejected API token is named, as it
//     is the one to expect in production, e.g. after the token was rotated.
func databaseFailure(err error, doing string) error {
	status := statusCodeOf(err)

	switch {
	case status == 0:
		if strings.Contains(err.Error(), "server must be EventSourcingDB") {
			return fmt.Errorf("%w: %s: the answer does not come from an EventSourcingDB: %v", ErrTransient, doing, err)
		}
		return fmt.Errorf("%w: %s: %v", ErrTransient, doing, err)

	case status == http.StatusTooManyRequests, status >= http.StatusInternalServerError:
		return fmt.Errorf("%w: %s: %v", ErrTransient, doing, err)

	case status == http.StatusConflict:
		if strings.Contains(err.Error(), "schema conflict") {
			return fmt.Errorf("%w: %s: %v", ErrPermanent, doing, err)
		}
		return fmt.Errorf("%w: %s: %v", ErrConflict, doing, err)

	case status == http.StatusUnauthorized:
		return fmt.Errorf("%w: %s: the database rejected the API token: %v", ErrPermanent, doing, err)

	default:
		return fmt.Errorf("%w: %s: %v", ErrPermanent, doing, err)
	}
}
