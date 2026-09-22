package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/httpapi"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// --- a domain just large enough to drive the HTTP layer ---

type noted struct {
	Text string `json:"text"`
}

func (noted) EventType() string { return "io.thenativeweb.httpapi.noted" }

type notes struct {
	Count int
}

type note struct {
	ID   string
	Text string
}

func (c note) Subject() string { return "/note/" + c.ID }

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
	if err != nil {
		t.Fatal(err)
	}
	client, err := eventsourcingdb.NewClient(deadURL, "secret")
	if err != nil {
		t.Fatal(err)
	}

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

func TestStatusForMapsEveryCategory(t *testing.T) {
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
		{"anything else", errors.New("who knows"), http.StatusInternalServerError},
	}

	for _, c := range cases {
		if got := httpapi.StatusFor(c.err); got != c.want {
			t.Fatalf("%s: got %d, want %d", c.label, got, c.want)
		}
	}
}

func TestStatusForPrefersConflictOverItsCategory(t *testing.T) {
	// A conflict is transient, so the order of the cases decides. 409 says
	// more than 503, hence it has to win.
	if !errors.Is(architecturekit.ErrConflict, architecturekit.ErrTransient) {
		t.Fatal("a conflict is expected to be transient")
	}
	if got := httpapi.StatusFor(architecturekit.ErrConflict); got != http.StatusConflict {
		t.Fatalf("got %d, want 409", got)
	}
}

