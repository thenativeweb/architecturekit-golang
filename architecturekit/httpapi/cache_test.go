package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/httpapi"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// Only an answer that carries a revision has a tag that a cache can ask about
// (see Revisioned). Every other answer may be the outcome of a command, or
// hold what only its caller may see, and a cache that kept it would hand it
// out again, so no cache may keep it.

func TestCacheControl(t *testing.T) {
	// failingWith returns an answer of a query that fails with the given
	// error.
	failingWith := func(err error) httpapi.Answer[countNotes, int] {
		return func(context.Context, countNotes) (int, error) { return 0, err }
	}

	// queried wires a query at QUERY /notes, with the given options, and asks
	// it, with the given headers.
	queried := func(t *testing.T, answer httpapi.Answer[countNotes, int], headers map[string]string, options ...httpapi.QueryOption) *httptest.ResponseRecorder {
		t.Helper()

		mux := http.NewServeMux()
		httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&bytes.Buffer{}))), mux, "QUERY /notes", allNotes, answer, options...)

		return askNotes(mux, headers)
	}

	answers := map[string]func(t *testing.T) *httptest.ResponseRecorder{
		"a command that succeeded": func(t *testing.T) *httptest.ResponseRecorder {
			return postNote(t, routed(httpapi.NewAPI(writingStore(t), userFrom)), `{"id":"1","text":"hello"}`)
		},
		"a command that failed": func(t *testing.T) *httptest.ResponseRecorder {
			return postNote(t, routed(httpapi.NewAPI(deadStore(t), userFrom)), `{"text":"no id"}`)
		},
		"a command whose subject panicked": func(t *testing.T) *httptest.ResponseRecorder {
			mux := http.NewServeMux()
			httpapi.Route(httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&bytes.Buffer{}))),
				mux, "POST /note", toUncheckedNote, uncheckedNoteDecider())

			return postUncheckedNote(t, mux, "")
		},
		"a query that succeeded": func(t *testing.T) *httptest.ResponseRecorder {
			return queried(t, countNotesIn(noteView()), nil)
		},
		"a query that failed": func(t *testing.T) *httptest.ResponseRecorder {
			return queried(t, failingWith(architecturekit.NewDomainError("nothing to count")), nil)
		},
		"a query that panicked": func(t *testing.T) *httptest.ResponseRecorder {
			return queried(t, func(context.Context, countNotes) (int, error) { panic("the index is broken") }, nil)
		},
		"a query whose caller is unknown": func(t *testing.T) *httptest.ResponseRecorder {
			mux := http.NewServeMux()
			httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&bytes.Buffer{}))),
				mux, "QUERY /notes", allNotes, countNotesIn(noteView()))

			return ask(t, mux, "/notes", "")
		},
		"a revisioned query whose view has seen nothing": func(t *testing.T) *httptest.ResponseRecorder {
			view := noteView()

			return queried(t, countNotesIn(view), nil, httpapi.Revisioned(view, time.Second))
		},
		"a revisioned query that failed": func(t *testing.T) *httptest.ResponseRecorder {
			return queried(t, failingWith(architecturekit.NewDomainError("nothing to count")), nil,
				httpapi.Revisioned(seenView("4"), time.Second))
		},
		"a revisioned query that failed internally": func(t *testing.T) *httptest.ResponseRecorder {
			return queried(t, failingWith(errors.New("the index is gone")), nil,
				httpapi.Revisioned(seenView("4"), time.Second))
		},
		"a revisioned query that waited for something that is not a revision": func(t *testing.T) *httptest.ResponseRecorder {
			view := seenView("4")

			return queried(t, countNotesIn(view), map[string]string{httpapi.HeaderWaitFor: "soon"}, httpapi.Revisioned(view, time.Second))
		},
		"a revisioned query that panicked": func(t *testing.T) *httptest.ResponseRecorder {
			return queried(t, func(context.Context, countNotes) (int, error) { panic("the index is broken") }, nil,
				httpapi.Revisioned(seenView("4"), time.Second))
		},
		"a revisioned query whose caller is unknown": func(t *testing.T) *httptest.ResponseRecorder {
			mux := http.NewServeMux()
			view := seenView("4")
			httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&bytes.Buffer{}))),
				mux, "QUERY /notes", allNotes, countNotesIn(view), httpapi.Revisioned(view, time.Second))

			return ask(t, mux, "/notes", "")
		},
		"Respond after a command that succeeded": func(*testing.T) *httptest.ResponseRecorder {
			request, api := inAHandler(&bytes.Buffer{})
			recorder := httptest.NewRecorder()
			httpapi.Respond(recorder, request, api, []eventsourcingdb.Event{{ID: "4"}}, nil)

			return recorder
		},
		"Respond with an error": func(*testing.T) *httptest.ResponseRecorder {
			request, api := inAHandler(&bytes.Buffer{})
			recorder := httptest.NewRecorder()
			httpapi.Respond(recorder, request, api, nil, fmt.Errorf("%w: id must not be empty", httpapi.ErrMalformed))

			return recorder
		},
		"RespondResult with a result": func(*testing.T) *httptest.ResponseRecorder {
			request, api := inAHandler(&bytes.Buffer{})
			recorder := httptest.NewRecorder()
			httpapi.RespondResult(recorder, request, api, []noteResponse{{Text: "only"}}, nil)

			return recorder
		},
		"RespondResult with an error": func(*testing.T) *httptest.ResponseRecorder {
			request, api := inAHandler(&bytes.Buffer{})
			recorder := httptest.NewRecorder()
			httpapi.RespondResult(recorder, request, api, []noteResponse(nil), errors.New("the index is gone"))

			return recorder
		},
		"RespondError": func(*testing.T) *httptest.ResponseRecorder {
			request, api := inAHandler(&bytes.Buffer{})
			recorder := httptest.NewRecorder()
			httpapi.RespondError(recorder, request, api, httpapi.ErrForbidden)

			return recorder
		},
	}

	for name, answer := range answers {
		t.Run(name+" may not be kept by any cache", func(t *testing.T) {
			response := answer(t)

			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			assert.Empty(t, response.Header().Get(httpapi.HeaderRevision), "the answer carries a revision")
		})
	}

	t.Run("a revisioned query that succeeded may be kept, if it is asked about again", func(t *testing.T) {
		view := seenView("4")
		mux := http.NewServeMux()
		httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom), mux, "QUERY /notes", allNotes, countNotesIn(view),
			httpapi.Revisioned(view, time.Second))

		first := askNotes(mux, nil)
		require.Equal(t, http.StatusOK, first.Code)

		again := askNotes(mux, map[string]string{"If-None-Match": first.Header().Get("ETag")})
		require.Equal(t, http.StatusNotModified, again.Code)

		assert.Equal(t, "private, no-cache", first.Header().Get("Cache-Control"))
		assert.Equal(t, "private, no-cache", again.Header().Get("Cache-Control"))
	})
}
