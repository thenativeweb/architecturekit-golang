package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	httpapi.Route[noteRequest](api, mux, "POST /note", noteDecider(), options...)

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
			httpapi.Adding(func(handled httpapi.Handled[note]) any {
				return struct {
					ID    string `json:"id"`
					Count int64  `json:"count"`
				}{handled.Command.ID, 9_007_199_254_740_993}
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
			httpapi.Adding(func(httpapi.Handled[note]) any {
				isAsked = true
				return struct{}{}
			}))

		response := postNote(t, mux, `{"id":"1","text":"hello"}`)

		assert.Equal(t, http.StatusServiceUnavailable, response.Code)
		assert.False(t, isAsked, "the fields are only asked for after the command has succeeded")
	})

	for _, test := range []struct {
		name   string
		fields any
		logged string
	}{
		{"a revision in the fields", struct {
			Revision string `json:"revision"`
		}{"mine"}, "must not contain a revision"},
		{"fields that are no JSON object", "just text", "must encode to a JSON object"},
		{"fields that are null", (*struct{})(nil), "must encode to a JSON object"},
		{"fields that can not be encoded", struct{ Callback func() }{func() {}}, "encoding the fields of the answer"},
	} {
		t.Run("with 500 and a log entry for "+test.name, func(t *testing.T) {
			var logs bytes.Buffer
			mux := routed(httpapi.NewAPI(writingStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs))),
				httpapi.Adding(func(httpapi.Handled[note]) any { return test.fields }))

			response := postNote(t, mux, `{"id":"1","text":"hello"}`)

			assert.Equal(t, http.StatusInternalServerError, response.Code)
			assert.JSONEq(t, `{"message": "internal server error"}`, response.Body.String())
			assert.Contains(t, logs.String(), test.logged)
		})
	}

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
		fields := func(httpapi.Handled[note]) any { return struct{}{} }

		assert.PanicsWithValue(t, "architecturekit/httpapi: Adding is given twice", func() {
			routed(httpapi.NewAPI(deadStore(t), userFrom), httpapi.Adding(fields), httpapi.Adding(fields))
		})
	})
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
		httpapi.QueryRevisioned(api, mux, "GET /notes", noteView(), toListNotes, failingNotes, time.Second)

		response := ask(t, mux, "/notes", "golo")

		require.Equal(t, http.StatusInternalServerError, response.Code)
		assert.Contains(t, logs.String(), `route="GET /notes"`)
	})

	t.Run("logs a failure while waiting for a revision, with its route", func(t *testing.T) {
		var logs bytes.Buffer
		api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
		mux := http.NewServeMux()
		httpapi.QueryVarying(api, mux, "GET /notes", brokenView{}, toListNotes, answerListNotes, time.Second, nil)

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
		httpapi.Route[publicNoteRequest](api, mux, "POST /note", noteDecider())

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

// publicNoteRequest is noteRequest for an API without users.
type publicNoteRequest struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

func (r publicNoteRequest) ToCommand(httpapi.NoUser) (note, error) {
	return note(r), nil
}

func TestRespondResultAt(t *testing.T) {
	t.Run("writes the result and the revision it shows", func(t *testing.T) {
		var logs bytes.Buffer
		request, api := inAHandler(&logs)
		recorder := httptest.NewRecorder()

		httpapi.RespondResultAt(recorder, request, api, "7", []int{1, 2}, nil, nil)

		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.Equal(t, "7", recorder.Header().Get(httpapi.HeaderRevision))
		assert.NotEmpty(t, recorder.Header().Get("ETag"))
		assert.JSONEq(t, `[1, 2]`, recorder.Body.String())
	})

	t.Run("logs an internal failure once, through the logger of the API, with the route", func(t *testing.T) {
		var logs bytes.Buffer
		request, api := inAHandler(&logs)
		recorder := httptest.NewRecorder()

		httpapi.RespondResultAt(recorder, request, api, "7", []int{}, errors.New("the index is gone"), nil)

		assert.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.Empty(t, recorder.Header().Get(httpapi.HeaderRevision), "a failure shows no revision")
		assert.Equal(t, 1, strings.Count(logs.String(), "httpapi: internal failure"))
		assert.Contains(t, logs.String(), `route="GET /notes"`)
		assert.Contains(t, logs.String(), "the index is gone")
	})
}

func TestAnsweringWithoutAnAPI(t *testing.T) {
	var noAPI *httpapi.API[user]
	request := httptest.NewRequest(http.MethodGet, "/notes", nil)

	for name, answer := range map[string]func(){
		"Respond":         func() { httpapi.Respond(httptest.NewRecorder(), request, noAPI, nil, nil) },
		"RespondResult":   func() { httpapi.RespondResult(httptest.NewRecorder(), request, noAPI, []int{}, nil) },
		"RespondResultAt": func() { httpapi.RespondResultAt(httptest.NewRecorder(), request, noAPI, "7", []int{}, nil, nil) },
	} {
		t.Run(name+" panics, also on success", func(t *testing.T) {
			assert.PanicsWithValue(t, "architecturekit/httpapi: answering needs the API, not nil", answer)
		})
	}
}
