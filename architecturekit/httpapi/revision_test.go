package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/httpapi"
)

// The read side used below: a view of notes, and a query that counts them.

type noteItem struct {
	Text string
}

type countNotes struct{}

// noteView is a view of notes, each keyed by its text.
func noteView() *architecturekit.InMemoryView[string, noteItem] {
	return architecturekit.NewInMemoryView(func(item noteItem) string { return item.Text })
}

func insertNote(t *testing.T, view *architecturekit.InMemoryView[string, noteItem], eventID string, note noteItem) {
	t.Helper()

	err := view.Insert(context.Background(), eventID, note)
	require.NoError(t, err, "failed to insert %+v", note)
}

func countNotesIn(view *architecturekit.InMemoryView[string, noteItem]) httpapi.Answer[countNotes, int] {
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
func servingNotes(t *testing.T, view *architecturekit.InMemoryView[string, noteItem], wait time.Duration) *http.ServeMux {
	t.Helper()

	mux := http.NewServeMux()
	api := httpapi.NewAPI(deadStore(t), userFrom)

	httpapi.Query(api, mux, "GET /notes", allNotes, countNotesIn(view), httpapi.Revisioned(view, wait))

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

func TestQueryOptions(t *testing.T) {
	answer := func(context.Context, countNotes) (int, error) { return 0, nil }
	wire := func(options ...httpapi.QueryOption) {
		httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom), http.NewServeMux(), "GET /notes", allNotes, answer, options...)
	}
	day := func(*http.Request) string { return "2026-10-02" }

	for name, test := range map[string]struct {
		message string
		wire    func()
	}{
		"Revisioned without a view": {
			"architecturekit/httpapi: Revisioned needs a view, not nil",
			func() { httpapi.Revisioned(nil, time.Second) },
		},
		"Revisioned with a negative wait": {
			"architecturekit/httpapi: Revisioned needs a wait that is not negative, not -1s",
			func() { httpapi.Revisioned(noteView(), -time.Second) },
		},
		"Revisioned twice": {
			"architecturekit/httpapi: Revisioned is given twice",
			func() { wire(httpapi.Revisioned(noteView(), time.Second), httpapi.Revisioned(noteView(), time.Second)) },
		},
		"Varying without a function": {
			"architecturekit/httpapi: Varying needs a function, not nil",
			func() { httpapi.Varying(nil) },
		},
		"Varying twice": {
			"architecturekit/httpapi: Varying is given twice",
			func() { wire(httpapi.Revisioned(noteView(), time.Second), httpapi.Varying(day), httpapi.Varying(day)) },
		},
		"Varying without Revisioned": {
			"architecturekit/httpapi: Varying needs Revisioned, since only a revisioned query has a tag",
			func() { wire(httpapi.Varying(day)) },
		},
	} {
		t.Run(name+" panics", func(t *testing.T) {
			assert.PanicsWithValue(t, test.message, test.wire)
		})
	}

	t.Run("a query that is not revisioned answers without a revision", func(t *testing.T) {
		view := noteView()
		view.Seen("3")

		mux := http.NewServeMux()
		httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom), mux, "GET /notes", allNotes, countNotesIn(view))

		response := askNotes(mux, map[string]string{httpapi.HeaderWaitFor: "99"})

		assert.Equal(t, http.StatusOK, response.Code)
		assert.Empty(t, response.Header().Get("Revision"))
		assert.Empty(t, response.Header().Get("ETag"))
	})
}

