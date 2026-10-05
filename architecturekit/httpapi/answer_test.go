package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/httpapi"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/query"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// writingStore is a store on a database without any events that accepts every
// write, numbering the events it writes from 0 on.
func writingStore(t *testing.T) *architecturekit.Store {
	t.Helper()

	var mutex sync.Mutex
	next := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "EventSourcingDB/test")

		if r.URL.Path != "/api/v1/write-events" {
			return
		}

		var request struct {
			Events []struct {
				Source  string          `json:"source"`
				Subject string          `json:"subject"`
				Type    string          `json:"type"`
				Data    json.RawMessage `json:"data"`
			} `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		mutex.Lock()
		defer mutex.Unlock()

		written := make([]map[string]any, len(request.Events))
		for i, event := range request.Events {
			written[i] = map[string]any{
				"specversion":     "1.0",
				"id":              fmt.Sprint(next),
				"time":            time.Now().Format(time.RFC3339Nano),
				"source":          event.Source,
				"subject":         event.Subject,
				"type":            event.Type,
				"datacontenttype": "application/json",
				"data":            event.Data,
				"hash":            "hash",
				"predecessorhash": "predecessor",
			}
			next++
		}

		_ = json.NewEncoder(w).Encode(written)
	}))
	t.Cleanup(server.Close)

	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	client, err := eventsourcingdb.NewClient(serverURL, "secret")
	require.NoError(t, err)

	return architecturekit.NewStore(client, "https://thenativeweb.io")
}

// conflictingStore is a store on a database without any events that refuses
// every write, because a precondition did not hold, and says so the way the
// database does.
func conflictingStore(t *testing.T) *architecturekit.Store {
	t.Helper()

	return refusingStore(t, "/api/v1/write-events", http.StatusConflict, "state conflict: precondition failed")
}

// refusingStore is a store on a database without any events that answers
// every request to the given path with the given status and reason.
func refusingStore(t *testing.T, path string, status int, reason string) *architecturekit.Store {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "EventSourcingDB/test")

		if r.URL.Path != path {
			return
		}

		w.WriteHeader(status)
		_, _ = io.WriteString(w, reason)
	}))
	t.Cleanup(server.Close)

	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	client, err := eventsourcingdb.NewClient(serverURL, "secret")
	require.NoError(t, err)

	return architecturekit.NewStore(client, "https://thenativeweb.io")
}

// inAHandler returns what a handler of its own answers with: a request for
// GET /notes, as a mux hands it over, and an API that logs into logs.
func inAHandler(logs *bytes.Buffer) (*http.Request, *httpapi.API[user]) {
	request := httptest.NewRequest(http.MethodGet, "/notes", nil)
	request.Pattern = "GET /notes"

	return request, httpapi.NewAPI(nil, userFrom, httpapi.WithLogger(loggerInto(logs)))
}

// loggerInto returns a logger that writes into the given buffer.
func loggerInto(buffer *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buffer, nil))
}

func postNote(t *testing.T, mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	t.Helper()

	return send(t, mux, request{user: "golo", contentType: "application/json", body: body})
}

// routed wires the note decider at POST /note, with the given options.
func routed(api *httpapi.API[user], options ...httpapi.RouteOption[note]) *http.ServeMux {
	mux := http.NewServeMux()
	httpapi.Route(api, mux, "POST /note", toNote, noteDecider(), options...)

	return mux
}

func TestRouteAnswers(t *testing.T) {
	t.Run("with the revision it wrote", func(t *testing.T) {
		response := postNote(t, routed(httpapi.NewAPI(writingStore(t), userFrom)), `{"id":"1","text":"hello"}`)

		assert.Equal(t, http.StatusOK, response.Code)
		assert.JSONEq(t, `{"revision": "0"}`, response.Body.String())
	})

	t.Run("with the fields of Adding next to the revision", func(t *testing.T) {
		mux := routed(httpapi.NewAPI(writingStore(t), userFrom),
			httpapi.Adding(func(handled httpapi.Handled[note]) (any, error) {
				return struct {
					ID    string `json:"id"`
					Count int64  `json:"count"`
				}{handled.Command.ID, 9_007_199_254_740_993}, nil
			}))

		response := postNote(t, mux, `{"id":"1","text":"hello"}`)

		assert.Equal(t, http.StatusOK, response.Code)
		assert.JSONEq(t, `{"revision": "0", "id": "1", "count": 9007199254740993}`, response.Body.String())

		// The count is one more than a float can hold exactly, so it shows that
		// the fields keep their numbers as they are. JSONEq compares through
		// floats itself, which is why the text is checked as well.
		assert.Contains(t, response.Body.String(), `"count":9007199254740993`)
	})

	t.Run("without asking Adding after a failure", func(t *testing.T) {
		isAsked := false
		mux := routed(httpapi.NewAPI(deadStore(t), userFrom),
			httpapi.Adding(func(httpapi.Handled[note]) (any, error) {
				isAsked = true
				return struct{}{}, nil
			}))

		response := postNote(t, mux, `{"id":"1","text":"hello"}`)

		assert.Equal(t, http.StatusServiceUnavailable, response.Code)
		assert.False(t, isAsked, "the fields are only asked for after the command has succeeded")
	})

	for _, test := range unusableFields {
		t.Run("with 500 and a log entry for "+test.name, func(t *testing.T) {
			var logs bytes.Buffer
			mux := routed(httpapi.NewAPI(writingStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs))),
				httpapi.Adding(func(httpapi.Handled[note]) (any, error) { return test.fields, nil }))

			response := postNote(t, mux, `{"id":"1","text":"hello"}`)

			assert.Equal(t, http.StatusInternalServerError, response.Code)
			assert.JSONEq(t, `{"message": "internal server error"}`, response.Body.String())
			assert.Contains(t, logs.String(), test.logged)
		})
	}

	for _, test := range []struct {
		name   string
		fields any
	}{
		{"nil", nil},
		{"a nil pointer of a concrete type", (*struct {
			ID string `json:"id"`
		})(nil)},
		{"a nil map", map[string]any(nil)},
		{"a nil slice", []string(nil)},
		{"a MarshalJSON function that returns null", nothingToAdd{}},
	} {
		t.Run("with the revision alone, and without a log entry, for "+test.name, func(t *testing.T) {
			var logs bytes.Buffer
			mux := routed(httpapi.NewAPI(writingStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs))),
				httpapi.Adding(func(httpapi.Handled[note]) (any, error) { return test.fields, nil }))

			response := postNote(t, mux, `{"id":"1","text":"hello"}`)

			assert.Equal(t, http.StatusOK, response.Code)
			assert.JSONEq(t, `{"revision": "0"}`, response.Body.String())
			assert.Empty(t, logs.String(), "a value that encodes to null adds no fields, which is no failure")
		})
	}

	t.Run("as a success when Adding fails after the command has succeeded", func(t *testing.T) {
		var logs bytes.Buffer
		mux := routed(httpapi.NewAPI(writingStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs))),
			httpapi.Adding(func(handled httpapi.Handled[note]) (any, error) {
				return struct {
					ID string `json:"id"`
				}{handled.Command.ID}, errors.New("the files are gone")
			}))

		response := postNote(t, mux, `{"id":"1","text":"hello"}`)

		// The events are written, so the caller needs the revision, and must
		// not send the command again.
		assert.Equal(t, http.StatusOK, response.Code)
		assert.JSONEq(t, `{"revision": "0", "id": "1"}`, response.Body.String(), "the fields returned along with the error are kept")
		assert.Equal(t, 1, strings.Count(logs.String(), "\n"), "want exactly one entry")
		assert.Contains(t, logs.String(), "httpapi: incomplete answer")
		assert.Contains(t, logs.String(), `route="POST /note"`)
		assert.Contains(t, logs.String(), "the files are gone")
		assert.NotContains(t, logs.String(), "dropped", "fields that can be used are kept")
		assert.NotContains(t, logs.String(), "internal failure")
	})

	t.Run("with the revision alone when Adding fails with a nil pointer of a concrete type", func(t *testing.T) {
		// This is how a function that hands on a lookup with the usual shape
		// of Go, a pointer and an error, fails: return lookup(handled).
		lookup := func(httpapi.Handled[note]) (*struct {
			ID string `json:"id"`
		}, error) {
			return nil, errors.New("looking up the answer failed")
		}

		var logs bytes.Buffer
		mux := routed(httpapi.NewAPI(writingStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs))),
			httpapi.Adding(func(handled httpapi.Handled[note]) (any, error) {
				return lookup(handled)
			}))

		response := postNote(t, mux, `{"id":"1","text":"hello"}`)

		assert.Equal(t, http.StatusOK, response.Code)
		assert.JSONEq(t, `{"revision": "0"}`, response.Body.String())
		assert.Equal(t, 1, strings.Count(logs.String(), "\n"), "want exactly one entry")
		assert.Contains(t, logs.String(), "httpapi: incomplete answer")
		assert.Contains(t, logs.String(), "looking up the answer failed")
		assert.NotContains(t, logs.String(), "dropped", "null holds no fields, so nothing was dropped")
		assert.NotContains(t, logs.String(), "internal failure")
	})

	for _, test := range unusableFields {
		t.Run("as a success, without the fields, when Adding fails with "+test.name, func(t *testing.T) {
			var logs bytes.Buffer
			mux := routed(httpapi.NewAPI(writingStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs))),
				httpapi.Adding(func(httpapi.Handled[note]) (any, error) {
					return test.fields, errors.New("the files are gone")
				}))

			response := postNote(t, mux, `{"id":"1","text":"hello"}`)

			// The events are written, so whatever is wrong with the fields, the
			// caller needs the revision, and must not send the command again.
			assert.Equal(t, http.StatusOK, response.Code)
			assert.JSONEq(t, `{"revision": "0"}`, response.Body.String())
			assert.Equal(t, 1, strings.Count(logs.String(), "\n"), "want exactly one entry")
			assert.Contains(t, logs.String(), `level=ERROR msg="httpapi: incomplete answer"`)
			assert.Contains(t, logs.String(), `route="POST /note"`)
			assert.Contains(t, logs.String(), `error="the files are gone"`)
			assert.Contains(t, logs.String(), "dropped=")
			assert.Contains(t, logs.String(), test.logged)
			assert.NotContains(t, logs.String(), "internal failure")
		})
	}

	t.Run("with the revision alone when Adding fails without fields", func(t *testing.T) {
		var logs bytes.Buffer
		mux := routed(httpapi.NewAPI(writingStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs))),
			httpapi.Adding(func(httpapi.Handled[note]) (any, error) {
				return nil, errors.New("nothing to add")
			}))

		response := postNote(t, mux, `{"id":"1","text":"hello"}`)

		assert.Equal(t, http.StatusOK, response.Code)
		assert.JSONEq(t, `{"revision": "0"}`, response.Body.String())
		assert.Contains(t, logs.String(), "nothing to add")
	})

	t.Run("with a fixed text for 401, and the details in the log", func(t *testing.T) {
		var logs bytes.Buffer
		mux := routed(httpapi.NewAPI(writingStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs))))

		response := send(t, mux, request{contentType: "application/json", body: `{"id":"1","text":"hello"}`})

		assert.Equal(t, http.StatusUnauthorized, response.Code)
		assert.JSONEq(t, `{"message": "unauthorized"}`, response.Body.String())
		assertRefusalLogged(t, logs.String(), "POST", "POST /note", http.StatusUnauthorized, "no user given")
	})

	t.Run("with a fixed text for 409, and the details in the log", func(t *testing.T) {
		var logs bytes.Buffer
		mux := routed(httpapi.NewAPI(conflictingStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs))))

		response := postNote(t, mux, `{"id":"1","text":"hello"}`)

		// The error names the subject and what the database answered, which is
		// nothing the caller needs to know.
		assert.Equal(t, http.StatusConflict, response.Code)
		assert.JSONEq(t, `{"message": "conflict: the data has changed since it was read"}`, response.Body.String())
		assertRefusalLogged(t, logs.String(), "POST", "POST /note", http.StatusConflict, `writing \"/note/1\"`)
		assert.Contains(t, logs.String(), "state conflict: precondition failed")
	})

	t.Run("explains a failure the caller can fix, as before", func(t *testing.T) {
		mux := routed(httpapi.NewAPI(writingStore(t), userFrom))

		response := postNote(t, mux, `{"text":"no id"}`)

		assert.Equal(t, http.StatusBadRequest, response.Code)
		assert.Contains(t, response.Body.String(), `"message"`)
		assert.NotContains(t, response.Body.String(), "revision")
	})
}

