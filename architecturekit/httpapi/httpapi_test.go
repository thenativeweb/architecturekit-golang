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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/httpapi"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/query"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// --- a domain just large enough to drive the HTTP layer ---

type noted struct {
	Text string `json:"text"`
}

func (noted) EventType() string { return "io.thenativeweb.httpapi.noted" }

func (noted) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"text": map[string]any{"type": "string"},
		},
	}
}

type notes struct {
	Count int
}

type note struct {
	ID   string
	Text string
}

func (c note) Subject() string { return "/note/" + c.ID }

func (c note) Preconditions() []architecturekit.Precondition {
	return []architecturekit.Precondition{architecturekit.Unconditionally()}
}

func noteDecider() architecturekit.Decider[note, notes] {
	state := architecturekit.NewState(notes{})
	state.Evolve(func(current notes, event noted) notes {
		current.Count++
		return current
	})

	return architecturekit.NewDecider(state,
		func(ctx context.Context, cmd note, current notes) ([]architecturekit.Event, error) {
			if current.Count > 0 {
				return nil, architecturekit.NewDomainError("note %s already exists", cmd.ID)
			}
			return []architecturekit.Event{noted{Text: cmd.Text}}, nil
		})
}

type user struct {
	UserID string
}

func userFrom(r *http.Request) (user, error) {
	id := r.Header.Get("X-User")
	if id == "" {
		return user{}, errors.New("no user given")
	}
	return user{UserID: id}, nil
}

type noteRequest struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

func toNote(_ *http.Request, request noteRequest, _ user) (note, error) {
	if request.ID == "" {
		return note{}, errors.New("id must not be empty")
	}
	return note(request), nil
}

// --- helpers ---

// deadStore points at a port where nothing listens. Every test that fails
// before the store is touched can use it, which keeps those tests fast.
func deadStore(t *testing.T) *architecturekit.Store {
	t.Helper()

	deadURL, err := url.Parse("http://127.0.0.1:1")
	require.NoError(t, err)
	client, err := eventsourcingdb.NewClient(deadURL, "secret")
	require.NoError(t, err)

	return architecturekit.NewStore(client, "https://thenativeweb.io")
}

func muxFor(t *testing.T, store *architecturekit.Store) *http.ServeMux {
	t.Helper()

	api := httpapi.NewAPI(store, userFrom)
	mux := http.NewServeMux()
	httpapi.Route(api, mux, "POST /note", toNote, noteDecider())

	return mux
}

type request struct {
	user, contentType, body string
}

func send(t *testing.T, mux *http.ServeMux, r request) *httptest.ResponseRecorder {
	t.Helper()

	httpRequest := httptest.NewRequest(http.MethodPost, "/note", strings.NewReader(r.body))
	if r.user != "" {
		httpRequest.Header.Set("X-User", r.user)
	}
	if r.contentType != "" {
		httpRequest.Header.Set("Content-Type", r.contentType)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httpRequest)

	return recorder
}

// postingTo returns a request of a known user that posts the body to the
// path.
func postingTo(path, body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("X-User", "golo")
	request.Header.Set("Content-Type", "application/json")

	return request
}

// --- StatusFor, without any infrastructure ---

