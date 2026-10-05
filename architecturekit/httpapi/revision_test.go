package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
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

	_, err := view.Insert(context.Background(), eventID, note)
	require.NoError(t, err, "failed to insert %+v", note)
}

func countNotesIn(view *architecturekit.InMemoryView[string, noteItem]) httpapi.Answer[countNotes, int] {
	return func(ctx context.Context, _ countNotes) (int, error) {
		count := 0
		for _, err := range view.All(ctx) {
			if err != nil {
				return 0, err
			}
			count++
		}

		return count, nil
	}
}

func allNotes(*http.Request, httpapi.NoBody, user) (countNotes, error) { return countNotes{}, nil }

// servingNotes wires one revisioned query onto a mux.
func servingNotes(t *testing.T, view *architecturekit.InMemoryView[string, noteItem], wait time.Duration) *http.ServeMux {
	t.Helper()

	mux := http.NewServeMux()
	api := httpapi.NewAPI(deadStore(t), userFrom)

	httpapi.Query(api, mux, "QUERY /notes", allNotes, countNotesIn(view), httpapi.Revisioned(view, wait))

	return mux
}

// askNotes sends a request as a known user, with optional revision headers.
func askNotes(mux *http.ServeMux, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest("QUERY", "/notes", nil)
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
		httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom), http.NewServeMux(), "QUERY /notes", allNotes, answer, options...)
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
		// A nil pointer or function of a concrete type is not equal to nil once
		// an interface holds it, and would fail every request with 500.
		"Revisioned with a view that was never created": {
			"architecturekit/httpapi: Revisioned needs a view, not nil",
			func() {
				var catalog *architecturekit.InMemoryView[string, noteItem]
				httpapi.Revisioned(catalog, time.Second)
			},
		},
		"Revisioned with a nil function as the view": {
			"architecturekit/httpapi: Revisioned needs a view, not nil",
			func() { httpapi.Revisioned(revisionFunc(nil), time.Second) },
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
		"Awaiting without a view": {
			"architecturekit/httpapi: Awaiting needs a view, not nil",
			func() { httpapi.Awaiting(nil, time.Second) },
		},
		"Awaiting with a view that was never created": {
			"architecturekit/httpapi: Awaiting needs a view, not nil",
			func() {
				var catalog *architecturekit.InMemoryView[string, noteItem]
				httpapi.Awaiting(catalog, time.Second)
			},
		},
		"Awaiting with a nil function as the view": {
			"architecturekit/httpapi: Awaiting needs a view, not nil",
			func() { httpapi.Awaiting(revisionFunc(nil), time.Second) },
		},
		"Awaiting with a negative wait": {
			"architecturekit/httpapi: Awaiting needs a wait that is not negative, not -1s",
			func() { httpapi.Awaiting(noteView(), -time.Second) },
		},
		"Awaiting twice": {
			"architecturekit/httpapi: Awaiting is given twice",
			func() { wire(httpapi.Awaiting(noteView(), time.Second), httpapi.Awaiting(noteView(), time.Second)) },
		},
		"Awaiting after Revisioned": {
			"architecturekit/httpapi: Awaiting is given along with Revisioned, which waits as well",
			func() { wire(httpapi.Revisioned(noteView(), time.Second), httpapi.Awaiting(noteView(), time.Second)) },
		},
		"Revisioned after Awaiting": {
			"architecturekit/httpapi: Awaiting is given along with Revisioned, which waits as well",
			func() { wire(httpapi.Awaiting(noteView(), time.Second), httpapi.Revisioned(noteView(), time.Second)) },
		},
		// Only a revisioned query has a tag that Varying could add to.
		"Varying with Awaiting": {
			"architecturekit/httpapi: Varying needs Revisioned, since only a revisioned query has a tag",
			func() { wire(httpapi.Awaiting(noteView(), time.Second), httpapi.Varying(day)) },
		},
		"Varying before Awaiting": {
			"architecturekit/httpapi: Varying needs Revisioned, since only a revisioned query has a tag",
			func() { wire(httpapi.Varying(day), httpapi.Awaiting(noteView(), time.Second)) },
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
		httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom), mux, "QUERY /notes", allNotes, countNotesIn(view))

		response := askNotes(mux, map[string]string{httpapi.HeaderWaitFor: "99"})

		assert.Equal(t, http.StatusOK, response.Code)
		assert.Empty(t, response.Header().Get("Revision"))
		assert.Empty(t, response.Header().Get("ETag"))
	})

	t.Run("Varying before Revisioned is fine", func(t *testing.T) {
		assert.NotPanics(t, func() { wire(httpapi.Varying(day), httpapi.Revisioned(noteView(), time.Second)) })
	})

	t.Run("a view that is a function is revisioned", func(t *testing.T) {
		// It is not nil, so the check has to let it through.
		mux := http.NewServeMux()
		httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom), mux, "QUERY /notes", allNotes, answer,
			httpapi.Revisioned(revisionFunc(func(context.Context) (string, error) { return "7", nil }), time.Second))

		response := askNotes(mux, nil)

		assert.Equal(t, http.StatusOK, response.Code)
		assert.Equal(t, "7", response.Header().Get(httpapi.HeaderRevision))
	})
}

