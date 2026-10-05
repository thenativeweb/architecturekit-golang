package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func toListNotes(r *http.Request, _ httpapi.NoBody, _ user) (listNotes, error) {
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
	httpapi.Query(api, mux, "QUERY /notes", toListNotes, answerListNotes)

	return mux
}

func ask(t *testing.T, mux *http.ServeMux, path, asUser string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest("QUERY", path, nil)
	if asUser != "" {
		request.Header.Set("X-User", asUser)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	return recorder
}

func TestQuery(t *testing.T) {
	t.Run("answers with the result", func(t *testing.T) {
		response := ask(t, queryMux(t), "/notes", "golo")

		assert.Equal(t, http.StatusOK, response.Code)
		assert.Equal(t, "application/json", response.Header().Get("Content-Type"))

		var notes []noteResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &notes))
		assert.Len(t, notes, 3)
	})

	t.Run("passes request parameters through", func(t *testing.T) {
		response := ask(t, queryMux(t), "/notes?limit=2", "golo")

		var notes []noteResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &notes))
		assert.Len(t, notes, 2)
	})

	t.Run("needs a user", func(t *testing.T) {
		response := ask(t, queryMux(t), "/notes", "")

		assert.Equal(t, http.StatusUnauthorized, response.Code)
	})

	t.Run("rejects unusable parameters", func(t *testing.T) {
		response := ask(t, queryMux(t), "/notes?limit=banana", "golo")

		assert.Equal(t, http.StatusBadRequest, response.Code)
		assert.Contains(t, response.Body.String(), "limit must be a number")
	})

	t.Run("without items is not found", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)
		mux := http.NewServeMux()

		// A single-item lookup that finds nothing reports query.ErrNoItems, which
		// the HTTP layer turns into 404 without the application saying so.
		httpapi.Query(api, mux, "QUERY /notes/{id}", toListNotes,
			func(context.Context, listNotes) (noteResponse, error) {
				return noteResponse{}, query.ErrNoItems
			})

		response := ask(t, mux, "/notes/7", "golo")

		assert.Equal(t, http.StatusNotFound, response.Code)
		assert.JSONEq(t, `{"message": "not found"}`, response.Body.String(), "the text of the package query is not for the caller")
	})

	t.Run("ErrForbidden survives the way out", func(t *testing.T) {
		// An application that refuses in ToQuery keeps its 403 instead of having
		// it turned into a 400.
		api := httpapi.NewAPI(deadStore(t), userFrom)
		mux := http.NewServeMux()

		httpapi.Query(api, mux, "QUERY /restricted",
			func(*http.Request, httpapi.NoBody, user) (listNotes, error) {
				return listNotes{}, errors.Join(httpapi.ErrForbidden, errors.New("not for you"))
			},
			answerListNotes)

		response := ask(t, mux, "/restricted", "golo")

		assert.Equal(t, http.StatusForbidden, response.Code)
	})

	t.Run("domain errors from the query side keep their status", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)
		mux := http.NewServeMux()

		httpapi.Query(api, mux, "QUERY /rule",
			func(*http.Request, httpapi.NoBody, user) (listNotes, error) {
				return listNotes{}, architecturekit.NewDomainError("that combination makes no sense")
			},
			answerListNotes)

		response := ask(t, mux, "/rule", "golo")

		assert.Equal(t, http.StatusUnprocessableEntity, response.Code)
	})

	var (
		noAPI     *httpapi.API[user]
		toNothing httpapi.ToQuery[user, httpapi.NoBody, listNotes]
		noAnswer  httpapi.Answer[listNotes, []noteResponse]
	)

	for kind, options := range map[string][]httpapi.QueryOption{
		"query":            nil,
		"revisioned query": {httpapi.Revisioned(noteView(), time.Second)},
	} {
		t.Run("a "+kind+" panics for a nil API", func(t *testing.T) {
			// Without the panic, every request panics again while the panic is
			// answered, and net/http closes the connection without an answer.
			assert.PanicsWithValue(t, "architecturekit/httpapi: Query needs the API, not nil", func() {
				httpapi.Query(noAPI, http.NewServeMux(), "QUERY /notes", toListNotes, answerListNotes, options...)
			})
		})

		t.Run("a "+kind+" panics for a nil function that turns the request into a query", func(t *testing.T) {
			assert.PanicsWithValue(t, "architecturekit/httpapi: Query needs a function that turns the request into a query, not nil", func() {
				httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom), http.NewServeMux(), "QUERY /notes", toNothing, answerListNotes, options...)
			})
		})

		t.Run("a "+kind+" panics for a nil function that answers the query", func(t *testing.T) {
			assert.PanicsWithValue(t, "architecturekit/httpapi: Query needs a function that answers the query, not nil", func() {
				httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom), http.NewServeMux(), "QUERY /notes", toListNotes, noAnswer, options...)
			})
		})
	}
}

