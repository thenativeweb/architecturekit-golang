package architecturekit_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// fakeDatabase answers reading and observing events the way EventSourcingDB
// does, but it can end an observed stream on purpose, which a real database
// only does when it restarts.
type fakeDatabase struct {
	mutex  sync.Mutex
	events []int

	// endObserving tells, for every observed stream by number, starting with
	// 1, whether to end it after sending the events instead of keeping it
	// open.
	endObserving func(connection int) bool
	connections  int

	// cutAfter, if set, ends every observed stream that endObserving keeps
	// open once it has been open that long, as a load balancer does that
	// limits how long a connection may last.
	cutAfter time.Duration

	// readDelay holds back the answer to every read, as a database under load
	// does.
	readDelay time.Duration

	// tampered hands out events whose hash does not match their content, as if
	// they had been changed after they were written.
	tampered bool
}

func (d *fakeDatabase) add(ids ...int) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	d.events = append(d.events, ids...)
}

// newFakeDatabase starts the fake and returns a client for it.
func newFakeDatabase(t *testing.T, database *fakeDatabase) *eventsourcingdb.Client {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Server", "EventSourcingDB/test")

		var body struct {
			Options struct {
				LowerBound *struct {
					ID string `json:"id"`
				} `json:"lowerBound"`
			} `json:"options"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		after := -1
		if body.Options.LowerBound != nil {
			after, _ = strconv.Atoi(body.Options.LowerBound.ID)
		}

		database.mutex.Lock()
		var ids []int
		for _, id := range database.events {
			if id > after {
				ids = append(ids, id)
			}
		}
		isObserving := request.URL.Path == "/api/v1/observe-events"
		endAfterSending := !isObserving
		if isObserving {
			database.connections++
			endAfterSending = database.endObserving(database.connections)
		}
		database.mutex.Unlock()

		if !isObserving {
			time.Sleep(database.readDelay)
		}

		for _, id := range ids {
			writeEvent(writer, id, database.tampered)
			after = id
		}

		if endAfterSending {
			return
		}

		// An open stream sends events that are added later, and heartbeats in
		// between, which let the client notice that its context has ended,
		// since it only looks at the context between two lines.
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()

		// Without cutAfter, the channel stays nil, and the stream is never cut.
		var cut <-chan time.Time
		if database.cutAfter > 0 {
			cut = time.After(database.cutAfter)
		}

		for {
			select {
			case <-request.Context().Done():
				return
			case <-cut:
				return
			case <-ticker.C:
			}

			database.mutex.Lock()
			var added []int
			for _, id := range database.events {
				if id > after {
					added = append(added, id)
				}
			}
			database.mutex.Unlock()

			for _, id := range added {
				writeEvent(writer, id, database.tampered)
				after = id
			}

			writeLine(writer, `{"type":"heartbeat"}`)
		}
	}))
	t.Cleanup(server.Close)

	return clientFor(t, server)
}

// refusingDatabase answers requests to the given path the way EventSourcingDB
// refuses a request, with the given status and reason, and every other request
// with an empty result. It returns a client for it.
func refusingDatabase(t *testing.T, path string, status int, reason string) *eventsourcingdb.Client {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Server", "EventSourcingDB/test")

		if request.URL.Path == path {
			writer.WriteHeader(status)
			_, _ = fmt.Fprint(writer, reason)
		}
	}))
	t.Cleanup(server.Close)

	return clientFor(t, server)
}

// writingDatabase answers every request to the given path with answer, once it
// has read the request completely, as EventSourcingDB does with a write or the
// registration of a schema, and every other request with an empty result, as
// for a subject without events. It returns a client for it.
func writingDatabase(t *testing.T, path string, answer http.HandlerFunc) *eventsourcingdb.Client {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Server", "EventSourcingDB/test")

		if request.URL.Path == path {
			_, _ = io.ReadAll(request.Body)
			answer(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	return clientFor(t, server)
}

// vanishingDatabase answers reading with no events, and stops taking
// connections once it has answered, so that a write after the read can not
// even connect, as with a database that has gone down in between. It returns a
// client for it.
func vanishingDatabase(t *testing.T) *eventsourcingdb.Client {
	t.Helper()

	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Server", "EventSourcingDB/test")
		writer.Header().Set("Connection", "close")
		_ = server.Listener.Close()
	})
	server.Start()
	t.Cleanup(server.Close)

	return clientFor(t, server)
}

// hangingUpDatabase answers reading with no events, and hangs up on a write as
// soon as it has its headers, without reading the events, as a database does
// that goes down while the request arrives. It returns a client for it.
func hangingUpDatabase(t *testing.T) *eventsourcingdb.Client {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Server", "EventSourcingDB/test")

		if request.URL.Path == "/api/v1/write-events" {
			panic(http.ErrAbortHandler)
		}
	}))
	t.Cleanup(server.Close)

	return clientFor(t, server)
}

// clientFor returns a client for the given server.
func clientFor(t *testing.T, server *httptest.Server) *eventsourcingdb.Client {
	t.Helper()

	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)

	client, err := eventsourcingdb.NewClient(serverURL, "secret")
	require.NoError(t, err)

	return client
}

// The fields of every event the fake hands out, apart from its ID and hashes.
const (
	fakeEventTime     = "2026-01-01T00:00:00Z"
	fakeEventSource   = "https://thenativeweb.io"
	fakeEventSubject  = "/test"
	fakeEventType     = "io.thenativeweb.test.incremented"
	fakeEventDataType = "application/json"
	fakeEventData     = `{"by":1}`
)

func writeEvent(writer http.ResponseWriter, id int, tampered bool) {
	hash := fakeHashOf(id)
	if tampered {
		hash = fmt.Sprintf("%064x", id)
	}

	writeLine(writer, fmt.Sprintf(`{"type":"event","payload":%s}`, fakeEvent(id, hash)))
}

// fakeEvent returns the event with the given ID and hash, encoded the way
// EventSourcingDB encodes it.
func fakeEvent(id int, hash string) string {
	return fmt.Sprintf(`{"specversion":"1.0","id":"%d","time":%q,"source":%q,"subject":%q,"type":%q,"datacontenttype":%q,"data":%s,"hash":%q,"predecessorhash":%q}`,
		id, fakeEventTime, fakeEventSource, fakeEventSubject, fakeEventType, fakeEventDataType, fakeEventData, hash, fakeHashOf(id-1))
}

// writtenAnswer is how EventSourcingDB answers a write of a single event to
// the subject of the fake: with the event as it recorded it.
var writtenAnswer = "[" + fakeEvent(0, fakeHashOf(0)) + "]"

// fakeHashOf computes the hash of the event with the given ID the way
// EventSourcingDB does, chained to the hash of the event before it. The first
// event follows a hash of zeros.
func fakeHashOf(id int) string {
	if id < 0 {
		return fmt.Sprintf("%064x", 0)
	}

	metadata := fmt.Sprintf("1.0|%d|%s|%s|%s|%s|%s|%s",
		id, fakeHashOf(id-1), fakeEventTime, fakeEventSource, fakeEventSubject, fakeEventType, fakeEventDataType)
	metadataHash := sha256.Sum256([]byte(metadata))
	dataHash := sha256.Sum256([]byte(fakeEventData))

	return fmt.Sprintf("%x", sha256.Sum256(fmt.Appendf(nil, "%x%x", metadataHash, dataHash)))
}

func writeLine(writer http.ResponseWriter, line string) {
	_, _ = fmt.Fprintln(writer, line)

	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
}