func TestStatusFor(t *testing.T) {
	t.Run("maps every category", func(t *testing.T) {
		cases := []struct {
			label string
			err   error
			want  int
		}{
			{"no error", nil, http.StatusOK},
			{"unauthorized", httpapi.ErrUnauthorized, http.StatusUnauthorized},
			{"too large", httpapi.ErrTooLarge, http.StatusRequestEntityTooLarge},
			{"wrong media type", httpapi.ErrUnsupportedMediaType, http.StatusUnsupportedMediaType},
			{"malformed", httpapi.ErrMalformed, http.StatusBadRequest},
			{"domain rule", architecturekit.NewDomainError("nope"), http.StatusUnprocessableEntity},
			{"conflict", architecturekit.ErrConflict, http.StatusConflict},
			{"transient", architecturekit.ErrTransient, http.StatusServiceUnavailable},
			{"permanent", architecturekit.ErrPermanent, http.StatusInternalServerError},
			{"unverified", architecturekit.ErrUnverified, http.StatusInternalServerError},
			{"anything else", errors.New("who knows"), http.StatusInternalServerError},
			{"a caller who went away", fmt.Errorf("architecturekit: reading: %w", context.Canceled), 499},
			{"a deadline that ran out", fmt.Errorf("architecturekit: reading: %w", context.DeadlineExceeded), http.StatusServiceUnavailable},
		}

		for _, c := range cases {
			t.Run(c.label, func(t *testing.T) {
				assert.Equal(t, c.want, httpapi.StatusFor(c.err))
			})
		}
	})

	t.Run("prefers conflict over its category", func(t *testing.T) {
		// A conflict is transient, so the order of the cases decides. 409 says
		// more than 503, hence it has to win.
		require.ErrorIs(t, architecturekit.ErrConflict, architecturekit.ErrTransient, "a conflict is expected to be transient")
		assert.Equal(t, http.StatusConflict, httpapi.StatusFor(architecturekit.ErrConflict))
	})

	t.Run("maps a value that is not a revision to 400, unless it is a permanent failure", func(t *testing.T) {
		// A value that is not a revision was handed over, usually from the
		// request, while one that is permanent as well is an ID that the server
		// stored or made itself.
		notARevision := fmt.Errorf("%w: %q", architecturekit.ErrNotARevision, "abc")

		cases := []struct {
			label string
			err   error
			want  int
		}{
			{"alone", architecturekit.ErrNotARevision, http.StatusBadRequest},
			{"as CompareRevisions returns it", notARevision, http.StatusBadRequest},
			{"wrapped", fmt.Errorf("listing the books: %w", notARevision), http.StatusBadRequest},
			{"joined with a permanent failure", errors.Join(architecturekit.ErrPermanent, notARevision), http.StatusInternalServerError},
			{"joined with a permanent failure, the other way round", errors.Join(notARevision, architecturekit.ErrPermanent), http.StatusInternalServerError},
			{"wrapped as a permanent failure", fmt.Errorf("%w: %w", architecturekit.ErrPermanent, notARevision), http.StatusInternalServerError},
			{"wrapping a permanent failure", fmt.Errorf("%w: %w", notARevision, architecturekit.ErrPermanent), http.StatusInternalServerError},
			{"joined with an event that could not be verified", errors.Join(architecturekit.ErrUnverified, notARevision), http.StatusInternalServerError},
		}

		for _, c := range cases {
			t.Run(c.label, func(t *testing.T) {
				assert.Equal(t, c.want, httpapi.StatusFor(c.err))
			})
		}
	})

	t.Run("keeps the status of every other category, also for an error that is permanent or not a revision as well", func(t *testing.T) {
		// Both come last, so that an error with a status of its own keeps it,
		// such as a permanent one that userFrom wraps with ErrUnauthorized to
		// have it answered with 401.
		categories := []struct {
			label string
			err   error
			want  int
		}{
			{"unauthorized", httpapi.ErrUnauthorized, http.StatusUnauthorized},
			{"forbidden", httpapi.ErrForbidden, http.StatusForbidden},
			{"too large", httpapi.ErrTooLarge, http.StatusRequestEntityTooLarge},
			{"wrong media type", httpapi.ErrUnsupportedMediaType, http.StatusUnsupportedMediaType},
			{"malformed", httpapi.ErrMalformed, http.StatusBadRequest},
			{"not found", httpapi.ErrNotFound, http.StatusNotFound},
			{"no items", query.ErrNoItems, http.StatusNotFound},
			{"domain rule", architecturekit.NewDomainError("nope"), http.StatusUnprocessableEntity},
			{"conflict", architecturekit.ErrConflict, http.StatusConflict},
			{"transient", architecturekit.ErrTransient, http.StatusServiceUnavailable},
			{"a caller who went away", context.Canceled, 499},
			{"a deadline that ran out", context.DeadlineExceeded, http.StatusServiceUnavailable},
		}

		for _, category := range categories {
			for _, other := range []error{architecturekit.ErrPermanent, architecturekit.ErrNotARevision} {
				t.Run(category.label+" and "+other.Error(), func(t *testing.T) {
					assert.Equal(t, category.want, httpapi.StatusFor(errors.Join(category.err, other)))
					assert.Equal(t, category.want, httpapi.StatusFor(errors.Join(other, category.err)))
					assert.Equal(t, category.want, httpapi.StatusFor(fmt.Errorf("%w: %w", category.err, other)))
				})
			}
		}
	})

	t.Run("maps a failure of the database by its category, although it wraps the error of the client", func(t *testing.T) {
		// The kit wraps the error of the client after the category, so that
		// errors.As reaches the answer of the database. The category decides
		// all the same, so the status is the one of the category.
		write := func(store *architecturekit.Store) error {
			_, err := architecturekit.Write(context.Background(), store,
				[]architecturekit.EventOn{{Subject: "/notes/1", Event: noted{Text: "hello"}}}, architecturekit.Unconditionally())

			return err
		}
		read := func(store *architecturekit.Store) error {
			for _, err := range architecturekit.Read(context.Background(), store, architecturekit.ExactSubject("/notes/1")) {
				if err != nil {
					return err
				}
			}

			return nil
		}

		cases := []struct {
			label  string
			path   string
			call   func(store *architecturekit.Store) error
			status int
			reason string
			want   int
		}{
			{"a bad request", "/api/v1/write-events", write, http.StatusBadRequest, "bad request", http.StatusInternalServerError},
			{"a rejected API token", "/api/v1/write-events", write, http.StatusUnauthorized, "unauthorized", http.StatusInternalServerError},
			{"a request that is too large", "/api/v1/write-events", write, http.StatusRequestEntityTooLarge, "too large", http.StatusInternalServerError},
			{"a failed precondition", "/api/v1/write-events", write, http.StatusConflict, "state conflict: precondition failed", http.StatusConflict},
			{"a schema violation", "/api/v1/write-events", write, http.StatusConflict, "schema conflict: event does not match", http.StatusInternalServerError},
			{"too many requests", "/api/v1/write-events", write, http.StatusTooManyRequests, "slow down", http.StatusServiceUnavailable},
			{"an internal error", "/api/v1/write-events", write, http.StatusInternalServerError, "failed", http.StatusServiceUnavailable},
			{"an unavailable database", "/api/v1/write-events", write, http.StatusServiceUnavailable, "shutting down", http.StatusServiceUnavailable},
			{"a 409 when reading", "/api/v1/read-events", read, http.StatusConflict, "state conflict: beyond the upper bound", http.StatusInternalServerError},
		}

		for _, c := range cases {
			t.Run(c.label, func(t *testing.T) {
				err := c.call(refusingStore(t, c.path, c.status, c.reason))
				assert.Equal(t, c.want, httpapi.StatusFor(err))

				answer, isAnswer := errors.AsType[*eventsourcingdb.DBAPIError](err)
				require.True(t, isAnswer, "errors.As has to reach the answer of the database, got: %v", err)
				assert.Equal(t, c.status, answer.StatusCode)
				assert.Equal(t, c.reason, answer.Reason)
			})
		}

		t.Run("an answer that does not come from an EventSourcingDB", func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
			}))
			t.Cleanup(server.Close)

			serverURL, err := url.Parse(server.URL)
			require.NoError(t, err)
			client, err := eventsourcingdb.NewClient(serverURL, "secret")
			require.NoError(t, err)

			err = write(architecturekit.NewStore(client, "https://thenativeweb.io"))

			assert.Equal(t, http.StatusServiceUnavailable, httpapi.StatusFor(err))
			assert.ErrorIs(t, err, eventsourcingdb.ErrInvalidServerHeader, "errors.Is has to reach the error of the client")
		})
	})
}

