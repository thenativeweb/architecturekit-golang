package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/httpapi"
)

// A query takes its input from the body, which Query and Ask decode into the
// request type by the rules of BodyOf, as Route does for a command, before
// the function that turns the request into a query gets it.

// listNotesRequest is the body of a query that lists notes.
type listNotesRequest struct {
	Limit int `json:"limit"`
}

// toListNotesOf builds the query that lists notes from the body.
func toListNotesOf(_ *http.Request, request listNotesRequest, _ user) (listNotes, error) {
	return listNotes(request), nil
}

// waitedView is a view that has seen the revision 4, and records whether
// anything waited for it.
type waitedView struct {
	isWaitedFor *atomic.Bool
}

func (waitedView) Revision() string { return "4" }

func (view waitedView) WaitFor(context.Context, string) error {
	view.isWaitedFor.Store(true)

	return nil
}

// queryKinds are the ways that Query wires a query: as it is, and revisioned,
// with a view that records whether anything waited for it.
var queryKinds = map[string]func(isWaitedFor *atomic.Bool) []httpapi.QueryOption{
	"a query": func(*atomic.Bool) []httpapi.QueryOption { return nil },
	"a revisioned query": func(isWaitedFor *atomic.Bool) []httpapi.QueryOption {
		return []httpapi.QueryOption{httpapi.Revisioned(waitedView{isWaitedFor: isWaitedFor}, time.Second)}
	},
}

// queried wires toQuery and answer at QUERY /notes, with the options.
func queried[TRequest any, TQuery any, TResult any](
	t *testing.T,
	toQuery httpapi.ToQuery[user, TRequest, TQuery],
	answer httpapi.Answer[TQuery, TResult],
	options ...httpapi.QueryOption,
) *http.ServeMux {
	t.Helper()

	mux := http.NewServeMux()
	httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom), mux, "QUERY /notes", toQuery, answer, options...)

	return mux
}

// asking returns a request of the caller, if any, that asks for the notes with
// the method, the Content-Type, if any, and the body.
func asking(method, caller, contentType string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, "/notes", body)
	if caller != "" {
		request.Header.Set("X-User", caller)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	return request
}

// recording wraps toQuery, and records whether it was called.
func recording[TRequest any, TQuery any](isBuilt *atomic.Bool, toQuery httpapi.ToQuery[user, TRequest, TQuery]) httpapi.ToQuery[user, TRequest, TQuery] {
	return func(r *http.Request, request TRequest, caller user) (TQuery, error) {
		isBuilt.Store(true)

		return toQuery(r, request, caller)
	}
}

// answering wraps answer, and records whether it was called.
func answering[TQuery any, TResult any](isAnswered *atomic.Bool, answer httpapi.Answer[TQuery, TResult]) httpapi.Answer[TQuery, TResult] {
	return func(ctx context.Context, query TQuery) (TResult, error) {
		isAnswered.Store(true)

		return answer(ctx, query)
	}
}

// notesIn returns the texts of the notes that an answer holds.
func notesIn(t *testing.T, response *httptest.ResponseRecorder) []string {
	t.Helper()

	var notes []noteResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &notes), response.Body.String())

	texts := []string{}
	for _, item := range notes {
		texts = append(texts, item.Text)
	}

	return texts
}

func TestToQuery(t *testing.T) {
	for kind, options := range queryKinds {
		t.Run(kind+" gets the body, decoded into the request type", func(t *testing.T) {
			mux := queried(t, toListNotesOf, answerListNotes, options(&atomic.Bool{})...)

			response := serve(t, mux, asking("QUERY", "golo", "application/json", strings.NewReader(`{"limit":2}`)))

			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Equal(t, []string{"first", "second"}, notesIn(t, response))
		})
	}

	t.Run("gets the request itself, the body, and the user", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)
		request := asking("QUERY", "golo", "application/json", strings.NewReader(`{"limit":2}`))

		var (
			gotRequest *http.Request
			gotBody    listNotesRequest
			gotUser    user
		)
		_, err := httpapi.Ask(request, api, func(r *http.Request, body listNotesRequest, u user) (listNotes, error) {
			gotRequest, gotBody, gotUser = r, body, u
			return listNotes(body), nil
		}, answerListNotes)

		require.NoError(t, err)
		assert.Same(t, request, gotRequest, "the function has to get the request itself, with its context")
		assert.Equal(t, listNotesRequest{Limit: 2}, gotBody)
		assert.Equal(t, user{UserID: "golo"}, gotUser)
	})
}