// revisionFunc is a view that is a function, so that a nil one can be handed
// to Revisioned. It has reached every revision it is asked for.
type revisionFunc func(ctx context.Context) (string, error)

func (f revisionFunc) Revision(ctx context.Context) (string, error) { return f(ctx) }

func (revisionFunc) WaitFor(context.Context, string) error { return nil }

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

		httpapi.Query(api, mux, "QUERY /notes", allNotes, countNotesIn(view), httpapi.Revisioned(view, time.Second))
		httpapi.Query(api, mux, "QUERY /other", allNotes, countNotesIn(view), httpapi.Revisioned(view, time.Second))

		tag := askNotes(mux, nil).Header().Get("ETag")

		request := httptest.NewRequest("QUERY", "/other", nil)
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
		request := httptest.NewRequest("QUERY", "/notes", nil)
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

		httpapi.Query(api, mux, "QUERY /notes", allNotes, failing, httpapi.Revisioned(view, time.Second))

		response := askNotes(mux, nil)

		assert.Equal(t, http.StatusUnprocessableEntity, response.Code)

		assert.Empty(t, response.Header().Get("ETag"), "a failed answer was tagged")
	})

	for _, test := range []struct {
		label   string
		err     error
		status  int
		message string
	}{
		{"an internal failure", errors.New("the revisions are gone"), http.StatusInternalServerError, "internal server error"},
		{"a transient failure", fmt.Errorf("%w: the revisions are down", architecturekit.ErrTransient), http.StatusServiceUnavailable,
			"internal server error"},
		{"a failure the caller can fix", fmt.Errorf("%w: the revisions are elsewhere", httpapi.ErrNotFound), http.StatusNotFound,
			"not found: the revisions are elsewhere"},
	} {
		t.Run("a view that fails to read its revision answers "+test.label+" as every other error, without answering the query", func(t *testing.T) {
			var logs bytes.Buffer
			var isAnswered atomic.Bool
			mux := http.NewServeMux()
			api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))

			unreadable := revisionFunc(func(context.Context) (string, error) { return "", test.err })
			httpapi.Query(api, mux, "QUERY /notes", allNotes, answering(&isAnswered, countNotesIn(noteView())),
				httpapi.Revisioned(unreadable, time.Second))

			response := askNotes(mux, nil)

			assert.Equal(t, test.status, response.Code)
			assert.JSONEq(t, messageOf(t, test.message), response.Body.String())
			assert.Empty(t, response.Header().Get("ETag"), "a failure was tagged")
			assert.Empty(t, response.Header().Get(httpapi.HeaderRevision), "a failure carries a revision")
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			assert.False(t, isAnswered.Load(), "the query must not be answered without its revision")

			if test.status >= http.StatusInternalServerError {
				assert.Contains(t, logs.String(), `msg="httpapi: internal failure"`)
				assert.Contains(t, logs.String(), `route="QUERY /notes"`)
				assert.Contains(t, logs.String(), test.err.Error())
			}
		})
	}

	t.Run("a view reads its revision within the context of the request", func(t *testing.T) {
		type key struct{}

		var asked context.Context
		mux := http.NewServeMux()
		httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom), mux, "QUERY /notes", allNotes, countNotesIn(noteView()),
			httpapi.Revisioned(revisionFunc(func(ctx context.Context) (string, error) {
				asked = ctx
				return "7", nil
			}), time.Second))

		request := httptest.NewRequestWithContext(context.WithValue(t.Context(), key{}, "the request"), "QUERY", "/notes", nil)
		request.Header.Set("X-User", "someone")
		response := serve(t, mux, request)

		require.Equal(t, http.StatusOK, response.Code)
		require.NotNil(t, asked, "the revision was not read")
		assert.Equal(t, "the request", asked.Value(key{}))
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