func TestQueryRefusals(t *testing.T) {
	// The answer fails with a conflict, which a query rarely does, but which
	// names a subject all the same.
	conflicting := func(context.Context, listNotes) ([]noteResponse, error) {
		return nil, fmt.Errorf("%w: writing %q", architecturekit.ErrConflict, "/tenants/acme-bank/books/42")
	}

	for name, options := range map[string][]httpapi.QueryOption{
		"a query":            nil,
		"a revisioned query": {httpapi.Revisioned(noteView(), time.Second)},
	} {
		wire := func(logs *bytes.Buffer, answer httpapi.Answer[listNotes, []noteResponse]) *http.ServeMux {
			api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(logs)))
			mux := http.NewServeMux()
			httpapi.Query(api, mux, "QUERY /notes", toListNotes, answer, options...)

			return mux
		}

		t.Run(name+" answers 401 with a fixed text, and logs the details", func(t *testing.T) {
			var logs bytes.Buffer

			response := ask(t, wire(&logs, answerListNotes), "/notes", "")

			assert.Equal(t, http.StatusUnauthorized, response.Code)
			assert.JSONEq(t, `{"message": "unauthorized"}`, response.Body.String())
			assertRefusalLogged(t, logs.String(), "QUERY", "QUERY /notes", http.StatusUnauthorized, "no user given")
		})

		t.Run(name+" answers 409 with a fixed text, and logs the details", func(t *testing.T) {
			var logs bytes.Buffer

			response := ask(t, wire(&logs, conflicting), "/notes", "golo")

			assert.Equal(t, http.StatusConflict, response.Code)
			assert.JSONEq(t, `{"message": "conflict: the data has changed since it was read"}`, response.Body.String())
			assertRefusalLogged(t, logs.String(), "QUERY", "QUERY /notes", http.StatusConflict, "/tenants/acme-bank/books/42")
		})
	}
}

func TestAsk(t *testing.T) {
	t.Run("returns the result without writing", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)

		request := httptest.NewRequest(http.MethodGet, "/notes", nil)
		request.Header.Set("X-User", "golo")

		notes, err := httpapi.Ask(request, api, toListNotes, answerListNotes)
		require.NoError(t, err)
		assert.Len(t, notes, 3)
	})

	t.Run("reports a failing answer", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)

		request := httptest.NewRequest(http.MethodGet, "/notes", nil)
		request.Header.Set("X-User", "golo")

		_, err := httpapi.Ask(request, api, toListNotes,
			func(context.Context, listNotes) ([]noteResponse, error) {
				return nil, errors.New("the view is unavailable")
			})

		assert.Error(t, err, "expected the error from the answer")
	})
}

func TestPublicAPI(t *testing.T) {
	t.Run("serves everyone", func(t *testing.T) {
		api := httpapi.NewPublicAPI(deadStore(t))
		mux := http.NewServeMux()

		httpapi.Query(api, mux, "QUERY /public",
			func(*http.Request, httpapi.NoBody, httpapi.NoUser) (listNotes, error) {
				return listNotes{}, nil
			},
			answerListNotes)

		// No user header at all, and it is served anyway.
		response := ask(t, mux, "/public", "")

		assert.Equal(t, http.StatusOK, response.Code)
	})
}