// bodyFailures are bodies that can not be decoded into a listNotesRequest,
// with the status and the message they are answered with, as for a command.
var bodyFailures = []struct {
	label       string
	contentType string
	body        func() io.Reader
	category    error
	status      int
	message     string
}{
	{
		label: "without a Content-Type", contentType: "", body: func() io.Reader { return strings.NewReader(`{"limit":2}`) },
		category: httpapi.ErrUnsupportedMediaType, status: http.StatusUnsupportedMediaType,
		message: "unsupported media type: Content-Type is required",
	},
	{
		label: "that is not said to be JSON", contentType: "text/plain", body: func() io.Reader { return strings.NewReader(`{"limit":2}`) },
		category: httpapi.ErrUnsupportedMediaType, status: http.StatusUnsupportedMediaType,
		message: `unsupported media type: "text/plain" is not application/json`,
	},
	{
		label: "that is too large", contentType: "application/json",
		body:     func() io.Reader { return strings.NewReader(strings.Repeat(" ", httpapi.MaxRequestBody+1)) },
		category: httpapi.ErrTooLarge, status: http.StatusRequestEntityTooLarge,
		message: "request body too large: at most 1048576 bytes are read",
	},
	{
		label: "that is empty", contentType: "application/json", body: func() io.Reader { return strings.NewReader("") },
		category: httpapi.ErrMalformed, status: http.StatusBadRequest,
		message: "malformed request: empty body",
	},
	{
		label: "that is no JSON", contentType: "application/json", body: func() io.Reader { return strings.NewReader("not json") },
		category: httpapi.ErrMalformed, status: http.StatusBadRequest,
		message: "malformed request: invalid JSON",
	},
	{
		label: "with an unknown field", contentType: "application/json", body: func() io.Reader { return strings.NewReader(`{"limt":2}`) },
		category: httpapi.ErrMalformed, status: http.StatusBadRequest,
		message: `malformed request: unknown field "limt"`,
	},
	{
		label: "with a value of the wrong kind", contentType: "application/json", body: func() io.Reader { return strings.NewReader(`{"limit":"two"}`) },
		category: httpapi.ErrMalformed, status: http.StatusBadRequest,
		message: `malformed request: "limit" must be a number`,
	},
	{
		label: "that can not be read", contentType: "application/json", body: func() io.Reader { return failingReader{} },
		category: httpapi.ErrMalformed, status: http.StatusBadRequest,
		message: "malformed request: the body could not be read",
	},
}