func TestRespondReportsEventIDsOnSuccess(t *testing.T) {
	recorder := httptest.NewRecorder()

	httpapi.Respond(recorder, []eventsourcingdb.Event{{ID: "0"}, {ID: "1"}}, nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d", recorder.Code)
	}
	if recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("got %q", recorder.Header().Get("Content-Type"))
	}

	var body struct {
		Message  string   `json:"message"`
		EventIDs []string `json:"eventIds"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Message != "ok" {
		t.Fatalf("got %q", body.Message)
	}
	if len(body.EventIDs) != 2 || body.EventIDs[0] != "0" || body.EventIDs[1] != "1" {
		t.Fatalf("got %v", body.EventIDs)
	}
}

func TestRespondKeepsInternalFailuresToItself(t *testing.T) {
	recorder := httptest.NewRecorder()

	httpapi.Respond(recorder, nil, errors.New("the password is hunter2"))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("got %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "hunter2") {
		t.Fatalf("an internal failure must not be explained: %s", recorder.Body)
	}
}

func TestRespondExplainsFailuresTheCallerCanFix(t *testing.T) {
	recorder := httptest.NewRecorder()

	httpapi.Respond(recorder, nil, architecturekit.NewDomainError("note 7 already exists"))

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "note 7 already exists") {
		t.Fatalf("got %s", recorder.Body)
	}
}

// --- everything that fails before the store is touched ---

func TestRequestWithoutAUserIsUnauthorized(t *testing.T) {
	response := send(t, muxFor(t, deadStore(t)), request{
		contentType: "application/json",
		body:        `{"id":"1"}`,
	})

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("got %d: %s", response.Code, response.Body)
	}
}

func TestContentTypeIsRequired(t *testing.T) {
	mux := muxFor(t, deadStore(t))

	for _, contentType := range []string{
		"",                // missing
		"text/plain",      // the CSRF-friendly one
		"application/xml", // simply wrong
		"application/x-www-form-urlencoded",
		"text/plain; application/json", // would pass a substring test
		"not a media type at all;;;",
	} {
		response := send(t, mux, request{
			user:        "golo",
			contentType: contentType,
			body:        `{"id":"1"}`,
		})

		if response.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("%q: got %d, want 415: %s", contentType, response.Code, response.Body)
		}
	}
}

func TestContentTypeWithParametersIsAccepted(t *testing.T) {
	// The media type is parsed, so a charset does not get in the way. This one
	// reaches the store and fails there, which is enough to show it passed the
	// media type check.
	response := send(t, muxFor(t, deadStore(t)), request{
		user:        "golo",
		contentType: "application/json; charset=utf-8",
		body:        `{"id":"1"}`,
	})

	if response.Code == http.StatusUnsupportedMediaType {
		t.Fatalf("a charset must not be rejected: %s", response.Body)
	}
}

func TestMalformedJSONIsRejected(t *testing.T) {
	response := send(t, muxFor(t, deadStore(t)), request{
		user:        "golo",
		contentType: "application/json",
		body:        `not json`,
	})

	if response.Code != http.StatusBadRequest {
		t.Fatalf("got %d: %s", response.Code, response.Body)
	}
}

func TestUnknownFieldsAreRejected(t *testing.T) {
	response := send(t, muxFor(t, deadStore(t)), request{
		user:        "golo",
		contentType: "application/json",
		// A misspelled field would otherwise turn into a zero value in silence.
		body: `{"id":"1","txt":"typo"}`,
	})

	if response.Code != http.StatusBadRequest {
		t.Fatalf("got %d: %s", response.Code, response.Body)
	}
	if !strings.Contains(response.Body.String(), "txt") {
		t.Fatalf("the answer should name the unknown field: %s", response.Body)
	}
}

func TestCommandThatCannotBeBuiltIsRejected(t *testing.T) {
	response := send(t, muxFor(t, deadStore(t)), request{
		user:        "golo",
		contentType: "application/json",
		body:        `{"text":"no id"}`,
	})

	if response.Code != http.StatusBadRequest {
		t.Fatalf("got %d: %s", response.Code, response.Body)
	}
	if !strings.Contains(response.Body.String(), "id must not be empty") {
		t.Fatalf("got %s", response.Body)
	}
}

func TestBodyOverTheLimitIsRejected(t *testing.T) {
	padding := strings.Repeat("a", httpapi.MaxRequestBody)

	response := send(t, muxFor(t, deadStore(t)), request{
		user:        "golo",
		contentType: "application/json",
		body:        `{"id":"1","text":"` + padding + `"}`,
	})

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413: %s", response.Code, response.Body)
	}
}

func TestBodyAtTheLimitIsRead(t *testing.T) {
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

	if response.Code == http.StatusRequestEntityTooLarge {
		t.Fatal("a body exactly at the limit must still be read")
	}
}

func TestUnreadableBodyIsRejected(t *testing.T) {
	httpRequest := httptest.NewRequest(http.MethodPost, "/note", failingReader{})
	httpRequest.Header.Set("X-User", "golo")
	httpRequest.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	muxFor(t, deadStore(t)).ServeHTTP(recorder, httpRequest)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("got %d: %s", recorder.Code, recorder.Body)
	}
}

func TestUnreachableStoreIsAnInternalFailure(t *testing.T) {
	response := send(t, muxFor(t, deadStore(t)), request{
		user:        "golo",
		contentType: "application/json",
		body:        `{"id":"1","text":"hello"}`,
	})

	// Reading fails, which the kit reports as transient, so the caller is told
	// to try again later.
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503: %s", response.Code, response.Body)
	}
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

func TestHandleGivesBackTheCommandItBuilt(t *testing.T) {
	api := httpapi.NewAPI(deadStore(t), userFrom)

	request := httptest.NewRequest(http.MethodPost, "/note", strings.NewReader(`{"text":"hello"}`))
	request.Header.Set("X-User", "golo")
	request.Header.Set("Content-Type", "application/json")

	handled, err := httpapi.Handle[generatedRequest](request, api, noteDecider())

	// The store is unreachable, so this fails, and the command still comes
	// back: a handler may want to say what it tried to do.
	if err == nil {
		t.Fatal("expected the store to be unreachable")
	}
	if handled.Command.ID != "generated-golo" {
		t.Fatalf("got %q, want the id the handler made up", handled.Command.ID)
	}
	// Without this, the id would have to be dug back out of the subject of a
	// written event, which does not exist when the write failed.
	if handled.Command.Subject() != "/note/generated-golo" {
		t.Fatalf("got %q", handled.Command.Subject())
	}
}

func TestHandleReportsAFailureBeforeACommandExists(t *testing.T) {
	api := httpapi.NewAPI(deadStore(t), userFrom)

	request := httptest.NewRequest(http.MethodPost, "/note", strings.NewReader(`{"text":"x"}`))
	request.Header.Set("Content-Type", "application/json")

	handled, err := httpapi.Handle[generatedRequest](request, api, noteDecider())

	if !errors.Is(err, httpapi.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
	if handled.Command.ID != "" {
		t.Fatalf("no command was built, so it has to be empty: %+v", handled.Command)
	}
}

func TestUserOfDeterminesTheCallerForYourOwnHandlers(t *testing.T) {
	api := httpapi.NewAPI(deadStore(t), userFrom)

	known := httptest.NewRequest(http.MethodGet, "/", nil)
	known.Header.Set("X-User", "golo")

	user, err := httpapi.UserOf(known, api)
	if err != nil {
		t.Fatalf("failed to determine the user: %v", err)
	}
	if user.UserID != "golo" {
		t.Errorf("got %q, want %q", user.UserID, "golo")
	}

	// An unknown caller comes back as 401, exactly as on a wired route.
	_, err = httpapi.UserOf(httptest.NewRequest(http.MethodGet, "/", nil), api)

	if !errors.Is(err, httpapi.ErrUnauthorized) {
		t.Errorf("got %v, want %v", err, httpapi.ErrUnauthorized)
	}
	if got := httpapi.StatusFor(err); got != http.StatusUnauthorized {
		t.Errorf("got status %d, want %d", got, http.StatusUnauthorized)
	}
}