func TestRespondResult(t *testing.T) {
	t.Run("writes the result", func(t *testing.T) {
		var logs bytes.Buffer
		request, api := inAHandler(&logs)
		recorder := httptest.NewRecorder()

		httpapi.RespondResult(recorder, request, api, []noteResponse{{Text: "only"}}, nil)

		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.Contains(t, recorder.Body.String(), "only")
	})

	t.Run("answers an empty result with an empty list", func(t *testing.T) {
		var logs bytes.Buffer
		request, api := inAHandler(&logs)
		recorder := httptest.NewRecorder()

		// query.Collect, which the kit suggests for turning items into a slice,
		// returns nil when there are no items.
		notes, err := query.Collect(noteView().All(context.Background()))
		require.NoError(t, err)
		require.Nil(t, notes)

		httpapi.RespondResult(recorder, request, api, notes, nil)

		assert.Equal(t, "[]", strings.TrimSpace(recorder.Body.String()))
	})

	t.Run("explains failures the caller can fix", func(t *testing.T) {
		var logs bytes.Buffer
		request, api := inAHandler(&logs)
		recorder := httptest.NewRecorder()

		httpapi.RespondResult(recorder, request, api, []noteResponse(nil),
			errors.Join(httpapi.ErrNotFound, errors.New("note 7 is unknown")))

		assert.Equal(t, http.StatusNotFound, recorder.Code)
		assert.Contains(t, recorder.Body.String(), "note 7 is unknown")
	})

	t.Run("keeps internal failures to itself", func(t *testing.T) {
		var logs bytes.Buffer
		request, api := inAHandler(&logs)
		recorder := httptest.NewRecorder()

		httpapi.RespondResult(recorder, request, api, []noteResponse(nil), errors.New("the password is hunter2"))

		assert.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.NotContains(t, recorder.Body.String(), "hunter2", "an internal failure must not be explained")
	})

	t.Run("logs internal failures once, through the logger of the API, with the route", func(t *testing.T) {
		var logs bytes.Buffer
		request, api := inAHandler(&logs)

		defaults := logsOf(func() {
			httpapi.RespondResult(httptest.NewRecorder(), request, api, []noteResponse(nil), errors.New("the view is gone"))
		})

		assert.Equal(t, 1, strings.Count(logs.String(), "httpapi: internal failure"))
		assert.Contains(t, logs.String(), "the view is gone", "an internal failure must be logged")
		assert.Contains(t, logs.String(), `route="GET /notes"`)
		assert.Empty(t, defaults, "nothing must go to the default logger as well")
	})

	t.Run("does not log failures the caller can fix", func(t *testing.T) {
		var logs bytes.Buffer
		request, api := inAHandler(&logs)

		httpapi.RespondResult(httptest.NewRecorder(), request, api, []noteResponse(nil), httpapi.ErrNotFound)

		assert.Empty(t, logs.String(), "a failure the caller can fix must not be logged")
	})
}

func TestResultsThatCanNotBeEncoded(t *testing.T) {
	// A result that can not be encoded is a mistake in the code, so it is
	// answered like any other internal failure, rather than with 200 and an
	// empty body, and whatever category the error of encoding it claims.
	results := map[string]httpapi.Answer[countNotes, any]{
		"NaN, which JSON has no number for": func(context.Context, countNotes) (any, error) {
			return []float64{math.NaN()}, nil
		},
		"an error with a category": func(context.Context, countNotes) (any, error) {
			return unencodable{}, nil
		},
	}

	for name, options := range map[string][]httpapi.QueryOption{
		"a query":            nil,
		"a revisioned query": {httpapi.Revisioned(seenView("4"), time.Second)},
		"an awaiting query":  {httpapi.Awaiting(seenView("4"), time.Second)},
	} {
		for result, answer := range results {
			t.Run(name+" answers a result with "+result+" with 500, and logs why", func(t *testing.T) {
				var logs bytes.Buffer
				api := httpapi.NewAPI(deadStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
				mux := http.NewServeMux()
				httpapi.Query(api, mux, "QUERY /notes", allNotes, answer, options...)

				response := ask(t, mux, "/notes", "golo")

				assert.Equal(t, http.StatusInternalServerError, response.Code)
				assert.JSONEq(t, `{"message": "internal server error"}`, response.Body.String())
				assert.Empty(t, response.Header().Get("ETag"), "an answer that never came was tagged")
				assert.Empty(t, response.Header().Get(httpapi.HeaderRevision), "an answer that never came carries a revision")
				assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))

				assert.Equal(t, 1, strings.Count(logs.String(), "\n"), "want exactly one entry")
				assert.Contains(t, logs.String(), `level=ERROR msg="httpapi: internal failure"`)
				assert.Contains(t, logs.String(), "method=QUERY")
				assert.Contains(t, logs.String(), `route="QUERY /notes"`)
				assert.Contains(t, logs.String(), "httpapi: encoding the result")
			})
		}
	}

	t.Run("RespondResult answers a result with NaN with 500, and logs why", func(t *testing.T) {
		var logs bytes.Buffer
		request, api := inAHandler(&logs)
		recorder := httptest.NewRecorder()

		httpapi.RespondResult(recorder, request, api, []float64{math.NaN()}, nil)

		assert.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.JSONEq(t, `{"message": "internal server error"}`, recorder.Body.String())
		assert.Contains(t, logs.String(), `level=ERROR msg="httpapi: internal failure"`)
		assert.Contains(t, logs.String(), "unsupported value: NaN")
	})
}

// seenView is a view of notes that has seen the given revision.
func seenView(revision string) *architecturekit.InMemoryView[string, noteItem] {
	view := noteView()
	view.Seen(revision)

	return view
}

// A query is asked with QUERY, a method that asks with a body and changes
// nothing (RFC 10008).