func TestCategoryTexts(t *testing.T) {
	// An error that is written for the caller is answered with its text, so the
	// text of its category names what is wrong rather than the package it
	// comes from.
	for _, test := range []struct {
		err  error
		text string
	}{
		{httpapi.ErrUnauthorized, "unauthorized"},
		{httpapi.ErrUnsupportedMediaType, "unsupported media type"},
		{httpapi.ErrTooLarge, "request body too large"},
		{httpapi.ErrForbidden, "forbidden"},
		{httpapi.ErrMalformed, "malformed request"},
		{httpapi.ErrNotFound, "not found"},
	} {
		t.Run(test.text, func(t *testing.T) {
			assert.EqualError(t, test.err, test.text)
		})
	}
}

func TestRespond(t *testing.T) {
	t.Run("reports the revision on success", func(t *testing.T) {
		var logs bytes.Buffer
		request, api := inAHandler(&logs)
		recorder := httptest.NewRecorder()

		httpapi.Respond(recorder, request, api, []eventsourcingdb.Event{{ID: "0"}, {ID: "1"}}, nil)

		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.Equal(t, "application/json", recorder.Header().Get("Content-Type"))
		assert.JSONEq(t, `{"revision": "1"}`, recorder.Body.String(), "want the ID of the last event, and nothing else")
	})

	t.Run("reports an empty revision if nothing was written", func(t *testing.T) {
		var logs bytes.Buffer
		request, api := inAHandler(&logs)
		recorder := httptest.NewRecorder()

		httpapi.Respond(recorder, request, api, nil, nil)

		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.JSONEq(t, `{"revision": ""}`, recorder.Body.String())
	})

	t.Run("keeps internal failures to itself", func(t *testing.T) {
		var logs bytes.Buffer
		request, api := inAHandler(&logs)
		recorder := httptest.NewRecorder()

		httpapi.Respond(recorder, request, api, nil, errors.New("the password is hunter2"))

		assert.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.NotContains(t, recorder.Body.String(), "hunter2", "an internal failure must not be explained")
	})

	t.Run("logs internal failures once, through the logger of the API, with the route", func(t *testing.T) {
		var logs bytes.Buffer
		request, api := inAHandler(&logs)

		defaults := logsOf(func() {
			httpapi.Respond(httptest.NewRecorder(), request, api, nil, errors.New("the database is gone"))
		})

		assert.Equal(t, 1, strings.Count(logs.String(), "httpapi: internal failure"))
		assert.Contains(t, logs.String(), "the database is gone", "an internal failure must be logged")
		assert.Contains(t, logs.String(), `route="GET /notes"`)
		assert.Empty(t, defaults, "nothing must go to the default logger as well")
	})

	t.Run("does not log failures the caller can fix", func(t *testing.T) {
		var logs bytes.Buffer
		request, api := inAHandler(&logs)

		httpapi.Respond(httptest.NewRecorder(), request, api, nil, architecturekit.NewDomainError("note 7 already exists"))

		assert.Empty(t, logs.String(), "a failure the caller can fix must not be logged")
	})

	t.Run("explains failures the caller can fix", func(t *testing.T) {
		var logs bytes.Buffer
		request, api := inAHandler(&logs)
		recorder := httptest.NewRecorder()

		httpapi.Respond(recorder, request, api, nil, architecturekit.NewDomainError("note 7 already exists"))

		assert.Equal(t, http.StatusUnprocessableEntity, recorder.Code)
		assert.Contains(t, recorder.Body.String(), "note 7 already exists")
	})
}

// logsOf returns what the kit logs with the default logger of log/slog while
// fn runs.
func logsOf(fn func()) string {
	var buffer bytes.Buffer

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buffer, nil)))
	defer slog.SetDefault(previous)

	fn()

	return buffer.String()
}

// --- everything that fails before the store is touched ---

