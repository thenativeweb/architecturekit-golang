package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/httpapi"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/query"
)

// The read side touches no store, so none of these tests needs a database.

type listNotes struct {
	Limit int
}

type noteResponse struct {
	Text string `json:"text"`
}

func toListNotes(r *http.Request, _ user) (listNotes, error) {
	ask := listNotes{}

	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			return listNotes{}, errors.New("limit must be a number")
		}
		ask.Limit = limit
	}

	return ask, nil
}

func answerListNotes(ctx context.Context, ask listNotes) ([]noteResponse, error) {
	notes := []noteResponse{{Text: "first"}, {Text: "second"}, {Text: "third"}}

	if ask.Limit > 0 && ask.Limit < len(notes) {
		notes = notes[:ask.Limit]
	}

	return notes, nil
}

func queryMux(t *testing.T) *http.ServeMux {
	t.Helper()

	api := httpapi.NewAPI(deadStore(t), userFrom)
	mux := http.NewServeMux()
	httpapi.Query(api, mux, "GET /notes", toListNotes, answerListNotes)

	return mux
}

func ask(t *testing.T, mux *http.ServeMux, path, asUser string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, path, nil)
	if asUser != "" {
		request.Header.Set("X-User", asUser)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	return recorder
}

func TestQueryAnswersWithTheResult(t *testing.T) {
	response := ask(t, queryMux(t), "/notes", "golo")

	if response.Code != http.StatusOK {
		t.Fatalf("got %d: %s", response.Code, response.Body)
	}
	if response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("got %q", response.Header().Get("Content-Type"))
	}

	var notes []noteResponse
	if err := json.Unmarshal(response.Body.Bytes(), &notes); err != nil {
		t.Fatalf("failed to decode %s: %v", response.Body, err)
	}
	if len(notes) != 3 {
		t.Fatalf("got %d notes", len(notes))
	}
}

func TestQueryPassesRequestParametersThrough(t *testing.T) {
	response := ask(t, queryMux(t), "/notes?limit=2", "golo")

	var notes []noteResponse
	if err := json.Unmarshal(response.Body.Bytes(), &notes); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("got %d notes, want 2", len(notes))
	}
}

func TestQueryNeedsAUser(t *testing.T) {
	response := ask(t, queryMux(t), "/notes", "")

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401: %s", response.Code, response.Body)
	}
}

func TestQueryRejectsUnusableParameters(t *testing.T) {
	response := ask(t, queryMux(t), "/notes?limit=banana", "golo")

	if response.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", response.Code, response.Body)
	}
	if !strings.Contains(response.Body.String(), "limit must be a number") {
		t.Fatalf("got %s", response.Body)
	}
}

func TestAskReturnsTheResultWithoutWriting(t *testing.T) {
	api := httpapi.NewAPI(deadStore(t), userFrom)

	request := httptest.NewRequest(http.MethodGet, "/notes", nil)
	request.Header.Set("X-User", "golo")

	notes, err := httpapi.Ask(request, api, toListNotes, answerListNotes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(notes) != 3 {
		t.Fatalf("got %d", len(notes))
	}
}

func TestAskReportsAFailingAnswer(t *testing.T) {
	api := httpapi.NewAPI(deadStore(t), userFrom)

	request := httptest.NewRequest(http.MethodGet, "/notes", nil)
	request.Header.Set("X-User", "golo")

	_, err := httpapi.Ask(request, api, toListNotes,
		func(context.Context, listNotes) ([]noteResponse, error) {
			return nil, errors.New("the view is unavailable")
		})

	if err == nil {
		t.Fatal("expected the error from the answer")
	}
}

func TestQueryWithoutItemsIsNotFound(t *testing.T) {
	api := httpapi.NewAPI(deadStore(t), userFrom)
	mux := http.NewServeMux()

	// A single-item lookup that finds nothing reports query.ErrNoItems, which
	// the HTTP layer turns into 404 without the application saying so.
	httpapi.Query(api, mux, "GET /notes/{id}", toListNotes,
		func(context.Context, listNotes) (noteResponse, error) {
			return noteResponse{}, query.ErrNoItems
		})

	response := ask(t, mux, "/notes/7", "golo")

	if response.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404: %s", response.Code, response.Body)
	}
}

func TestPublicAPIServesEveryone(t *testing.T) {
	api := httpapi.NewPublicAPI(deadStore(t))
	mux := http.NewServeMux()

	httpapi.Query(api, mux, "GET /public",
		func(r *http.Request, _ httpapi.NoUser) (listNotes, error) {
			return listNotes{}, nil
		},
		answerListNotes)

	// No user header at all, and it is served anyway.
	response := ask(t, mux, "/public", "")

	if response.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", response.Code, response.Body)
	}
}

func TestRespondResultWritesTheResult(t *testing.T) {
	recorder := httptest.NewRecorder()

	httpapi.RespondResult(recorder, []noteResponse{{Text: "only"}}, nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "only") {
		t.Fatalf("got %s", recorder.Body)
	}
}

func TestRespondResultExplainsFailuresTheCallerCanFix(t *testing.T) {
	recorder := httptest.NewRecorder()

	httpapi.RespondResult(recorder, []noteResponse(nil),
		errors.Join(httpapi.ErrNotFound, errors.New("note 7 is unknown")))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "note 7 is unknown") {
		t.Fatalf("got %s", recorder.Body)
	}
}

func TestRespondResultKeepsInternalFailuresToItself(t *testing.T) {
	recorder := httptest.NewRecorder()

	httpapi.RespondResult(recorder, []noteResponse(nil), errors.New("the password is hunter2"))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("got %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "hunter2") {
		t.Fatalf("an internal failure must not be explained: %s", recorder.Body)
	}
}

func TestForbiddenSurvivesTheWayOut(t *testing.T) {
	// An application that refuses in ToQuery keeps its 403 instead of having
	// it turned into a 400.
	api := httpapi.NewAPI(deadStore(t), userFrom)
	mux := http.NewServeMux()

	httpapi.Query(api, mux, "GET /restricted",
		func(*http.Request, user) (listNotes, error) {
			return listNotes{}, errors.Join(httpapi.ErrForbidden, errors.New("not for you"))
		},
		answerListNotes)

	response := ask(t, mux, "/restricted", "golo")

	if response.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", response.Code, response.Body)
	}
}

func TestDomainErrorsFromTheQuerySideKeepTheirStatus(t *testing.T) {
	api := httpapi.NewAPI(deadStore(t), userFrom)
	mux := http.NewServeMux()

	httpapi.Query(api, mux, "GET /rule",
		func(*http.Request, user) (listNotes, error) {
			return listNotes{}, architecturekit.NewDomainError("that combination makes no sense")
		},
		answerListNotes)

	response := ask(t, mux, "/rule", "golo")

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422: %s", response.Code, response.Body)
	}
}