func TestQueryMethod(t *testing.T) {
	for kind, options := range map[string]func() []httpapi.QueryOption{
		"a query": func() []httpapi.QueryOption { return nil },
		"a revisioned query": func() []httpapi.QueryOption {
			return []httpapi.QueryOption{httpapi.Revisioned(seenView("4"), time.Second)}
		},
		"an awaiting query": func() []httpapi.QueryOption {
			return []httpapi.QueryOption{httpapi.Awaiting(seenView("4"), time.Second)}
		},
	} {
		wire := func(pattern string) func() {
			return func() {
				httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom), http.NewServeMux(), pattern, allNotes, countNotesIn(noteView()), options()...)
			}
		}

		t.Run(kind+" panics for a pattern without a method, which accepts every method", func(t *testing.T) {
			// A space in front of the path leaves the method empty, as the mux
			// reads it.
			for _, pattern := range []string{"/notes", "example.com/notes", " /notes"} {
				t.Run(pattern, func(t *testing.T) {
					assert.PanicsWithValue(t,
						fmt.Sprintf("architecturekit/httpapi: Query needs a pattern that names the method QUERY, not %q, "+
							"which accepts every method", pattern),
						wire(pattern))
				})
			}
		})

		t.Run(kind+" panics for a pattern with another method", func(t *testing.T) {
			for _, test := range []struct {
				pattern, method string
			}{
				{pattern: "GET /notes", method: http.MethodGet},
				{pattern: "HEAD /notes", method: http.MethodHead},
				{pattern: "POST /notes", method: http.MethodPost},
				{pattern: "OPTIONS /notes", method: http.MethodOptions},
				{pattern: "GET example.com/notes", method: http.MethodGet},
				// The mux takes a tab for a space.
				{pattern: "GET\t/notes", method: http.MethodGet},
				// The mux tells methods apart by case.
				{pattern: "query /notes", method: "query"},
			} {
				t.Run(test.pattern, func(t *testing.T) {
					assert.PanicsWithValue(t,
						fmt.Sprintf("architecturekit/httpapi: Query needs a pattern whose method is QUERY, not %q, "+
							"since a query is asked with QUERY rather than %s", test.pattern, test.method),
						wire(test.pattern))
				})
			}
		})

		t.Run(kind+" panics for a nil function first, as for every other mistake in the wiring", func(t *testing.T) {
			var toNothing httpapi.ToQuery[user, httpapi.NoBody, countNotes]

			assert.PanicsWithValue(t, "architecturekit/httpapi: Query needs a function that turns the request into a query, not nil", func() {
				httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom), http.NewServeMux(), "GET /notes", toNothing, countNotesIn(noteView()), options()...)
			})
		})

		t.Run(kind+" answers a pattern with QUERY", func(t *testing.T) {
			for _, pattern := range []string{"QUERY /notes", "QUERY example.com/notes", "QUERY\t/notes"} {
				t.Run(pattern, func(t *testing.T) {
					mux := http.NewServeMux()
					httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom), mux, pattern, allNotes, countNotesIn(noteView()), options()...)

					response := askNotes(mux, nil)

					assert.Equal(t, http.StatusOK, response.Code)
					assert.Equal(t, "0\n", response.Body.String())
				})
			}
		})

		t.Run(kind+" answers no other method", func(t *testing.T) {
			isAsked := false
			mux := http.NewServeMux()
			httpapi.Query(httpapi.NewAPI(deadStore(t), userFrom), mux, "QUERY /notes", allNotes,
				func(context.Context, countNotes) (int, error) {
					isAsked = true
					return 0, nil
				}, options()...)

			for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodOptions} {
				t.Run(method, func(t *testing.T) {
					request := httptest.NewRequest(method, "/notes", nil)
					request.Header.Set("X-User", "golo")
					response := serve(t, mux, request)

					assert.Equal(t, http.StatusMethodNotAllowed, response.Code)
					assert.Equal(t, "QUERY", response.Header().Get("Allow"))
				})
			}

			assert.False(t, isAsked, "the query must only be asked with QUERY")
		})
	}

	t.Run("a handler of your own asks with any method, such as GET for a download", func(t *testing.T) {
		api := httpapi.NewAPI(deadStore(t), userFrom)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /notes/export", func(w http.ResponseWriter, r *http.Request) {
			notes, err := httpapi.Ask(r, api, toListNotes, answerListNotes)
			if err != nil {
				httpapi.RespondError(w, r, api, err)
				return
			}

			w.Header().Set("Content-Type", "text/plain")
			for _, note := range notes {
				_, _ = fmt.Fprintln(w, note.Text)
			}
		})

		request := httptest.NewRequest(http.MethodGet, "/notes/export?limit=2", nil)
		request.Header.Set("X-User", "golo")
		response := serve(t, mux, request)

		assert.Equal(t, http.StatusOK, response.Code)
		assert.Equal(t, "first\nsecond\n", response.Body.String())
	})
}