func TestRoute(t *testing.T) {
	t.Run("a request without a user is unauthorized", func(t *testing.T) {
		response := send(t, muxFor(t, deadStore(t)), request{
			contentType: "application/json",
			body:        `{"id":"1"}`,
		})

		assert.Equal(t, http.StatusUnauthorized, response.Code)
	})

	t.Run("the content type is required", func(t *testing.T) {
		mux := muxFor(t, deadStore(t))

		for _, c := range []struct {
			label       string
			contentType string
		}{
			{"missing", ""},
			{"the CSRF-friendly one", "text/plain"},
			{"simply wrong", "application/xml"},
			{"a form", "application/x-www-form-urlencoded"},
			{"would pass a substring test", "text/plain; application/json"},
			{"not a media type at all", "not a media type at all;;;"},
		} {
			t.Run(c.label, func(t *testing.T) {
				response := send(t, mux, request{
					user:        "golo",
					contentType: c.contentType,
					body:        `{"id":"1"}`,
				})

				assert.Equal(t, http.StatusUnsupportedMediaType, response.Code)
			})
		}
	})

	t.Run("a content type with parameters is accepted", func(t *testing.T) {
		// The media type is parsed, so a charset does not get in the way. This one
		// reaches the store and fails there, which is enough to show it passed the
		// media type check.
		response := send(t, muxFor(t, deadStore(t)), request{
			user:        "golo",
			contentType: "application/json; charset=utf-8",
			body:        `{"id":"1"}`,
		})

		assert.NotEqual(t, http.StatusUnsupportedMediaType, response.Code, "a charset must not be rejected")
	})

	t.Run("malformed JSON is rejected", func(t *testing.T) {
		response := send(t, muxFor(t, deadStore(t)), request{
			user:        "golo",
			contentType: "application/json",
			body:        `not json`,
		})

		assert.Equal(t, http.StatusBadRequest, response.Code)
	})

	t.Run("unknown fields are rejected", func(t *testing.T) {
		response := send(t, muxFor(t, deadStore(t)), request{
			user:        "golo",
			contentType: "application/json",
			// A misspelled field would otherwise turn into a zero value in silence.
			body: `{"id":"1","txt":"typo"}`,
		})

		assert.Equal(t, http.StatusBadRequest, response.Code)
		assert.JSONEq(t, `{"message": "malformed request: unknown field \"txt\""}`, response.Body.String(), "the answer should name the unknown field")
	})

	t.Run("ambiguous JSON is rejected", func(t *testing.T) {
		mux := muxFor(t, deadStore(t))

		for _, c := range []struct {
			label string
			body  string
		}{
			{"a second value", `{"id":"1","text":"a"} {"id":"1","text":"b"}`},
			{"garbage after the value", `{"id":"1","text":"a"} garbage`},
			{"a name that occurs twice", `{"id":"1","text":"a","text":"b"}`},
		} {
			t.Run(c.label, func(t *testing.T) {
				response := send(t, mux, request{
					user:        "golo",
					contentType: "application/json",
					body:        c.body,
				})

				assert.Equal(t, http.StatusBadRequest, response.Code)
			})
		}
	})

	t.Run("a command that cannot be built is rejected", func(t *testing.T) {
		response := send(t, muxFor(t, deadStore(t)), request{
			user:        "golo",
			contentType: "application/json",
			body:        `{"text":"no id"}`,
		})

		assert.Equal(t, http.StatusBadRequest, response.Code)
		assert.Contains(t, response.Body.String(), "id must not be empty")
	})

	t.Run("a body over the limit is rejected", func(t *testing.T) {
		padding := strings.Repeat("a", httpapi.MaxRequestBody)

		response := send(t, muxFor(t, deadStore(t)), request{
			user:        "golo",
			contentType: "application/json",
			body:        `{"id":"1","text":"` + padding + `"}`,
		})

		assert.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
	})

	t.Run("a body at the limit is read", func(t *testing.T) {
		// Exactly at the limit the body is still read, so this fails later, at the
		// dead store, rather than with 413.
		prefix := `{"id":"1","text":"`
		suffix := `"}`
		padding := strings.Repeat("a", httpapi.MaxRequestBody-len(prefix)-len(suffix))

		response := send(t, muxFor(t, deadStore(t)), request{
			user:        "golo",
			contentType: "application/json",
			body:        prefix + padding + suffix,
		})

		assert.NotEqual(t, http.StatusRequestEntityTooLarge, response.Code, "a body exactly at the limit must still be read")
	})

	t.Run("an unreadable body is rejected", func(t *testing.T) {
		httpRequest := httptest.NewRequest(http.MethodPost, "/note", failingReader{})
		httpRequest.Header.Set("X-User", "golo")
		httpRequest.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()

		muxFor(t, deadStore(t)).ServeHTTP(recorder, httpRequest)

		assert.Equal(t, http.StatusBadRequest, recorder.Code)
	})

	t.Run("an unreachable store is an internal failure", func(t *testing.T) {
		response := send(t, muxFor(t, deadStore(t)), request{
			user:        "golo",
			contentType: "application/json",
			body:        `{"id":"1","text":"hello"}`,
		})

		// Reading fails, which the kit reports as transient, so the caller is told
		// to try again later.
		assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	})

	t.Run("panics for a nil function", func(t *testing.T) {
		var toNothing httpapi.ToCommand[user, noteRequest, note]

		assert.PanicsWithValue(t, "architecturekit/httpapi: Route needs a function that turns the request into a command, not nil", func() {
			httpapi.Route(httpapi.NewAPI(deadStore(t), userFrom), http.NewServeMux(), "POST /note", toNothing, noteDecider())
		})
	})

	t.Run("panics for a nil API", func(t *testing.T) {
		// Without the panic, every request panics again while the panic is
		// answered, and net/http closes the connection without an answer.
		var noAPI *httpapi.API[user]

		assert.PanicsWithValue(t, "architecturekit/httpapi: Route needs the API, not nil", func() {
			httpapi.Route(noAPI, http.NewServeMux(), "POST /note", toNote, noteDecider())
		})
	})

	t.Run("panics for the zero Decider, which was not made with NewDecider", func(t *testing.T) {
		var decider architecturekit.Decider[note, notes]

		assert.PanicsWithValue(t, "architecturekit/httpapi: Route needs a decider made with NewDecider, not the zero Decider", func() {
			httpapi.Route(httpapi.NewAPI(deadStore(t), userFrom), http.NewServeMux(), "POST /note", toNote, decider)
		})
	})

	t.Run("panics for a pattern without a method, which accepts every method", func(t *testing.T) {
		// A space in front of the path leaves the method empty, as the mux
		// reads it.
		for _, pattern := range []string{"/note", "example.com/note", " /note"} {
			t.Run(pattern, func(t *testing.T) {
				assert.PanicsWithValue(t,
					fmt.Sprintf("architecturekit/httpapi: Route needs a pattern that names a method, such as POST, not %q, "+
						"which accepts every method, GET included", pattern),
					func() {
						httpapi.Route(httpapi.NewAPI(deadStore(t), userFrom), http.NewServeMux(), pattern, toNote, noteDecider())
					})
			})
		}
	})

	t.Run("panics for a pattern whose method must not change anything", func(t *testing.T) {
		for _, test := range []struct {
			pattern, method string
		}{
			{pattern: "GET /note", method: http.MethodGet},
			{pattern: "HEAD /note", method: http.MethodHead},
			{pattern: "OPTIONS /note", method: http.MethodOptions},
			// QUERY asks with a body and changes nothing, like GET, which is why
			// queries are asked with it (see Query).
			{pattern: "QUERY /note", method: "QUERY"},
			{pattern: "GET example.com/note", method: http.MethodGet},
			// The mux takes a tab for a space.
			{pattern: "GET\t/note", method: http.MethodGet},
		} {
			t.Run(test.pattern, func(t *testing.T) {
				assert.PanicsWithValue(t,
					fmt.Sprintf("architecturekit/httpapi: Route needs a pattern whose method may change something, such as POST, not %q, "+
						"since %s must not change anything", test.pattern, test.method),
					func() {
						httpapi.Route(httpapi.NewAPI(deadStore(t), userFrom), http.NewServeMux(), test.pattern, toNote, noteDecider())
					})
			})
		}
	})

	t.Run("executes a command on a pattern whose method may change something", func(t *testing.T) {
		for _, test := range []struct {
			pattern, method string
		}{
			{pattern: "POST /note", method: http.MethodPost},
			{pattern: "PUT /note", method: http.MethodPut},
			{pattern: "PATCH /note", method: http.MethodPatch},
			{pattern: "DELETE /note", method: http.MethodDelete},
			{pattern: "ARCHIVE /note", method: "ARCHIVE"},
			{pattern: "POST example.com/note", method: http.MethodPost},
			{pattern: "POST\t/note", method: http.MethodPost},
		} {
			t.Run(test.pattern, func(t *testing.T) {
				mux := http.NewServeMux()
				httpapi.Route(httpapi.NewAPI(writingStore(t), userFrom), mux, test.pattern, toNote, noteDecider())

				httpRequest := postingTo("/note", `{"id":"1","text":"hello"}`)
				httpRequest.Method = test.method
				response := serve(t, mux, httpRequest)

				assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
				assert.JSONEq(t, `{"revision": "0"}`, response.Body.String())
			})
		}
	})
}

