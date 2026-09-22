package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/httpapi"
)

// The read side used below: a view of notes, and a query that counts them.

type noteItem struct {
	Text string
}

type countNotes struct{}

func countNotesIn(view *architecturekit.ItemView[noteItem]) httpapi.Answer[countNotes, int] {
	return func(ctx context.Context, _ countNotes) (int, error) {
		items, err := view.All(ctx)
		if err != nil {
			return 0, err
		}

		count := 0
		for range items {
			count++
		}

		return count, nil
	}
}

func allNotes(*http.Request, user) (countNotes, error) { return countNotes{}, nil }

// servingNotes wires one revisioned query onto a mux.
func servingNotes(t *testing.T, view *architecturekit.ItemView[noteItem], wait time.Duration) *http.ServeMux {
	t.Helper()

	mux := http.NewServeMux()
	api := httpapi.NewAPI(deadStore(t), userFrom)

	httpapi.QueryRevisioned(api, mux, "GET /notes", view, allNotes, countNotesIn(view), wait)

	return mux
}

// askNotes sends a request as a known user, with optional revision headers.
func askNotes(mux *http.ServeMux, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/notes", nil)
	request.Header.Set("X-User", "someone")

	for name, value := range headers {
		request.Header.Set(name, value)
	}

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	return recorder
}

func TestAQueryWithoutAWantedRevisionAnswersAtOnce(t *testing.T) {
	view := architecturekit.NewItemView[noteItem]()
	view.Insert(noteItem{Text: "one"})
	view.Seen("3")

	response := askNotes(servingNotes(t, view, time.Second), nil)

	if response.Code != http.StatusOK {
		t.Fatalf("got %d: %s", response.Code, response.Body)
	}

	if got := response.Header().Get(httpapi.HeaderRevision); got != "3" {
		t.Errorf("got revision %q, want %q", got, "3")
	}

	if got := response.Header().Get("ETag"); got == "" {
		t.Error("no ETag")
	}
}

func TestAQueryWaitsForTheRevisionItWasAskedFor(t *testing.T) {
	view := architecturekit.NewItemView[noteItem]()
	view.Seen("1")

	// The revision arrives only after the request is already waiting.
	go func() {
		time.Sleep(100 * time.Millisecond)
		view.Insert(noteItem{Text: "late"})
		view.Seen("5")
	}()

	started := time.Now()
	response := askNotes(servingNotes(t, view, 10*time.Second), map[string]string{
		httpapi.HeaderWaitFor: "5",
	})

	if response.Code != http.StatusOK {
		t.Fatalf("got %d: %s", response.Code, response.Body)
	}

	if took := time.Since(started); took < 100*time.Millisecond {
		t.Errorf("answered after %v, so it cannot have waited", took)
	}

	if got := response.Header().Get(httpapi.HeaderRevision); got != "5" {
		t.Errorf("got revision %q, want %q", got, "5")
	}

	// The whole point: the item written with that revision is in the answer.
	if got := response.Body.String(); got != "1\n" {
		t.Errorf("got %q, want the late item to be counted", got)
	}
}

func TestAQueryAnswersWithWhatItHasWhenTheWaitRunsOut(t *testing.T) {
	view := architecturekit.NewItemView[noteItem]()
	view.Insert(noteItem{Text: "one"})
	view.Seen("2")

	response := askNotes(servingNotes(t, view, 50*time.Millisecond), map[string]string{
		httpapi.HeaderWaitFor: "99",
	})

	// Running out of time is not an error: the caller gets the data and is
	// told which revision it is looking at.
	if response.Code != http.StatusOK {
		t.Fatalf("got %d: %s", response.Code, response.Body)
	}

	if got := response.Header().Get(httpapi.HeaderRevision); got != "2" {
		t.Errorf("got revision %q, want %q", got, "2")
	}
}