func TestAdding(t *testing.T) {
	t.Run("panics for a nil function", func(t *testing.T) {
		assert.PanicsWithValue(t, "architecturekit/httpapi: Adding needs a function, not nil", func() {
			httpapi.Adding[note](nil)
		})
	})

	t.Run("panics if given twice", func(t *testing.T) {
		fields := func(httpapi.Handled[note]) (any, error) { return struct{}{}, nil }

		assert.PanicsWithValue(t, "architecturekit/httpapi: Adding is given twice", func() {
			routed(httpapi.NewAPI(deadStore(t), userFrom), httpapi.Adding(fields), httpapi.Adding(fields))
		})
	})
}

// unusableFields are values that Adding can not add to an answer, along with
// what the log says about each of them.
var unusableFields = []struct {
	name   string
	fields any
	logged string
}{
	{"a revision in the fields", struct {
		Revision string `json:"revision"`
	}{"mine"}, "must not contain a revision"},
	{"fields that are no JSON object", "just text", "must encode to a JSON object"},
	{"fields that are an empty list", []string{}, "must encode to a JSON object"},
	{"fields that can not be encoded", struct{ Callback func() }{func() {}}, "encoding the fields of the answer"},
	{"fields that hold NaN", struct {
		Score float64 `json:"score"`
	}{math.NaN()}, "unsupported value: NaN"},
	{"fields whose encoding fails with a category", unencodable{}, "encoding the fields of the answer"},
	{"fields whose encoding panics", panicking{}, "the note panicked"},
}