// errBrokenBody is what failingReader fails with.
var errBrokenBody = errors.New("broken body")

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errBrokenBody }

// --- what the handler generated on the way ---

type textRequest struct {
	Text string `json:"text"`
}

// toGeneratedNote makes up the ID, the way an application does when the
// server, not the client, decides what a new aggregate is called.
func toGeneratedNote(_ *http.Request, request textRequest, u user) (note, error) {
	return note{ID: "generated-" + u.UserID, Text: request.Text}, nil
}

func TestHandle(t *testing.T) {
	t.Run("gives back the command it built", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)

		request := httptest.NewRequest(http.MethodPost, "/note", strings.NewReader(`{"text":"hello"}`))
		request.Header.Set("X-User", "golo")
		request.Header.Set("Content-Type", "application/json")

		handled, err := httpapi.Handle(request, api, toGeneratedNote, noteDecider())

		// The store is unreachable, so this fails, and the command still comes
		// back: a handler may want to say what it tried to do.
		require.Error(t, err, "expected the store to be unreachable")
		assert.Equal(t, "generated-golo", handled.Command.ID, "want the id the handler made up")
		// Without this, the id would have to be dug back out of the subject of a
		// written event, which does not exist when the write failed.
		assert.Equal(t, "/note/generated-golo", handled.Command.Subject())
	})

	t.Run("reports a failure before a command exists", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)

		request := httptest.NewRequest(http.MethodPost, "/note", strings.NewReader(`{"text":"x"}`))
		request.Header.Set("Content-Type", "application/json")

		handled, err := httpapi.Handle(request, api, toGeneratedNote, noteDecider())

		assert.ErrorIs(t, err, httpapi.ErrUnauthorized)
		assert.Empty(t, handled.Command.ID, "no command was built, so it has to be empty")
	})
}

// --- what the function that turns a request into a command gets ---

// routedShowing wires toCommand at the pattern, on a store that accepts every
// write, and answers with the command next to the revision, so that a test
// sees what the command was built from.
func routedShowing[TRequest any](t *testing.T, pattern string, toCommand httpapi.ToCommand[user, TRequest, note]) *http.ServeMux {
	t.Helper()

	mux := http.NewServeMux()
	httpapi.Route(httpapi.NewAPI(writingStore(t), userFrom), mux, pattern, toCommand, noteDecider(),
		httpapi.Adding(func(handled httpapi.Handled[note]) (any, error) {
			return struct {
				ID   string `json:"id"`
				Text string `json:"text"`
			}{handled.Command.ID, handled.Command.Text}, nil
		}))

	return mux
}

func TestToCommand(t *testing.T) {
	t.Run("gets a value of the path", func(t *testing.T) {
		mux := routedShowing(t, "POST /notes/{id}/text", func(r *http.Request, request textRequest, _ user) (note, error) {
			return note{ID: r.PathValue("id"), Text: request.Text}, nil
		})

		response := serve(t, mux, postingTo("/notes/42/text", `{"text":"hello"}`))

		assert.Equal(t, http.StatusOK, response.Code)
		assert.JSONEq(t, `{"revision": "0", "id": "42", "text": "hello"}`, response.Body.String())
	})

	t.Run("gets a header", func(t *testing.T) {
		mux := routedShowing(t, "POST /notes", func(r *http.Request, request textRequest, _ user) (note, error) {
			return note{ID: r.Header.Get("Idempotency-Key"), Text: request.Text}, nil
		})

		request := postingTo("/notes", `{"text":"hello"}`)
		request.Header.Set("Idempotency-Key", "7")
		response := serve(t, mux, request)

		assert.Equal(t, http.StatusOK, response.Code)
		assert.JSONEq(t, `{"revision": "0", "id": "7", "text": "hello"}`, response.Body.String())
	})

	t.Run("gets the context of the request", func(t *testing.T) {
		type tenantKey struct{}

		mux := routedShowing(t, "POST /notes", func(r *http.Request, request textRequest, _ user) (note, error) {
			tenant, _ := r.Context().Value(tenantKey{}).(string)
			return note{ID: tenant + "-1", Text: request.Text}, nil
		})

		// A middleware puts the tenant into the context, as an application does.
		withTenant := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tenantKey{}, "acme")))
		})

		response := httptest.NewRecorder()
		withTenant.ServeHTTP(response, postingTo("/notes", `{"text":"hello"}`))

		assert.Equal(t, http.StatusOK, response.Code)
		assert.JSONEq(t, `{"revision": "0", "id": "acme-1", "text": "hello"}`, response.Body.String())
	})

	t.Run("gets a body of a type from another package", func(t *testing.T) {
		// A method can only be declared in the package of its type, so a body of
		// another package could not be turned into a command by a method.
		mux := routedShowing(t, "POST /notes/{id}/due", func(r *http.Request, due time.Time, _ user) (note, error) {
			return note{ID: r.PathValue("id"), Text: "due on " + due.Format(time.DateOnly)}, nil
		})

		response := serve(t, mux, postingTo("/notes/42/due", `"2026-10-24T09:00:00Z"`))

		assert.Equal(t, http.StatusOK, response.Code)
		assert.JSONEq(t, `{"revision": "0", "id": "42", "text": "due on 2026-10-24"}`, response.Body.String())
	})

	t.Run("gets the request itself, the body, and the user", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)
		request := postingTo("/note", `{"text":"hello"}`)

		var (
			gotRequest *http.Request
			gotBody    textRequest
			gotUser    user
		)
		_, err := httpapi.Handle(request, api, func(r *http.Request, body textRequest, u user) (note, error) {
			gotRequest, gotBody, gotUser = r, body, u
			return note{ID: "1"}, nil
		}, noteDecider())

		require.Error(t, err, "expected the store to be unreachable")
		assert.Same(t, request, gotRequest, "the function has to get the request itself, with its context")
		assert.Equal(t, textRequest{Text: "hello"}, gotBody)
		assert.Equal(t, user{UserID: "golo"}, gotUser)
	})
}