func TestQueryBody(t *testing.T) {
	for kind, options := range queryKinds {
		for _, failure := range bodyFailures {
			t.Run(kind+" answers a body "+failure.label+" as for a command, without building or answering the query", func(t *testing.T) {
				var isWaitedFor, isBuilt, isAnswered atomic.Bool
				mux := queried(t, recording(&isBuilt, toListNotesOf), answering(&isAnswered, answerListNotes), options(&isWaitedFor)...)

				request := asking("QUERY", "golo", failure.contentType, failure.body())
				request.Header.Set(httpapi.HeaderWaitFor, "4")
				response := serve(t, mux, request)

				assert.Equal(t, failure.status, response.Code)
				assert.JSONEq(t, messageOf(t, failure.message), response.Body.String())
				assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
				assert.Empty(t, response.Header().Get("ETag"), "a failure was tagged")
				assert.Empty(t, response.Header().Get(httpapi.HeaderRevision), "a failure carries a revision")

				assert.False(t, isBuilt.Load(), "the query must not be built from a body that does not fit")
				assert.False(t, isWaitedFor.Load(), "nothing must wait for a query that was never built")
				assert.False(t, isAnswered.Load(), "a query that was never built must not be answered")
			})
		}

		t.Run(kind+" determines the caller before it reads the body", func(t *testing.T) {
			var isBuilt atomic.Bool
			mux := queried(t, recording(&isBuilt, toListNotesOf), answerListNotes, options(&atomic.Bool{})...)

			response := serve(t, mux, asking("QUERY", "", "application/json", failingReader{}))

			assert.Equal(t, http.StatusUnauthorized, response.Code, "an unknown caller has to be refused before the body is read")
			assert.False(t, isBuilt.Load())
		})
	}

	for _, failure := range bodyFailures {
		t.Run("Ask returns the error of a body "+failure.label+", without building or answering the query", func(t *testing.T) {
			var isBuilt, isAnswered atomic.Bool

			_, err := httpapi.Ask(asking("QUERY", "golo", failure.contentType, failure.body()), httpapi.NewAPI(deadStore(t), userFrom),
				recording(&isBuilt, toListNotesOf), answering(&isAnswered, answerListNotes))

			require.ErrorIs(t, err, failure.category)
			assert.EqualError(t, err, failure.message)
			assert.Equal(t, failure.status, httpapi.StatusFor(err))
			assert.False(t, isBuilt.Load(), "the query must not be built from a body that does not fit")
			assert.False(t, isAnswered.Load(), "a query that was never built must not be answered")
		})
	}

	t.Run("Ask determines the caller before it reads the body", func(t *testing.T) {
		var isBuilt atomic.Bool

		_, err := httpapi.Ask(asking("QUERY", "", "application/json", failingReader{}), httpapi.NewAPI(deadStore(t), userFrom),
			recording(&isBuilt, toListNotesOf), answerListNotes)

		require.ErrorIs(t, err, httpapi.ErrUnauthorized, "an unknown caller has to be refused before the body is read")
		assert.NotErrorIs(t, err, errBrokenBody)
		assert.False(t, isBuilt.Load())
	})

	t.Run("Ask decodes the body by the rules of BodyOf in a handler of your own", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)
		mux := http.NewServeMux()
		mux.HandleFunc("QUERY /notes", func(w http.ResponseWriter, r *http.Request) {
			notes, err := httpapi.Ask(r, api, toListNotesOf, answerListNotes)
			httpapi.RespondResult(w, r, api, notes, err)
		})

		response := serve(t, mux, asking("QUERY", "golo", "application/json", strings.NewReader(`{"limit":"two"}`)))

		assert.Equal(t, http.StatusBadRequest, response.Code)
		assert.JSONEq(t, messageOf(t, `malformed request: "limit" must be a number`), response.Body.String())
	})
}

// toAllNotes builds the query that lists every note, from a request without a
// body.
func toAllNotes(*http.Request, httpapi.NoBody, user) (listNotes, error) {
	return listNotes{}, nil
}