// unencodable fails while it is encoded, with an error that has a category of
// its own, as a MarshalJSON function with a bug might. The answer exists all
// the same, so that category must not decide the status.
type unencodable struct{}

func (unencodable) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("%w: the note is gone", httpapi.ErrNotFound)
}

// panicking panics while it is encoded, as a MarshalJSON function with a bug
// might.
type panicking struct{}

func (panicking) MarshalJSON() ([]byte, error) {
	panic("the note panicked")
}

// nothingToAdd encodes to null, which holds no fields.
type nothingToAdd struct{}

func (nothingToAdd) MarshalJSON() ([]byte, error) {
	return []byte("null"), nil
}

// failingNotes answers every query with a failure the caller is not told
// about.
func failingNotes(context.Context, listNotes) ([]noteResponse, error) {
	return nil, errors.New("the index is gone")
}

func TestWithLogger(t *testing.T) {
	t.Run("logs an internal failure of a route once, with its method and route", func(t *testing.T) {
		var logs bytes.Buffer
		mux := routed(httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs))))

		defaults := logsOf(func() {
			response := postNote(t, mux, `{"id":"1","text":"hello"}`)
			require.Equal(t, http.StatusServiceUnavailable, response.Code)
		})

		assert.Equal(t, 1, strings.Count(logs.String(), "httpapi: internal failure"), "want exactly one entry")
		assert.Contains(t, logs.String(), "method=POST")
		assert.Contains(t, logs.String(), `route="POST /note"`)
		assert.Empty(t, defaults, "nothing must go to the default logger as well")
	})

	t.Run("does not log a failure the caller can fix", func(t *testing.T) {
		var logs bytes.Buffer
		mux := routed(httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs))))

		response := postNote(t, mux, `{"text":"no id"}`)

		require.Equal(t, http.StatusBadRequest, response.Code)
		assert.Empty(t, logs.String())
	})

	t.Run("logs an internal failure of a query, with its route", func(t *testing.T) {
		var logs bytes.Buffer
		api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
		mux := http.NewServeMux()
		httpapi.Query(api, mux, "GET /notes", toListNotes, failingNotes)

		response := ask(t, mux, "/notes", "golo")

		require.Equal(t, http.StatusInternalServerError, response.Code)
		assert.Equal(t, 1, strings.Count(logs.String(), "httpapi: internal failure"))
		assert.Contains(t, logs.String(), `route="GET /notes"`)
		assert.Contains(t, logs.String(), "the index is gone")
	})

	t.Run("logs an internal failure of a revisioned query, with its route", func(t *testing.T) {
		var logs bytes.Buffer
		api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
		mux := http.NewServeMux()
		httpapi.Query(api, mux, "GET /notes", toListNotes, failingNotes, httpapi.Revisioned(noteView(), time.Second))

		response := ask(t, mux, "/notes", "golo")

		require.Equal(t, http.StatusInternalServerError, response.Code)
		assert.Contains(t, logs.String(), `route="GET /notes"`)
	})

	t.Run("logs a failure while waiting for a revision, with its route", func(t *testing.T) {
		var logs bytes.Buffer
		api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
		mux := http.NewServeMux()
		httpapi.Query(api, mux, "GET /notes", toListNotes, answerListNotes, httpapi.Revisioned(brokenView{}, time.Second))

		request := httptest.NewRequest(http.MethodGet, "/notes", nil)
		request.Header.Set("X-User", "golo")
		request.Header.Set(httpapi.HeaderWaitFor, "7")
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)

		require.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.Contains(t, logs.String(), `route="GET /notes"`)
		assert.Contains(t, logs.String(), "the view is broken")
	})

	t.Run("falls back to the default logger", func(t *testing.T) {
		mux := routed(httpapi.NewAPI(deadStore(t), userFrom))

		defaults := logsOf(func() {
			postNote(t, mux, `{"id":"1","text":"hello"}`)
		})

		assert.Contains(t, defaults, `route="POST /note"`)
	})

	t.Run("works for a public API as well", func(t *testing.T) {
		var logs bytes.Buffer
		api := httpapi.NewPublicAPI(deadStore(t), httpapi.WithLogger(loggerInto(&logs)))
		mux := http.NewServeMux()
		httpapi.Route(api, mux, "POST /note", toPublicNote, noteDecider())

		send(t, mux, request{contentType: "application/json", body: `{"id":"1","text":"hello"}`})

		assert.Contains(t, logs.String(), `route="POST /note"`)
	})

	t.Run("panics for a nil logger", func(t *testing.T) {
		assert.PanicsWithValue(t, "architecturekit/httpapi: WithLogger needs a logger, not nil", func() {
			httpapi.WithLogger(nil)
		})
	})
}