func TestNewAPI(t *testing.T) {
	t.Run("panics on a nil function that determines the user, also one that was declared but never set", func(t *testing.T) {
		var declared func(*http.Request) (user, error)

		for _, userFrom := range []func(*http.Request) (user, error){nil, declared} {
			assert.PanicsWithValue(t,
				"architecturekit/httpapi: NewAPI needs a function that determines the user, not nil",
				func() { httpapi.NewAPI(deadStore(t), userFrom) })
		}
	})

	t.Run("panics on a nil function that determines the user without a store as well", func(t *testing.T) {
		assert.PanicsWithValue(t,
			"architecturekit/httpapi: NewAPI needs a function that determines the user, not nil",
			func() { httpapi.NewAPI[user](nil, nil) })
	})
}

func TestUserOf(t *testing.T) {
	t.Run("determines the caller for your own handlers", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)

		known := httptest.NewRequest(http.MethodGet, "/", nil)
		known.Header.Set("X-User", "golo")

		user, err := httpapi.UserOf(known, api)
		require.NoError(t, err)
		assert.Equal(t, "golo", user.UserID)

		// An unknown caller comes back as 401, exactly as on a wired route.
		_, err = httpapi.UserOf(httptest.NewRequest(http.MethodGet, "/", nil), api)

		assert.ErrorIs(t, err, httpapi.ErrUnauthorized)
		assert.Equal(t, http.StatusUnauthorized, httpapi.StatusFor(err))
	})

	for _, failure := range userFromFailures {
		t.Run("hands back an error of userFrom "+failure.label, func(t *testing.T) {
			api := httpapi.NewAPI(deadStore(t), userFromFailing(failure.err))

			_, err := httpapi.UserOf(httptest.NewRequest(http.MethodGet, "/", nil), api)

			assert.Equal(t, failure.status, httpapi.StatusFor(err))
			if failure.isKept {
				assert.Equal(t, failure.err, err, "the error has to come back as it is")
			} else {
				assert.ErrorIs(t, err, httpapi.ErrUnauthorized)
				assert.ErrorIs(t, err, failure.err, "the error of userFrom has to stay inspectable")
				assert.ErrorContains(t, err, failure.err.Error(), "the error has to say why")
			}
		})
	}

	t.Run("keeps an error of userFrom inspectable behind ErrUnauthorized", func(t *testing.T) {
		expired := &expiredToken{at: "2026-10-01T12:00:00Z"}
		api := httpapi.NewAPI(deadStore(t), userFromFailing(expired))

		_, err := httpapi.UserOf(httptest.NewRequest(http.MethodGet, "/", nil), api)

		require.ErrorIs(t, err, httpapi.ErrUnauthorized)

		found, isFound := errors.AsType[*expiredToken](err)
		require.True(t, isFound, "errors.As has to find the error of userFrom")
		assert.Same(t, expired, found)

		assert.Equal(t, "unauthorized: the token expired at 2026-10-01T12:00:00Z", err.Error())
	})
}

// expiredToken is an error of userFrom that carries more than its text.
type expiredToken struct {
	at string
}

func (e *expiredToken) Error() string { return "the token expired at " + e.at }

// userFromFailing returns a function that fails to determine the user, with
// the given error.
func userFromFailing(err error) func(*http.Request) (user, error) {
	return func(*http.Request) (user, error) { return user{}, err }
}