func TestRevisioned(t *testing.T) {
	t.Run("a query without a wanted revision answers at once", func(t *testing.T) {
		view := noteView()
		insertNote(t, view, "1", noteItem{Text: "one"})
		view.Seen("3")

		response := askNotes(servingNotes(t, view, time.Second), nil)

		assert.Equal(t, http.StatusOK, response.Code)

		assert.Equal(t, "3", response.Header().Get(httpapi.HeaderRevision))
		assert.Equal(t, "3", response.Header().Get("Revision"), "the header has no X- prefix")

		assert.NotEmpty(t, response.Header().Get("ETag"))
	})

	t.Run("a query waits for the revision it was asked for", func(t *testing.T) {
		view := noteView()
		view.Seen("1")

		// The revision arrives only after the request is already waiting.
		go func() {
			time.Sleep(100 * time.Millisecond)
			insertNote(t, view, "2", noteItem{Text: "late"})
			view.Seen("5")
		}()

		started := time.Now()
		response := askNotes(servingNotes(t, view, 10*time.Second), map[string]string{
			httpapi.HeaderWaitFor: "5",
		})

		assert.Equal(t, http.StatusOK, response.Code)

		assert.GreaterOrEqual(t, time.Since(started), 100*time.Millisecond, "answered too early, so it cannot have waited")

		assert.Equal(t, "5", response.Header().Get(httpapi.HeaderRevision))

		// The whole point: the item written with that revision is in the answer.
		assert.Equal(t, "1\n", response.Body.String(), "want the late item to be counted")
	})

	t.Run("a query answers with what it has when the wait runs out", func(t *testing.T) {
		view := noteView()
		insertNote(t, view, "3", noteItem{Text: "one"})
		view.Seen("2")

		response := askNotes(servingNotes(t, view, 50*time.Millisecond), map[string]string{
			httpapi.HeaderWaitFor: "99",
		})

		// Running out of time is not an error: the caller gets the data and is
		// told which revision it is looking at.
		assert.Equal(t, http.StatusOK, response.Code)

		assert.Equal(t, "2", response.Header().Get(httpapi.HeaderRevision))
	})

	t.Run("a wanted revision that is not one is refused", func(t *testing.T) {
		view := noteView()
		view.Seen("1")

		response := askNotes(servingNotes(t, view, 10*time.Second), map[string]string{
			httpapi.HeaderWaitFor: "soon",
		})

		assert.Equal(t, http.StatusBadRequest, response.Code)
	})

	t.Run("a known revision is answered with Not Modified", func(t *testing.T) {
		view := noteView()
		insertNote(t, view, "4", noteItem{Text: "one"})
		view.Seen("4")

		mux := servingNotes(t, view, time.Second)

		first := askNotes(mux, nil)
		tag := first.Header().Get("ETag")

		second := askNotes(mux, map[string]string{"If-None-Match": tag})

		require.Equal(t, http.StatusNotModified, second.Code)

		assert.Empty(t, second.Body.String(), "304 carried a body")

		// After something changes, the same tag no longer matches.
		insertNote(t, view, "5", noteItem{Text: "two"})
		view.Seen("5")

		third := askNotes(mux, map[string]string{"If-None-Match": tag})

		assert.Equal(t, http.StatusOK, third.Code)
	})

	t.Run("one resource's tag does not match another", func(t *testing.T) {
		// Every query over the same view shares a revision, so a tag that held
		// nothing else would wrongly match across resources.
		view := noteView()
		view.Seen("4")

		mux := http.NewServeMux()
		api := httpapi.NewAPI(deadStore(t), userFrom)

		httpapi.Query(api, mux, "GET /notes", allNotes, countNotesIn(view), httpapi.Revisioned(view, time.Second))
		httpapi.Query(api, mux, "GET /other", allNotes, countNotesIn(view), httpapi.Revisioned(view, time.Second))

		tag := askNotes(mux, nil).Header().Get("ETag")

		request := httptest.NewRequest(http.MethodGet, "/other", nil)
		request.Header.Set("X-User", "someone")
		request.Header.Set("If-None-Match", tag)

		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)

		assert.Equal(t, http.StatusOK, response.Code, "the tag of /notes matched /other")
	})

	t.Run("a view that has seen nothing carries no tag", func(t *testing.T) {
		view := noteView()

		response := askNotes(servingNotes(t, view, time.Second), nil)

		assert.Equal(t, http.StatusOK, response.Code)

		assert.Empty(t, response.Header().Get("ETag"))
		assert.Empty(t, response.Header().Get(httpapi.HeaderRevision))
	})

	t.Run("nobody can make the server wait without being let in", func(t *testing.T) {
		view := noteView()
		view.Seen("1")

		// No X-User header, so the request never gets as far as waiting.
		request := httptest.NewRequest(http.MethodGet, "/notes", nil)
		request.Header.Set(httpapi.HeaderWaitFor, "99")

		started := time.Now()
		response := httptest.NewRecorder()
		servingNotes(t, view, 10*time.Second).ServeHTTP(response, request)

		assert.Equal(t, http.StatusUnauthorized, response.Code)

		assert.LessOrEqual(t, time.Since(started), time.Second, "waited before refusing")
	})

	t.Run("a failing query carries no revision", func(t *testing.T) {
		view := noteView()
		view.Seen("4")

		mux := http.NewServeMux()
		api := httpapi.NewAPI(deadStore(t), userFrom)

		failing := func(context.Context, countNotes) (int, error) {
			return 0, architecturekit.NewDomainError("nothing to count")
		}

		httpapi.Query(api, mux, "GET /notes", allNotes, failing, httpapi.Revisioned(view, time.Second))

		response := askNotes(mux, nil)

		assert.Equal(t, http.StatusUnprocessableEntity, response.Code)

		assert.Empty(t, response.Header().Get("ETag"), "a failed answer was tagged")
	})

	// The plain case stays honest: an answer that follows from the read model
	// alone needs nothing extra, and its tag still holds across requests.
	t.Run("is Varying without a variance", func(t *testing.T) {
		view := noteView()
		view.Seen("3")

		mux := servingNotes(t, view, time.Second)

		first := askNotes(mux, nil)
		tag := first.Header().Get("ETag")

		again := askNotes(mux, map[string]string{"If-None-Match": tag})
		assert.Equal(t, http.StatusNotModified, again.Code, "the answer has not changed")
	})
}