func TestAWantedRevisionThatIsNotOneIsRefused(t *testing.T) {
	view := architecturekit.NewItemView[noteItem]()
	view.Seen("1")

	response := askNotes(servingNotes(t, view, 10*time.Second), map[string]string{
		httpapi.HeaderWaitFor: "soon",
	})

	if response.Code != http.StatusBadRequest {
		t.Errorf("got %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestAKnownRevisionIsAnsweredWithNotModified(t *testing.T) {
	view := architecturekit.NewItemView[noteItem]()
	view.Insert(noteItem{Text: "one"})
	view.Seen("4")

	mux := servingNotes(t, view, time.Second)

	first := askNotes(mux, nil)
	tag := first.Header().Get("ETag")

	second := askNotes(mux, map[string]string{"If-None-Match": tag})

	if second.Code != http.StatusNotModified {
		t.Fatalf("got %d, want %d", second.Code, http.StatusNotModified)
	}

	if second.Body.Len() != 0 {
		t.Errorf("304 carried a body: %q", second.Body.String())
	}

	// After something changes, the same tag no longer matches.
	view.Insert(noteItem{Text: "two"})
	view.Seen("5")

	third := askNotes(mux, map[string]string{"If-None-Match": tag})

	if third.Code != http.StatusOK {
		t.Errorf("got %d, want %d", third.Code, http.StatusOK)
	}
}

func TestOneResourcesTagDoesNotMatchAnother(t *testing.T) {
	// Every query over the same view shares a revision, so a tag that held
	// nothing else would wrongly match across resources.
	view := architecturekit.NewItemView[noteItem]()
	view.Seen("4")

	mux := http.NewServeMux()
	api := httpapi.NewAPI(deadStore(t), userFrom)

	httpapi.QueryRevisioned(api, mux, "GET /notes", view, allNotes, countNotesIn(view), time.Second)
	httpapi.QueryRevisioned(api, mux, "GET /other", view, allNotes, countNotesIn(view), time.Second)

	tag := askNotes(mux, nil).Header().Get("ETag")

	request := httptest.NewRequest(http.MethodGet, "/other", nil)
	request.Header.Set("X-User", "someone")
	request.Header.Set("If-None-Match", tag)

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Errorf("got %d, want %d: the tag of /notes matched /other", response.Code, http.StatusOK)
	}
}

func TestAViewThatHasSeenNothingCarriesNoTag(t *testing.T) {
	view := architecturekit.NewItemView[noteItem]()

	response := askNotes(servingNotes(t, view, time.Second), nil)

	if response.Code != http.StatusOK {
		t.Fatalf("got %d", response.Code)
	}

	if got := response.Header().Get("ETag"); got != "" {
		t.Errorf("got ETag %q, want none", got)
	}
	if got := response.Header().Get(httpapi.HeaderRevision); got != "" {
		t.Errorf("got revision %q, want none", got)
	}
}

func TestNobodyCanMakeTheServerWaitWithoutBeingLetIn(t *testing.T) {
	view := architecturekit.NewItemView[noteItem]()
	view.Seen("1")

	// No X-User header, so the request never gets as far as waiting.
	request := httptest.NewRequest(http.MethodGet, "/notes", nil)
	request.Header.Set(httpapi.HeaderWaitFor, "99")

	started := time.Now()
	response := httptest.NewRecorder()
	servingNotes(t, view, 10*time.Second).ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want %d", response.Code, http.StatusUnauthorized)
	}

	if took := time.Since(started); took > time.Second {
		t.Errorf("waited %v before refusing", took)
	}
}

func TestAFailingQueryCarriesNoRevision(t *testing.T) {
	view := architecturekit.NewItemView[noteItem]()
	view.Seen("4")

	mux := http.NewServeMux()
	api := httpapi.NewAPI(deadStore(t), userFrom)

	failing := func(context.Context, countNotes) (int, error) {
		return 0, architecturekit.NewDomainError("nothing to count")
	}

	httpapi.QueryRevisioned(api, mux, "GET /notes", view, allNotes, failing, time.Second)

	response := askNotes(mux, nil)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d", response.Code)
	}

	if got := response.Header().Get("ETag"); got != "" {
		t.Errorf("a failed answer was tagged %q", got)
	}
}