// brokenView fails while it waits for a revision, with a failure the caller is
// not told about.
type brokenView struct{}

func (brokenView) Revision() string { return "" }

func (brokenView) WaitFor(context.Context, string) error { return errors.New("the view is broken") }

// toPublicNote is toNote for an API without users.
func toPublicNote(_ *http.Request, request noteRequest, _ httpapi.NoUser) (note, error) {
	return note(request), nil
}

func TestAnsweringWithoutAnAPI(t *testing.T) {
	var noAPI *httpapi.API[user]
	request := httptest.NewRequest(http.MethodGet, "/notes", nil)

	for name, answer := range map[string]func(){
		"Respond":       func() { httpapi.Respond(httptest.NewRecorder(), request, noAPI, nil, nil) },
		"RespondResult": func() { httpapi.RespondResult(httptest.NewRecorder(), request, noAPI, []int{}, nil) },
	} {
		t.Run(name+" panics, also on success", func(t *testing.T) {
			assert.PanicsWithValue(t, "architecturekit/httpapi: answering needs the API, not nil", answer)
		})
	}
}

func TestAnsweringWhenTheContextEnded(t *testing.T) {
	t.Run("a caller who went away gets 499, which is not logged", func(t *testing.T) {
		var logs bytes.Buffer
		request, api := inAHandler(&logs)
		recorder := httptest.NewRecorder()

		httpapi.Respond(recorder, request, api, nil, fmt.Errorf("architecturekit: reading %q: %w", "/notes/1", context.Canceled))

		assert.Equal(t, 499, recorder.Code)
		assert.Empty(t, logs.String(), "a caller who went away is no failure of the server")
	})

	t.Run("a deadline that ran out gets 503, which is logged", func(t *testing.T) {
		var logs bytes.Buffer
		request, api := inAHandler(&logs)
		recorder := httptest.NewRecorder()

		httpapi.Respond(recorder, request, api, nil, fmt.Errorf("architecturekit: reading %q: %w", "/notes/1", context.DeadlineExceeded))

		assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
		assert.Contains(t, logs.String(), "httpapi: internal failure")
		assert.Contains(t, logs.String(), "context deadline exceeded")
	})
}