// The read side for callers who are told apart: everybody owns notes, and
// asks for their own. This is how an application with users usually reads,
// and why a tag must not be shared between them.

type ownNotes struct {
	Owner string
}

type ownedNote struct {
	Owner string
	Text  string
}

func ownedNoteView() *architecturekit.InMemoryView[string, ownedNote] {
	return architecturekit.NewInMemoryView(func(item ownedNote) string { return item.Owner + "/" + item.Text })
}

// askOwnNotes builds the query from the caller, and refuses the one caller who
// may not read notes at all.
func askOwnNotes(_ *http.Request, caller user) (ownNotes, error) {
	if caller.UserID == "mallory" {
		return ownNotes{}, httpapi.ErrForbidden
	}

	return ownNotes{Owner: caller.UserID}, nil
}

func answerOwnNotes(view *architecturekit.InMemoryView[string, ownedNote]) httpapi.Answer[ownNotes, []string] {
	return func(ctx context.Context, query ownNotes) ([]string, error) {
		items, err := view.All(ctx)
		if err != nil {
			return nil, err
		}

		texts := []string{}
		for item := range items {
			if item.Owner == query.Owner {
				texts = append(texts, item.Text)
			}
		}

		return texts, nil
	}
}

func servingOwnNotes(t *testing.T, wait time.Duration) *http.ServeMux {
	t.Helper()

	view := ownedNoteView()
	require.NoError(t, view.Insert(t.Context(), "1", ownedNote{Owner: "alice", Text: "alice's secret"}))
	require.NoError(t, view.Insert(t.Context(), "2", ownedNote{Owner: "bob", Text: "bob's list"}))
	view.Seen("2")

	mux := http.NewServeMux()
	api := httpapi.NewAPI(deadStore(t), userFrom)

	httpapi.Query(api, mux, "GET /notes", askOwnNotes, answerOwnNotes(view), httpapi.Revisioned(view, wait))

	return mux
}

func askNotesAs(mux *http.ServeMux, caller string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/notes", nil)
	request.Header.Set("X-User", caller)

	for name, value := range headers {
		request.Header.Set(name, value)
	}

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	return recorder
}

// TestTagsOfCallers covers callers who share a browser one after the other: the
// browser keeps the answer for the first, and asks for the second whether that
// is still current. It must not be, or the second caller sees the answer of the
// first.
func TestTagsOfCallers(t *testing.T) {
	t.Run("callers who ask for their own get tags of their own", func(t *testing.T) {
		mux := servingOwnNotes(t, time.Second)

		alice := askNotesAs(mux, "alice", nil)
		bob := askNotesAs(mux, "bob", nil)

		require.Equal(t, http.StatusOK, alice.Code)
		require.Equal(t, http.StatusOK, bob.Code)

		assert.NotEqual(t, alice.Header().Get("ETag"), bob.Header().Get("ETag"))
	})

	t.Run("a caller who sends the tag of another gets their own answer", func(t *testing.T) {
		mux := servingOwnNotes(t, time.Second)

		alice := askNotesAs(mux, "alice", nil)
		bob := askNotesAs(mux, "bob", map[string]string{"If-None-Match": alice.Header().Get("ETag")})

		require.Equal(t, http.StatusOK, bob.Code, "bob was told that alice's answer is his")
		assert.JSONEq(t, `["bob's list"]`, bob.Body.String())
	})

	t.Run("a caller who asks the same again is told that nothing changed", func(t *testing.T) {
		mux := servingOwnNotes(t, time.Second)

		first := askNotesAs(mux, "alice", nil)
		again := askNotesAs(mux, "alice", map[string]string{"If-None-Match": first.Header().Get("ETag")})

		assert.Equal(t, http.StatusNotModified, again.Code)
	})

	t.Run("a caller who may not ask is refused rather than told that nothing changed", func(t *testing.T) {
		mux := servingOwnNotes(t, time.Second)

		// Without a user in the query, mallory's tag would be anybody's.
		alice := askNotesAs(mux, "alice", nil)
		mallory := askNotesAs(mux, "mallory", map[string]string{"If-None-Match": alice.Header().Get("ETag")})

		assert.Equal(t, http.StatusForbidden, mallory.Code)
	})

	t.Run("a caller who may not ask is refused before anything waits", func(t *testing.T) {
		mux := servingOwnNotes(t, 10*time.Second)

		started := time.Now()
		mallory := askNotesAs(mux, "mallory", map[string]string{httpapi.HeaderWaitFor: "99"})

		assert.Equal(t, http.StatusForbidden, mallory.Code)
		assert.LessOrEqual(t, time.Since(started), time.Second, "waited before refusing")
	})

	t.Run("answers are private even when nothing changed", func(t *testing.T) {
		mux := servingOwnNotes(t, time.Second)

		first := askNotesAs(mux, "alice", nil)
		again := askNotesAs(mux, "alice", map[string]string{"If-None-Match": first.Header().Get("ETag")})

		assert.Equal(t, "private, no-cache", first.Header().Get("Cache-Control"))
		assert.Equal(t, "private, no-cache", again.Header().Get("Cache-Control"))
	})

	t.Run("a query that can not be spelled out goes without a tag", func(t *testing.T) {
		view := noteView()
		view.Seen("3")

		mux := http.NewServeMux()
		api := httpapi.NewAPI(deadStore(t), userFrom)

		type filtered struct{ Keep func(noteItem) bool }

		httpapi.Query(api, mux, "GET /notes",
			func(*http.Request, user) (filtered, error) {
				return filtered{Keep: func(noteItem) bool { return true }}, nil
			},
			func(context.Context, filtered) (int, error) { return 0, nil },
			httpapi.Revisioned(view, time.Second))

		response := askNotes(mux, nil)

		require.Equal(t, http.StatusOK, response.Code)
		assert.NotContains(t, response.Header(), "Etag", "a tag that can not tell queries apart was handed out")
		assert.Equal(t, "3", response.Header().Get(httpapi.HeaderRevision), "the revision is still worth knowing")
		assert.Equal(t, "private, no-cache", response.Header().Get("Cache-Control"))
	})
}