// TestIfNoneMatch covers the ways HTTP lets a caller send back the tags it
// holds: a proxy that compresses an answer marks its tag as weak, and a cache
// that holds several answers sends all of their tags at once.
func TestIfNoneMatch(t *testing.T) {
	view := noteView()
	view.Seen("4")

	mux := servingNotes(t, view, time.Second)

	tag := askNotes(mux, nil).Header().Get("ETag")
	require.NotEmpty(t, tag, "the answer carries no entity tag")

	for _, test := range []struct {
		label  string
		lines  []string
		status int
	}{
		{"the tag", []string{tag}, http.StatusNotModified},
		{"the tag, marked as weak", []string{"W/" + tag}, http.StatusNotModified},
		{"a list that holds the tag", []string{`"other", ` + tag + `, "third"`}, http.StatusNotModified},
		{"a list that holds the tag, without spaces", []string{`"other",` + tag}, http.StatusNotModified},
		{"a list that holds the tag, with a tab", []string{"\"other\",\t" + tag}, http.StatusNotModified},
		{"a list that holds the tag, marked as weak", []string{`W/"other", W/` + tag}, http.StatusNotModified},
		{"a list over several lines that holds the tag", []string{`"other"`, tag}, http.StatusNotModified},
		{"a list that holds the tag after one with a comma", []string{`"a,b", ` + tag}, http.StatusNotModified},
		{"*", []string{"*"}, http.StatusNotModified},
		{"another tag", []string{`"other"`}, http.StatusOK},
		{"another tag, marked as weak", []string{`W/"other"`}, http.StatusOK},
		{"a list without the tag", []string{`"other", W/"third"`}, http.StatusOK},
		{"a tag that holds * between commas", []string{`"a,*,b"`}, http.StatusOK},
		{"the tag without its opening quote", []string{strings.TrimPrefix(tag, `"`)}, http.StatusOK},
		{"the tag without its closing quote", []string{strings.TrimSuffix(tag, `"`)}, http.StatusOK},
	} {
		t.Run("with "+test.label+" is answered with "+strconv.Itoa(test.status), func(t *testing.T) {
			request := httptest.NewRequest("QUERY", "/notes", nil)
			request.Header.Set("X-User", "someone")
			for _, line := range test.lines {
				request.Header.Add("If-None-Match", line)
			}

			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)

			require.Equal(t, test.status, response.Code)
			assert.Equal(t, tag, response.Header().Get("ETag"), "the answer carries the tag as it is")
		})
	}

	t.Run("with * is answered in full if the answer has no tag", func(t *testing.T) {
		response := askNotes(servingNotes(t, noteView(), time.Second), map[string]string{"If-None-Match": "*"})

		assert.Equal(t, http.StatusOK, response.Code, "an answer without a tag was taken for unchanged")
	})
}

// The read side for a query whose input is a list, which the body of the
// request holds: it counts how many of the given notes there are.

type findNotesRequest struct {
	Texts []string `json:"texts"`
}

type findNotes struct {
	Texts []string
}

func toFindNotes(_ *http.Request, request findNotesRequest, _ user) (findNotes, error) {
	return findNotes(request), nil
}

func answerFindNotes(view *architecturekit.InMemoryView[string, noteItem]) httpapi.Answer[findNotes, int] {
	return func(ctx context.Context, query findNotes) (int, error) {
		count := 0
		for _, text := range query.Texts {
			_, isFound, err := view.Get(ctx, text)
			if err != nil {
				return 0, err
			}
			if isFound {
				count++
			}
		}

		return count, nil
	}
}

// askWith sends a request as a known user, with the given method and body,
// and with each of the lines as an If-None-Match header of its own.
func askWith(mux *http.ServeMux, method, target, body string, lines ...string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("X-User", "someone")
	request.Header.Set("Content-Type", "application/json")
	for _, line := range lines {
		request.Header.Add("If-None-Match", line)
	}

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	return recorder
}