// assertRefusalLogged asserts that the logs hold exactly one entry, at level
// Info, for a refusal whose details the caller was not told.
func assertRefusalLogged(t *testing.T, logs, method, route string, status int, detail string) {
	t.Helper()

	assert.Equal(t, 1, strings.Count(logs, "\n"), "want exactly one entry")
	assert.Contains(t, logs, `level=INFO msg="httpapi: request refused"`)
	assert.Contains(t, logs, "method="+method)
	assert.Contains(t, logs, fmt.Sprintf("route=%q", route))
	assert.Contains(t, logs, fmt.Sprintf("status=%d", status))
	assert.Contains(t, logs, detail, "the details have to reach the log")
	assert.NotContains(t, logs, "internal failure", "a refusal is no failure of the server")
}

// answerers answer in a handler of your own, with no result and the given
// error, once for a command and once for a query.
var answerers = map[string]func(w http.ResponseWriter, r *http.Request, api *httpapi.API[user], err error){
	"Respond": func(w http.ResponseWriter, r *http.Request, api *httpapi.API[user], err error) {
		httpapi.Respond(w, r, api, nil, err)
	},
	"RespondResult": func(w http.ResponseWriter, r *http.Request, api *httpapi.API[user], err error) {
		httpapi.RespondResult(w, r, api, []noteResponse(nil), err)
	},
}