// --- the building blocks on their own ---

func TestAwait(t *testing.T) {
	t.Run("ignores a request that asks for nothing", func(t *testing.T) {
		view := noteView()

		request := httptest.NewRequest(http.MethodGet, "/notes", nil)

		assert.NoError(t, httpapi.Await(request, view, time.Millisecond))
	})

	t.Run("stops waiting once the request is over", func(t *testing.T) {
		view := noteView()
		view.Seen("1")

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/notes", nil)
		request.Header.Set(httpapi.HeaderWaitFor, "99")

		started := time.Now()
		err := httpapi.Await(request, view, 10*time.Second)

		assert.NoError(t, err, "running out of time is no error, and neither is a caller who went away")
		assert.Less(t, time.Since(started), time.Second, "waited for a caller who is gone")
	})

	t.Run("passes on what the view reports", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/notes", nil)
		request.Header.Set(httpapi.HeaderWaitFor, "5")

		// A view that refuses rather than waits.
		err := httpapi.Await(request, refusingView{}, time.Second)

		assert.Error(t, err, "waited without an error")
	})
}

type refusingView struct{}

func (refusingView) Revision() string { return "" }

func (refusingView) WaitFor(context.Context, string) error {
	return architecturekit.ErrNotARevision
}

// TestVarying covers the case the revision alone cannot describe. An answer
// such as "everything due today" changes at midnight although no event is
// written, so the revision stays put, and a tag built from it alone would
// tell the caller, wrongly, that nothing had changed. That is exactly how an
// application can end up showing yesterday's list until something unrelated
// happens.
func TestVarying(t *testing.T) {
	t.Run("an answer that depends on more than the revision", func(t *testing.T) {
		view := noteView()
		view.Seen("7")

		day := "2026-09-22"

		mux := http.NewServeMux()
		api := httpapi.NewAPI(deadStore(t), userFrom)

		httpapi.Query(api, mux, "GET /notes", allNotes, countNotesIn(view),
			httpapi.Revisioned(view, time.Second),
			httpapi.Varying(func(*http.Request) string { return day }))

		first := askNotes(mux, nil)
		require.Equal(t, http.StatusOK, first.Code)

		tag := first.Header().Get("ETag")
		require.NotEmpty(t, tag, "the answer carries no entity tag")

		// A browser left to itself decides how long an answer stays good and does
		// not ask again until it has.
		assert.Equal(t, "private, no-cache", first.Header().Get("Cache-Control"))

		// Same revision, same day: nothing has changed, and saying so is the
		// whole point of the tag.
		again := askNotes(mux, map[string]string{"If-None-Match": tag})
		assert.Equal(t, http.StatusNotModified, again.Code, "the answer has not changed")

		// Same revision, next day: the answer has changed even though no event
		// was written.
		day = "2026-09-23"

		tomorrow := askNotes(mux, map[string]string{"If-None-Match": tag})
		assert.Equal(t, http.StatusOK, tomorrow.Code, "the day turned over")

		assert.NotEqual(t, tag, tomorrow.Header().Get("ETag"), "the tag is the same on the next day, so the caller keeps yesterday's answer")
	})
}
