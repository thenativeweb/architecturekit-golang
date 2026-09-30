package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/httpapi"
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

	return architecturekit.Decider[note, notes]{
		State: state,
		Decide: func(ctx context.Context, cmd note, current notes) ([]architecturekit.Event, error) {
			if current.Count > 0 {
				return nil, architecturekit.NewDomainError("note %s already exists", cmd.ID)
			}
			return []architecturekit.Event{noted{Text: cmd.Text}}, nil
		},
	}
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

func (r noteRequest) ToCommand(u user) (note, error) {
	if r.ID == "" {
		return note{}, errors.New("id must not be empty")
	}
	return note(r), nil
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
	httpapi.Route[noteRequest](api, mux, "POST /note", noteDecider())

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
}

func TestRespond(t *testing.T) {
	t.Run("reports the revision on success", func(t *testing.T) {
		recorder := httptest.NewRecorder()

		httpapi.Respond(recorder, []eventsourcingdb.Event{{ID: "0"}, {ID: "1"}}, nil)

		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.Equal(t, "application/json", recorder.Header().Get("Content-Type"))
		assert.JSONEq(t, `{"revision": "1"}`, recorder.Body.String(), "want the ID of the last event, and nothing else")
	})

	t.Run("reports an empty revision if nothing was written", func(t *testing.T) {
		recorder := httptest.NewRecorder()

		httpapi.Respond(recorder, nil, nil)

		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.JSONEq(t, `{"revision": ""}`, recorder.Body.String())
	})

	t.Run("keeps internal failures to itself", func(t *testing.T) {
		recorder := httptest.NewRecorder()

		httpapi.Respond(recorder, nil, errors.New("the password is hunter2"))

		assert.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.NotContains(t, recorder.Body.String(), "hunter2", "an internal failure must not be explained")
	})

	t.Run("logs internal failures", func(t *testing.T) {
		logs := logsOf(func() {
			httpapi.Respond(httptest.NewRecorder(), nil, errors.New("the database is gone"))
		})

		assert.Contains(t, logs, "the database is gone", "an internal failure must be logged")
	})

	t.Run("does not log failures the caller can fix", func(t *testing.T) {
		logs := logsOf(func() {
			httpapi.Respond(httptest.NewRecorder(), nil, architecturekit.NewDomainError("note 7 already exists"))
		})

		assert.Empty(t, logs, "a failure the caller can fix must not be logged")
	})

	t.Run("explains failures the caller can fix", func(t *testing.T) {
		recorder := httptest.NewRecorder()

		httpapi.Respond(recorder, nil, architecturekit.NewDomainError("note 7 already exists"))

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
		assert.Contains(t, response.Body.String(), "txt", "the answer should name the unknown field")
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
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("broken body") }

// --- what the handler generated on the way ---

type generatedRequest struct {
	Text string `json:"text"`
}

// ToCommand makes up the ID, the way an application does when the server, not
// the client, decides what a new aggregate is called.
func (r generatedRequest) ToCommand(u user) (note, error) {
	return note{ID: "generated-" + u.UserID, Text: r.Text}, nil
}

func TestHandle(t *testing.T) {
	t.Run("gives back the command it built", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)

		request := httptest.NewRequest(http.MethodPost, "/note", strings.NewReader(`{"text":"hello"}`))
		request.Header.Set("X-User", "golo")
		request.Header.Set("Content-Type", "application/json")

		handled, err := httpapi.Handle[generatedRequest](request, api, noteDecider())

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

		handled, err := httpapi.Handle[generatedRequest](request, api, noteDecider())

		assert.ErrorIs(t, err, httpapi.ErrUnauthorized)
		assert.Empty(t, handled.Command.ID, "no command was built, so it has to be empty")
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
}