// TestIfNoneMatchWithABody covers a query that asks with a body. HTTP has 304
// for QUERY, which it treats like GET (RFC 10008), and the tag holds the
// query that the body is turned into, so a body that asks something else does
// not match, while one that asks the same in other words does.
func TestIfNoneMatchWithABody(t *testing.T) {
	const found = `{"texts":["one","two"]}`

	view := noteView()
	insertNote(t, view, "1", noteItem{Text: "one"})
	view.Seen("4")

	mux := http.NewServeMux()
	api := httpapi.NewAPI(deadStore(t), userFrom)

	httpapi.Query(api, mux, "QUERY /notes/found", toFindNotes, answerFindNotes(view), httpapi.Revisioned(view, time.Second))

	tagOf := func(t *testing.T, body string) string {
		t.Helper()

		response := askWith(mux, "QUERY", "/notes/found", body)
		require.Equal(t, http.StatusOK, response.Code)

		tag := response.Header().Get("ETag")
		require.NotEmpty(t, tag, "the answer carries no entity tag")

		return tag
	}

	t.Run("with the tag is answered with 304", func(t *testing.T) {
		tag := tagOf(t, found)

		response := askWith(mux, "QUERY", "/notes/found", found, tag)

		require.Equal(t, http.StatusNotModified, response.Code)
		assert.Empty(t, response.Body.String(), "304 carried a body")

		assert.Equal(t, tag, response.Header().Get("ETag"), "304 says which answer is current")
		assert.Equal(t, "4", response.Header().Get(httpapi.HeaderRevision))
		assert.Equal(t, "private, no-cache", response.Header().Get("Cache-Control"))
	})

	for _, test := range []struct {
		label string
		lines func(tag string) []string
	}{
		{"the tag, marked as weak", func(tag string) []string { return []string{"W/" + tag} }},
		{"a list that holds the tag", func(tag string) []string { return []string{`"other", ` + tag} }},
		{"a list over several lines that holds the tag", func(tag string) []string { return []string{`"other"`, tag} }},
		{"*", func(string) []string { return []string{"*"} }},
	} {
		t.Run("with "+test.label+" is answered with 304", func(t *testing.T) {
			tag := tagOf(t, found)

			response := askWith(mux, "QUERY", "/notes/found", found, test.lines(tag)...)

			require.Equal(t, http.StatusNotModified, response.Code)
			assert.Equal(t, tag, response.Header().Get("ETag"))
		})
	}

	t.Run("without the tag is answered in full", func(t *testing.T) {
		tag := tagOf(t, found)

		for _, lines := range [][]string{nil, {`"other"`}, {`W/"other", "third"`}} {
			response := askWith(mux, "QUERY", "/notes/found", found, lines...)

			require.Equal(t, http.StatusOK, response.Code, "with %q", lines)
			assert.Equal(t, "1\n", response.Body.String(), "with %q", lines)
			assert.Equal(t, tag, response.Header().Get("ETag"), "with %q", lines)
		}
	})

	t.Run("that asks something else is answered in full", func(t *testing.T) {
		tag := tagOf(t, found)

		response := askWith(mux, "QUERY", "/notes/found", `{"texts":["one"]}`, tag)

		require.Equal(t, http.StatusOK, response.Code, "the tag of another body matched")
		assert.Equal(t, "1\n", response.Body.String())
	})

	t.Run("that asks the same in other words is answered with 304", func(t *testing.T) {
		tag := tagOf(t, found)

		// Names match fields regardless of case, so both decode into the same
		// query as found.
		for _, body := range []string{`{ "texts" : [ "one", "two" ] }`, `{"Texts":["one","two"]}`} {
			response := askWith(mux, "QUERY", "/notes/found", body, tag)

			require.Equal(t, http.StatusNotModified, response.Code, "with %s", body)
			assert.Equal(t, tag, response.Header().Get("ETag"), "with %s", body)
		}
	})

	t.Run("with * is answered in full if the answer has no tag", func(t *testing.T) {
		empty := http.NewServeMux()
		nothing := noteView()

		httpapi.Query(api, empty, "QUERY /notes/found", toFindNotes, answerFindNotes(nothing), httpapi.Revisioned(nothing, time.Second))

		response := askWith(empty, "QUERY", "/notes/found", found, "*")

		assert.Equal(t, http.StatusOK, response.Code, "an answer without a tag was taken for unchanged")
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
func askOwnNotes(_ *http.Request, _ httpapi.NoBody, caller user) (ownNotes, error) {
	if caller.UserID == "mallory" {
		return ownNotes{}, httpapi.ErrForbidden
	}

	return ownNotes{Owner: caller.UserID}, nil
}

func answerOwnNotes(view *architecturekit.InMemoryView[string, ownedNote]) httpapi.Answer[ownNotes, []string] {
	return func(ctx context.Context, query ownNotes) ([]string, error) {
		texts := []string{}
		for item, err := range view.All(ctx) {
			if err != nil {
				return nil, err
			}
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
	_, err := view.Insert(t.Context(), "1", ownedNote{Owner: "alice", Text: "alice's secret"})
	require.NoError(t, err)
	_, err = view.Insert(t.Context(), "2", ownedNote{Owner: "bob", Text: "bob's list"})
	require.NoError(t, err)
	view.Seen("2")

	mux := http.NewServeMux()
	api := httpapi.NewAPI(deadStore(t), userFrom)

	httpapi.Query(api, mux, "QUERY /notes", askOwnNotes, answerOwnNotes(view), httpapi.Revisioned(view, wait))

	return mux
}

func askNotesAs(mux *http.ServeMux, caller string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest("QUERY", "/notes", nil)
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

		httpapi.Query(api, mux, "QUERY /notes",
			func(*http.Request, httpapi.NoBody, user) (filtered, error) {
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

	t.Run("refuses a revision that is not one, and says why", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/notes", nil)
		request.Header.Set(httpapi.HeaderWaitFor, "soon")

		err := httpapi.Await(request, noteView(), time.Second)

		assert.ErrorIs(t, err, httpapi.ErrMalformed)
		assert.ErrorIs(t, err, architecturekit.ErrNotARevision, "the error has to stay inspectable")
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

func (refusingView) Revision(context.Context) (string, error) { return "", nil }

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

		httpapi.Query(api, mux, "QUERY /notes", allNotes, countNotesIn(view),
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

// TestAwaiting covers a query that waits for the revision a caller asks for,
// as a revisioned one does, but tags nothing, since its answer depends on more
// than the query and the view.
func TestAwaiting(t *testing.T) {
	// awaitingNotes wires one awaiting query onto a mux.
	awaitingNotes := func(t *testing.T, view architecturekit.Revisioned, answer httpapi.Answer[countNotes, int], wait time.Duration) *http.ServeMux {
		t.Helper()

		mux := http.NewServeMux()
		httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&bytes.Buffer{}))), mux, "QUERY /notes",
			allNotes, answer, httpapi.Awaiting(view, wait))

		return mux
	}

	// assertUntagged asserts that an answer carries nothing that a cache could
	// ask about again.
	assertUntagged := func(t *testing.T, response *httptest.ResponseRecorder) {
		t.Helper()

		assert.Empty(t, response.Header().Get(httpapi.HeaderRevision), "the answer carries a revision")
		assert.NotContains(t, response.Header(), "Etag", "the answer is tagged")
		assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	}

	t.Run("waits for the revision it was asked for, and tags nothing", func(t *testing.T) {
		view := noteView()
		view.Seen("1")

		// The revision arrives only after the request is already waiting.
		go func() {
			time.Sleep(100 * time.Millisecond)
			insertNote(t, view, "2", noteItem{Text: "late"})
			view.Seen("5")
		}()

		started := time.Now()
		response := askNotes(awaitingNotes(t, view, countNotesIn(view), 10*time.Second), map[string]string{
			httpapi.HeaderWaitFor: "5",
		})

		require.Equal(t, http.StatusOK, response.Code)
		assert.GreaterOrEqual(t, time.Since(started), 100*time.Millisecond, "answered too early, so it cannot have waited")
		assert.Equal(t, "1\n", response.Body.String(), "want the late item to be counted")
		assertUntagged(t, response)
	})

	t.Run("answers with what it has when the wait runs out", func(t *testing.T) {
		view := noteView()
		insertNote(t, view, "3", noteItem{Text: "one"})
		view.Seen("3")

		response := askNotes(awaitingNotes(t, view, countNotesIn(view), 50*time.Millisecond), map[string]string{
			httpapi.HeaderWaitFor: "99",
		})

		require.Equal(t, http.StatusOK, response.Code)
		assert.Equal(t, "1\n", response.Body.String())
		assertUntagged(t, response)
	})

	t.Run("answers at once with no wait at all", func(t *testing.T) {
		view := seenView("1")

		started := time.Now()
		response := askNotes(awaitingNotes(t, view, countNotesIn(view), 0), map[string]string{httpapi.HeaderWaitFor: "99"})

		require.Equal(t, http.StatusOK, response.Code)
		assert.Less(t, time.Since(started), time.Second, "waited although it was not to wait at all")
		assertUntagged(t, response)
	})

	t.Run("answers within the context of the request", func(t *testing.T) {
		type key struct{}

		var asked context.Context
		mux := awaitingNotes(t, seenView("1"), func(ctx context.Context, _ countNotes) (int, error) {
			asked = ctx
			return 0, nil
		}, time.Second)

		request := httptest.NewRequestWithContext(context.WithValue(t.Context(), key{}, "the request"), "QUERY", "/notes", nil)
		request.Header.Set("X-User", "someone")
		response := serve(t, mux, request)

		require.Equal(t, http.StatusOK, response.Code)
		require.NotNil(t, asked, "the query was not answered")
		assert.Equal(t, "the request", asked.Value(key{}))
	})

	t.Run("answers at once without a wanted revision", func(t *testing.T) {
		var isWaitedFor atomic.Bool
		view := waitedView{isWaitedFor: &isWaitedFor}

		response := askNotes(awaitingNotes(t, view, countNotesIn(noteView()), 10*time.Second), nil)

		require.Equal(t, http.StatusOK, response.Code)
		assert.False(t, isWaitedFor.Load(), "waited although nothing was asked for")
		assertUntagged(t, response)
	})

	t.Run("refuses a wanted revision that is not one", func(t *testing.T) {
		var isAnswered atomic.Bool

		response := askNotes(awaitingNotes(t, seenView("1"), answering(&isAnswered, countNotesIn(noteView())), 10*time.Second),
			map[string]string{httpapi.HeaderWaitFor: "soon"})

		assert.Equal(t, http.StatusBadRequest, response.Code)
		assert.JSONEq(t, messageOf(t, `malformed request: not a revision: "soon"`), response.Body.String())
		assert.False(t, isAnswered.Load(), "a query that asked for something that is not a revision was answered")
		assertUntagged(t, response)
	})

	t.Run("answers a failure while waiting as every other error", func(t *testing.T) {
		response := askNotes(awaitingNotes(t, brokenView{}, countNotesIn(noteView()), time.Second),
			map[string]string{httpapi.HeaderWaitFor: "7"})

		assert.Equal(t, http.StatusInternalServerError, response.Code)
		assert.JSONEq(t, messageOf(t, "internal server error"), response.Body.String())
		assertUntagged(t, response)
	})

	t.Run("is never answered with 304", func(t *testing.T) {
		view := seenView("4")
		mux := awaitingNotes(t, view, countNotesIn(view), time.Second)

		for _, tag := range []string{"*", `"4"`, `W/"4"`} {
			response := askNotes(mux, map[string]string{"If-None-Match": tag})

			require.Equal(t, http.StatusOK, response.Code, "with %s", tag)
			assert.Equal(t, "0\n", response.Body.String(), "with %s", tag)
			assertUntagged(t, response)
		}
	})

	t.Run("never reads the revision of the view, since it tags nothing", func(t *testing.T) {
		unreadable := revisionFunc(func(context.Context) (string, error) { return "", errors.New("the revisions are gone") })

		response := askNotes(awaitingNotes(t, unreadable, countNotesIn(noteView()), time.Second), nil)

		require.Equal(t, http.StatusOK, response.Code)
		assertUntagged(t, response)
	})

	t.Run("answers a failure of the query as every other error", func(t *testing.T) {
		failing := func(context.Context, countNotes) (int, error) {
			return 0, architecturekit.NewDomainError("nothing to count")
		}

		response := askNotes(awaitingNotes(t, seenView("4"), failing, time.Second), nil)

		assert.Equal(t, http.StatusUnprocessableEntity, response.Code)
		assert.JSONEq(t, messageOf(t, "nothing to count"), response.Body.String())
		assertUntagged(t, response)
	})

	t.Run("answers a panic with 500", func(t *testing.T) {
		panicking := func(context.Context, countNotes) (int, error) { panic("the index is broken") }

		response := askNotes(awaitingNotes(t, seenView("4"), panicking, time.Second), nil)

		assert.Equal(t, http.StatusInternalServerError, response.Code)
		assertUntagged(t, response)
	})

	t.Run("nobody can make the server wait without being let in", func(t *testing.T) {
		view := seenView("1")

		// No X-User header, so the request never gets as far as waiting.
		request := httptest.NewRequest("QUERY", "/notes", nil)
		request.Header.Set(httpapi.HeaderWaitFor, "99")

		started := time.Now()
		response := serve(t, awaitingNotes(t, view, countNotesIn(view), 10*time.Second), request)

		assert.Equal(t, http.StatusUnauthorized, response.Code)
		assert.LessOrEqual(t, time.Since(started), time.Second, "waited before refusing")
	})
}