// --- the building blocks on their own ---

func TestAwaitIgnoresARequestThatAsksForNothing(t *testing.T) {
	view := architecturekit.NewItemView[noteItem]()

	request := httptest.NewRequest(http.MethodGet, "/notes", nil)

	if err := httpapi.Await(t.Context(), request, view, time.Millisecond); err != nil {
		t.Errorf("got %v", err)
	}
}

func TestAwaitPassesOnWhatTheViewReports(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/notes", nil)
	request.Header.Set(httpapi.HeaderWaitFor, "5")

	// A view that refuses rather than waits.
	err := httpapi.Await(t.Context(), request, refusingView{}, time.Second)

	if err == nil {
		t.Error("waited without an error")
	}
}

type refusingView struct{}

func (refusingView) Revision() string { return "" }

func (refusingView) WaitFor(context.Context, string) error {
	return architecturekit.ErrNotARevision
}

// TestAnAnswerThatDependsOnMoreThanTheRevision covers the case the revision
// alone cannot describe. An answer such as "everything due today" changes at
// midnight although no event is written, so the revision stays put -- and a
// tag built from it alone would tell the caller, wrongly, that nothing had
// changed. That is exactly how an application can end up showing yesterday's
// list until something unrelated happens.
func TestAnAnswerThatDependsOnMoreThanTheRevision(t *testing.T) {
	view := architecturekit.NewItemView[noteItem]()
	view.Seen("7")

	day := "2026-09-22"

	mux := http.NewServeMux()
	api := httpapi.NewAPI(deadStore(t), userFrom)

	httpapi.QueryVarying(api, mux, "GET /notes", view, allNotes, countNotesIn(view),
		time.Second, func(*http.Request) string { return day })

	first := askNotes(mux, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", first.Code)
	}

	tag := first.Header().Get("ETag")
	if tag == "" {
		t.Fatal("the answer carries no entity tag")
	}

	// A browser left to itself decides how long an answer stays good and does
	// not ask again until it has.
	if cache := first.Header().Get("Cache-Control"); cache != "no-cache" {
		t.Errorf("got Cache-Control %q, want no-cache", cache)
	}

	// Same revision, same day: nothing has changed, and saying so is the
	// whole point of the tag.
	again := askNotes(mux, map[string]string{"If-None-Match": tag})
	if again.Code != http.StatusNotModified {
		t.Errorf("got %d for an unchanged answer, want 304", again.Code)
	}

	// Same revision, next day: the answer has changed even though no event
	// was written.
	day = "2026-09-23"

	tomorrow := askNotes(mux, map[string]string{"If-None-Match": tag})
	if tomorrow.Code != http.StatusOK {
		t.Errorf("got %d after the day turned over, want 200", tomorrow.Code)
	}

	if moved := tomorrow.Header().Get("ETag"); moved == tag {
		t.Error("the tag is the same on the next day, so the caller keeps yesterday's answer")
	}
}

// TestQueryRevisionedIsQueryVaryingWithoutAVariance keeps the plain case
// honest: an answer that follows from the read model alone needs nothing
// extra, and its tag still holds across requests.
func TestQueryRevisionedIsQueryVaryingWithoutAVariance(t *testing.T) {
	view := architecturekit.NewItemView[noteItem]()
	view.Seen("3")

	mux := servingNotes(t, view, time.Second)

	first := askNotes(mux, nil)
	tag := first.Header().Get("ETag")

	again := askNotes(mux, map[string]string{"If-None-Match": tag})
	if again.Code != http.StatusNotModified {
		t.Errorf("got %d, want 304 for an unchanged answer", again.Code)
	}
}
