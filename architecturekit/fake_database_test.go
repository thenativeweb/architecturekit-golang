package architecturekit_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

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

		for _, id := range ids {
			writeEvent(writer, id)
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

		for {
			select {
			case <-request.Context().Done():
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
				writeEvent(writer, id)
				after = id
			}

			writeLine(writer, `{"type":"heartbeat"}`)
		}
	}))
	t.Cleanup(server.Close)

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	client, err := eventsourcingdb.NewClient(serverURL, "secret")
	if err != nil {
		t.Fatal(err)
	}

	return client
}

func writeEvent(writer http.ResponseWriter, id int) {
	writeLine(writer, fmt.Sprintf(`{"type":"event","payload":{"specversion":"1.0","id":"%d","time":"2026-01-01T00:00:00Z","source":"https://thenativeweb.io","subject":"/test","type":"io.thenativeweb.test.incremented","datacontenttype":"application/json","data":{"by":1},"hash":"hash-%d","predecessorhash":"hash-%d"}}`, id, id, id-1))
}

func writeLine(writer http.ResponseWriter, line string) {
	_, _ = fmt.Fprintln(writer, line)

	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
}