func TestAnsweringRefusals(t *testing.T) {
	// The errors are those of real answers, which name a key and a subject.
	refusals := []struct {
		label   string
		err     error
		status  int
		message string
		detail  string
	}{
		{
			label:   "401",
			err:     fmt.Errorf("%w: token signed with key kid=prod-2026-09 failed verification: crypto/ed25519: verification error", httpapi.ErrUnauthorized),
			status:  http.StatusUnauthorized,
			message: `{"message": "unauthorized"}`,
			detail:  "kid=prod-2026-09",
		},
		{
			label:   "409",
			err:     fmt.Errorf("%w: writing %q: failed to write events, got HTTP status code '409', expected '200': state conflict: precondition failed", architecturekit.ErrConflict, "/tenants/acme-bank/books/42"),
			status:  http.StatusConflict,
			message: `{"message": "conflict: the data has changed since it was read"}`,
			detail:  "/tenants/acme-bank/books/42",
		},
		{
			label:   "404 of a query that found no item",
			err:     fmt.Errorf("finding %q: %w", "/tenants/acme-bank/books/42", query.ErrNoItems),
			status:  http.StatusNotFound,
			message: `{"message": "not found"}`,
			detail:  "/tenants/acme-bank/books/42",
		},
	}

	// Each of these errors is written for the caller, so it is the message.
	explained := []struct {
		label   string
		err     error
		status  int
		message string
	}{
		{"400", fmt.Errorf("%w: id must not be empty", httpapi.ErrMalformed), http.StatusBadRequest, "malformed request: id must not be empty"},
		{"400 for a value that is not a revision", fmt.Errorf("%w: %q", architecturekit.ErrNotARevision, "abc"), http.StatusBadRequest, `not a revision: "abc"`},
		{"403", fmt.Errorf("%w: only librarians acquire books", httpapi.ErrForbidden), http.StatusForbidden, "forbidden: only librarians acquire books"},
		{"404", fmt.Errorf("%w: book 42 is unknown", httpapi.ErrNotFound), http.StatusNotFound, "not found: book 42 is unknown"},
		{"413", fmt.Errorf("%w: at most 1 byte is read", httpapi.ErrTooLarge), http.StatusRequestEntityTooLarge, "request body too large: at most 1 byte is read"},
		{"415", fmt.Errorf("%w: text/plain is not application/json", httpapi.ErrUnsupportedMediaType), http.StatusUnsupportedMediaType, "unsupported media type: text/plain is not application/json"},
		{"422", architecturekit.NewDomainError("book 42 is already borrowed"), http.StatusUnprocessableEntity, "book 42 is already borrowed"},
		{"422 for an error of the category alone", fmt.Errorf("%w: book 42 is already borrowed", architecturekit.ErrDomain), http.StatusUnprocessableEntity, "domain rule violated: book 42 is already borrowed"},
	}

	for name, answer := range answerers {
		for _, refusal := range refusals {
			t.Run(name+" answers "+refusal.label+" with a fixed text, and logs the details as information", func(t *testing.T) {
				var logs bytes.Buffer
				request, api := inAHandler(&logs)
				recorder := httptest.NewRecorder()

				defaults := logsOf(func() {
					answer(recorder, request, api, refusal.err)
				})

				assert.Equal(t, refusal.status, recorder.Code)
				assert.JSONEq(t, refusal.message, recorder.Body.String())
				assertRefusalLogged(t, logs.String(), "GET", "GET /notes", refusal.status, refusal.detail)
				assert.Empty(t, defaults, "nothing must go to the default logger as well")
			})
		}

		for _, failure := range explained {
			t.Run(name+" answers "+failure.label+" with the error itself, without logging it", func(t *testing.T) {
				var logs bytes.Buffer
				request, api := inAHandler(&logs)
				recorder := httptest.NewRecorder()

				answer(recorder, request, api, failure.err)

				assert.Equal(t, failure.status, recorder.Code)

				expected, err := json.Marshal(map[string]string{"message": failure.message})
				require.NoError(t, err)
				assert.JSONEq(t, string(expected), recorder.Body.String())
				assert.Empty(t, logs.String(), "a failure the caller can fix must not be logged")
			})
		}

		t.Run(name+" answers 500 with a fixed text, and logs the failure as an error", func(t *testing.T) {
			var logs bytes.Buffer
			request, api := inAHandler(&logs)
			recorder := httptest.NewRecorder()

			answer(recorder, request, api, fmt.Errorf("%w: the password is hunter2", architecturekit.ErrPermanent))

			assert.Equal(t, http.StatusInternalServerError, recorder.Code)
			assert.JSONEq(t, `{"message": "internal server error"}`, recorder.Body.String())
			assert.Contains(t, logs.String(), `level=ERROR msg="httpapi: internal failure"`)
			assert.Contains(t, logs.String(), "hunter2")
		})
	}
}