// userFromFailures are errors of userFrom, and the statuses they are answered
// with. An error that has a status of its own keeps it, and so does a
// permanent one, while every other error means that the caller is unknown.
var userFromFailures = []struct {
	label  string
	err    error
	status int
	isKept bool
}{
	{"without a category", errors.New("the token has expired"), http.StatusUnauthorized, false},
	{"that is wrapped as unauthorized", fmt.Errorf("%w: the token has expired", httpapi.ErrUnauthorized), http.StatusUnauthorized, true},
	{"that is transient", fmt.Errorf("%w: the session store is down", architecturekit.ErrTransient), http.StatusServiceUnavailable, true},
	{"that is transient, but wrapped as unauthorized", fmt.Errorf("%w: %v", httpapi.ErrUnauthorized, fmt.Errorf("%w: the session store is down", architecturekit.ErrTransient)), http.StatusUnauthorized, true},
	{"that is transient, but wrapped as unauthorized, inspectably", fmt.Errorf("%w: %w", httpapi.ErrUnauthorized, fmt.Errorf("%w: the session store is down", architecturekit.ErrTransient)), http.StatusUnauthorized, true},
	{"that is forbidden", fmt.Errorf("%w: the account is locked", httpapi.ErrForbidden), http.StatusForbidden, true},
	{"that is permanent", fmt.Errorf("%w: the session key is missing", architecturekit.ErrPermanent), http.StatusInternalServerError, true},
	{"that is unverified", fmt.Errorf("%w: the session is forged", architecturekit.ErrUnverified), http.StatusInternalServerError, true},
	{"of the domain", architecturekit.NewDomainError("the reader is suspended"), http.StatusUnprocessableEntity, true},
	{"that is not a revision", fmt.Errorf("%w: %q", architecturekit.ErrNotARevision, "abc"), http.StatusBadRequest, true},
	{"that is not a revision, but permanent", fmt.Errorf("%w: %w", architecturekit.ErrPermanent, fmt.Errorf("%w: %q", architecturekit.ErrNotARevision, "abc")), http.StatusInternalServerError, true},
	{"because the caller went away", fmt.Errorf("reading the session: %w", context.Canceled), 499, true},
	{"because the deadline ran out", fmt.Errorf("reading the session: %w", context.DeadlineExceeded), http.StatusServiceUnavailable, true},
}

func TestFailingUserFrom(t *testing.T) {
	// Every entry point determines the caller with UserOf, so each of them has
	// to answer with the status of the error.
	entryPoints := map[string]func(t *testing.T, api *httpapi.API[user]) *httptest.ResponseRecorder{
		"a command": func(t *testing.T, api *httpapi.API[user]) *httptest.ResponseRecorder {
			return postNote(t, routed(api), `{"id":"1","text":"hello"}`)
		},
		"a query": func(t *testing.T, api *httpapi.API[user]) *httptest.ResponseRecorder {
			mux := http.NewServeMux()
			httpapi.Query(api, mux, "QUERY /notes", toListNotes, answerListNotes)

			return ask(t, mux, "/notes", "golo")
		},
		"a revisioned query": func(t *testing.T, api *httpapi.API[user]) *httptest.ResponseRecorder {
			mux := http.NewServeMux()
			httpapi.Query(api, mux, "QUERY /notes", toListNotes, answerListNotes, httpapi.Revisioned(noteView(), time.Second))

			return ask(t, mux, "/notes", "golo")
		},
	}

	for name, enter := range entryPoints {
		for _, failure := range userFromFailures {
			t.Run(name+" answers an error of userFrom "+failure.label+" with "+strconv.Itoa(failure.status), func(t *testing.T) {
				var logs bytes.Buffer
				api := httpapi.NewAPI(deadStore(t), userFromFailing(failure.err), httpapi.WithLogger(loggerInto(&logs)))

				response := enter(t, api)

				assert.Equal(t, failure.status, response.Code)
			})
		}

		t.Run(name+" logs a transient error of userFrom as an internal failure", func(t *testing.T) {
			var logs bytes.Buffer
			api := httpapi.NewAPI(deadStore(t), userFromFailing(fmt.Errorf("%w: the session store is down", architecturekit.ErrTransient)), httpapi.WithLogger(loggerInto(&logs)))

			response := enter(t, api)

			require.Equal(t, http.StatusServiceUnavailable, response.Code)
			assert.JSONEq(t, `{"message": "internal server error"}`, response.Body.String())
			assert.Equal(t, 1, strings.Count(logs.String(), "httpapi: internal failure"))
			assert.Contains(t, logs.String(), "the session store is down")
		})
	}
}

// buildFailures are errors of ToCommand and ToQuery, and the statuses and
// messages they are answered with. An error that has a status of its own
// keeps it, and so does a permanent one, while every other error means that
// the request is malformed.
var buildFailures = []struct {
	label   string
	err     error
	status  int
	message string
	isKept  bool
}{
	{"without a category", errors.New("id must not be empty"), http.StatusBadRequest, "malformed request: id must not be empty", false},
	{"that is malformed", fmt.Errorf("%w: id must not be empty", httpapi.ErrMalformed), http.StatusBadRequest, "malformed request: id must not be empty", true},
	{"that is unauthorized", fmt.Errorf("%w: the token has expired", httpapi.ErrUnauthorized), http.StatusUnauthorized, "unauthorized", true},
	{"that is forbidden", fmt.Errorf("%w: only librarians acquire books", httpapi.ErrForbidden), http.StatusForbidden, "forbidden: only librarians acquire books", true},
	{"that is not found", fmt.Errorf("%w: book 42 is unknown", httpapi.ErrNotFound), http.StatusNotFound, "not found: book 42 is unknown", true},
	{"that found no item", fmt.Errorf("finding book 42: %w", query.ErrNoItems), http.StatusNotFound, "not found", true},
	{"that is not found, and found no item", fmt.Errorf("%w: book 42 is unknown: %w", httpapi.ErrNotFound, query.ErrNoItems), http.StatusNotFound, "not found: book 42 is unknown: no items", true},
	{"that is too large", fmt.Errorf("%w: at most 10 books at once", httpapi.ErrTooLarge), http.StatusRequestEntityTooLarge, "request body too large: at most 10 books at once", true},
	{"that is no JSON", fmt.Errorf("%w: text/plain is not application/json", httpapi.ErrUnsupportedMediaType), http.StatusUnsupportedMediaType, "unsupported media type: text/plain is not application/json", true},
	{"of the domain", architecturekit.NewDomainError("the reader is suspended"), http.StatusUnprocessableEntity, "the reader is suspended", true},
	{"that is not a revision", fmt.Errorf("%w: %q", architecturekit.ErrNotARevision, "abc"), http.StatusBadRequest, `not a revision: "abc"`, true},
	{"that is not a revision, but permanent", fmt.Errorf("%w: %w", architecturekit.ErrPermanent, fmt.Errorf("%w: %q", architecturekit.ErrNotARevision, "abc")), http.StatusInternalServerError, "internal server error", true},
	{"that is a conflict", fmt.Errorf("%w: reading %q", architecturekit.ErrConflict, "/readers/23"), http.StatusConflict, "conflict: the data has changed since it was read", true},
	{"that is transient", fmt.Errorf("%w: session store at redis://10.0.3.9 is down", architecturekit.ErrTransient), http.StatusServiceUnavailable, "internal server error", true},
	{"that is permanent", fmt.Errorf("%w: the catalog at /etc/catalog.yaml is missing", architecturekit.ErrPermanent), http.StatusInternalServerError, "internal server error", true},
	{"that is unverified", fmt.Errorf("%w: the reader is forged", architecturekit.ErrUnverified), http.StatusInternalServerError, "internal server error", true},
	{"because the caller went away", fmt.Errorf("looking up the reader: %w", context.Canceled), 499, "request canceled", true},
	{"because the deadline ran out", fmt.Errorf("looking up the reader: %w", context.DeadlineExceeded), http.StatusServiceUnavailable, "internal server error", true},
}