func TestQueryWithoutABody(t *testing.T) {
	for kind, options := range queryKinds {
		t.Run(kind+" without input accepts a body that is empty, or {}, whatever the Content-Type says", func(t *testing.T) {
			for label, test := range map[string]struct {
				body, contentType string
			}{
				"no body":                        {body: "", contentType: ""},
				"an empty body, said to be JSON": {body: "", contentType: "application/json"},
				"{}":                             {body: "{}", contentType: "application/json"},
				"{}, without a Content-Type":     {body: "{}", contentType: ""},
				"{}, as plain text":              {body: "{}", contentType: "text/plain"},
			} {
				t.Run(label, func(t *testing.T) {
					mux := queried(t, toAllNotes, answerListNotes, options(&atomic.Bool{})...)

					response := serve(t, mux, asking("QUERY", "golo", test.contentType, strings.NewReader(test.body)))

					require.Equal(t, http.StatusOK, response.Code, response.Body.String())
					assert.Equal(t, []string{"first", "second", "third"}, notesIn(t, response))
				})
			}
		})

		t.Run(kind+" without input refuses any other body, and says that it takes none", func(t *testing.T) {
			var isBuilt atomic.Bool
			mux := queried(t, recording(&isBuilt, toAllNotes), answerListNotes, options(&atomic.Bool{})...)

			for _, body := range []string{`{"limit":2}`, `null`, `[]`, "not json"} {
				response := serve(t, mux, asking("QUERY", "golo", "application/json", strings.NewReader(body)))

				assert.Equal(t, http.StatusBadRequest, response.Code, "with %s", body)
				assert.JSONEq(t, messageOf(t, "malformed request: this route takes no body, so the body has to be empty, or {}"),
					response.Body.String(), "with %s", body)
			}

			assert.False(t, isBuilt.Load(), "the query must not be built from a request with a body")
		})

		t.Run(kind+" without input refuses a body over the limit", func(t *testing.T) {
			mux := queried(t, toAllNotes, answerListNotes, options(&atomic.Bool{})...)

			response := serve(t, mux, asking("QUERY", "golo", "", strings.NewReader(strings.Repeat(" ", httpapi.MaxRequestBody+1))))

			assert.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
		})

		for _, origin := range crossOrigins {
			t.Run(kind+" without input refuses "+origin.label, func(t *testing.T) {
				var isBuilt, isWaitedFor atomic.Bool
				mux := queried(t, recording(&isBuilt, toAllNotes), answerListNotes, options(&isWaitedFor)...)

				request := fromOrigin(asking("QUERY", "golo", "", http.NoBody), origin.secFetchSite, origin.origin)
				request.Header.Set(httpapi.HeaderWaitFor, "4")
				response := serve(t, mux, request)

				assert.Equal(t, http.StatusForbidden, response.Code)
				assert.JSONEq(t, messageOf(t, fromAnotherOrigin), response.Body.String())
				assert.False(t, isBuilt.Load(), "the query must not be built for a request from another origin")
				assert.False(t, isWaitedFor.Load(), "nothing must wait for a request from another origin")
			})
		}

		for _, origin := range sameOrigins {
			t.Run(kind+" without input accepts "+origin.label, func(t *testing.T) {
				mux := queried(t, toAllNotes, answerListNotes, options(&atomic.Bool{})...)

				response := serve(t, mux, fromOrigin(asking("QUERY", "golo", "", http.NoBody), origin.secFetchSite, origin.origin))

				assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
			})
		}

		t.Run(kind+" with input accepts another origin, since it requires JSON", func(t *testing.T) {
			mux := queried(t, toListNotesOf, answerListNotes, options(&atomic.Bool{})...)

			response := serve(t, mux, fromOrigin(asking("QUERY", "golo", "application/json", strings.NewReader(`{"limit":1}`)), "cross-site", ""))

			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Equal(t, []string{"first"}, notesIn(t, response))
		})
	}

	t.Run("Ask without input refuses a query from another origin", func(t *testing.T) {
		request := fromOrigin(asking("QUERY", "golo", "", http.NoBody), "cross-site", "")

		_, err := httpapi.Ask(request, httpapi.NewAPI(deadStore(t), userFrom), toAllNotes, answerListNotes)

		require.ErrorIs(t, err, httpapi.ErrForbidden)
		assert.EqualError(t, err, fromAnotherOrigin)
	})

	t.Run("Ask without input lets GET pass from any origin, since it must not change anything", func(t *testing.T) {
		// A download is asked with GET, from a link that a page on another site
		// may hold, and carries no body.
		for _, origin := range slices.Concat(crossOrigins, sameOrigins) {
			t.Run(origin.label, func(t *testing.T) {
				request := fromOrigin(asking(http.MethodGet, "golo", "", nil), origin.secFetchSite, origin.origin)

				notes, err := httpapi.Ask(request, httpapi.NewAPI(deadStore(t), userFrom), toAllNotes, answerListNotes)

				require.NoError(t, err)
				assert.Len(t, notes, 3)
			})
		}
	})
}