// failingToCommand returns a function that fails to build a command, with the
// given error.
func failingToCommand(err error) httpapi.ToCommand[user, noteRequest, note] {
	return func(*http.Request, noteRequest, user) (note, error) { return note{}, err }
}

// failingToQuery returns a function that fails to build a query, with the
// given error.
func failingToQuery(err error) httpapi.ToQuery[user, httpapi.NoBody, listNotes] {
	return func(*http.Request, httpapi.NoBody, user) (listNotes, error) { return listNotes{}, err }
}

func TestFailingToCommandAndToQuery(t *testing.T) {
	// Every entry point builds its command or query and categorises its error
	// the same way, so each of them has to answer with the status of the
	// error.
	entryPoints := map[string]func(t *testing.T, api *httpapi.API[user], err error) *httptest.ResponseRecorder{
		"a command": func(t *testing.T, api *httpapi.API[user], err error) *httptest.ResponseRecorder {
			mux := http.NewServeMux()
			httpapi.Route(api, mux, "POST /note", failingToCommand(err), noteDecider())

			return postNote(t, mux, `{"id":"1","text":"hello"}`)
		},
		"a query": func(t *testing.T, api *httpapi.API[user], err error) *httptest.ResponseRecorder {
			mux := http.NewServeMux()
			httpapi.Query(api, mux, "QUERY /notes", failingToQuery(err), answerListNotes)

			return ask(t, mux, "/notes", "golo")
		},
		"a revisioned query": func(t *testing.T, api *httpapi.API[user], err error) *httptest.ResponseRecorder {
			mux := http.NewServeMux()
			httpapi.Query(api, mux, "QUERY /notes", failingToQuery(err), answerListNotes, httpapi.Revisioned(noteView(), time.Second))

			return ask(t, mux, "/notes", "golo")
		},
	}

	for name, enter := range entryPoints {
		for _, failure := range buildFailures {
			t.Run(name+" answers an error "+failure.label+" with "+strconv.Itoa(failure.status), func(t *testing.T) {
				var logs bytes.Buffer
				api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))

				response := enter(t, api, failure.err)

				assert.Equal(t, failure.status, response.Code)

				expected, err := json.Marshal(map[string]string{"message": failure.message})
				require.NoError(t, err)
				assert.JSONEq(t, string(expected), response.Body.String())
			})
		}

		t.Run(name+" logs a transient error as an internal failure, without telling the caller", func(t *testing.T) {
			var logs bytes.Buffer
			api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
			transient := fmt.Errorf("%w: session store at redis://10.0.3.9 is down", architecturekit.ErrTransient)

			response := enter(t, api, transient)

			require.Equal(t, http.StatusServiceUnavailable, response.Code)
			assert.NotContains(t, response.Body.String(), "redis://", "the caller must not learn about the internals")
			assert.Equal(t, 1, strings.Count(logs.String(), "httpapi: internal failure"))
			assert.Contains(t, logs.String(), "redis://10.0.3.9 is down")
		})
	}

	// Handle and Ask return the error, so it has to be the same that the routes
	// answer with, and stay inspectable when it is marked as malformed.
	returners := map[string]func(t *testing.T, api *httpapi.API[user], err error) error{
		"Handle": func(t *testing.T, api *httpapi.API[user], err error) error {
			request := httptest.NewRequest(http.MethodPost, "/note", strings.NewReader(`{"id":"1","text":"hello"}`))
			request.Header.Set("X-User", "golo")
			request.Header.Set("Content-Type", "application/json")

			_, err = httpapi.Handle(request, api, failingToCommand(err), noteDecider())

			return err
		},
		"Ask": func(t *testing.T, api *httpapi.API[user], err error) error {
			request := httptest.NewRequest(http.MethodGet, "/notes", nil)
			request.Header.Set("X-User", "golo")

			_, err = httpapi.Ask(request, api, failingToQuery(err), answerListNotes)

			return err
		},
	}

	for name, returnError := range returners {
		for _, failure := range buildFailures {
			t.Run(name+" returns an error "+failure.label+" with the status "+strconv.Itoa(failure.status), func(t *testing.T) {
				api := httpapi.NewAPI(deadStore(t), userFrom)

				err := returnError(t, api, failure.err)

				assert.Equal(t, failure.status, httpapi.StatusFor(err))
				if failure.isKept {
					assert.Equal(t, failure.err, err, "the error has to come back as it is")
				} else {
					assert.ErrorIs(t, err, httpapi.ErrMalformed)
					assert.ErrorIs(t, err, failure.err, "the error has to stay inspectable")
				}
			})
		}
	}
}
