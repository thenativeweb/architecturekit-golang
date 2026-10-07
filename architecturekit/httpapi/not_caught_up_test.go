package httpapi_test

import (
	"bytes"
	"context"
	"errors"
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

// notCaughtUp returns what WaitForWritten returns once the time has run out,
// as a step that waits for a write of its own does.
func notCaughtUp(t *testing.T) error {
	t.Helper()

	view := architecturekit.NewInMemoryView(func(item string) string { return item })
	err := architecturekit.WaitForWritten(context.Background(), view,
		[]eventsourcingdb.Event{{ID: "7", Subject: "/sessions/23"}}, time.Millisecond)
	require.ErrorIs(t, err, architecturekit.ErrNotCaughtUp)

	return err
}

// failingView is a view whose WaitFor fails with the given error.
type failingView struct {
	err error
}

func (failingView) Revision(context.Context) (string, error) { return "", nil }

func (v failingView) WaitFor(context.Context, string) error { return v.err }

func TestNotCaughtUpAfterTheCommand(t *testing.T) {
	written := []eventsourcingdb.Event{{ID: "7", Subject: "/instances/x"}}
	emptyView := func() architecturekit.Revisioned {
		return architecturekit.NewInMemoryView(func(item string) string { return item })
	}

	// Whatever keeps the view from catching up, the write has succeeded, so
	// the caller must neither be invited to try again nor be told that the
	// request failed.
	for name, wait := range map[string]func(t *testing.T) error{
		"once the time has run out": func(*testing.T) error {
			return architecturekit.WaitForWritten(context.Background(), emptyView(), written, time.Millisecond)
		},
		"once the deadline of the request has run out first": func(t *testing.T) error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
			t.Cleanup(cancel)

			return architecturekit.WaitForWritten(ctx, emptyView(), written, httpapi.DefaultWait)
		},
		"once the caller went away": func(t *testing.T) error {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			return architecturekit.WaitForWritten(ctx, emptyView(), written, httpapi.DefaultWait)
		},
		"once the view failed for now": func(*testing.T) error {
			failure := errors.New("polling the revision: connection refused")
			return architecturekit.WaitForWritten(context.Background(),
				failingView{err: errors.Join(architecturekit.ErrTransient, failure)}, written, time.Second)
		},
		"once the view failed for good": func(*testing.T) error {
			return architecturekit.WaitForWritten(context.Background(),
				failingView{err: architecturekit.ErrPermanent}, written, time.Second)
		},
	} {
		for answerer, answer := range answerers {
			t.Run(answerer+" answers a view that did not catch up "+name+" with 500 and the text that the request succeeded", func(t *testing.T) {
				err := wait(t)
				require.ErrorIs(t, err, architecturekit.ErrNotCaughtUp)

				var logs bytes.Buffer
				request, api := inAHandler(&logs)
				response := httptest.NewRecorder()

				answer(response, request, api, err)

				assert.Equal(t, http.StatusInternalServerError, response.Code, "neither 503 nor 499 fit a write that has succeeded")
				assert.JSONEq(t, `{"message": "the request succeeded, but its result is not visible yet"}`, response.Body.String())
				assert.Contains(t, logs.String(), `level=ERROR msg="httpapi: internal failure"`)
				assert.Contains(t, logs.String(), "the events were written", "the details have to reach the log")
			})
		}
	}

	t.Run("a handler of your own that waits after the command answers it as a success that is not visible yet", func(t *testing.T) {
		var logs bytes.Buffer
		api := httpapi.NewAPI(writingStore(t), userFrom, httpapi.WithLogger(loggerInto(&logs)))
		view := architecturekit.NewInMemoryView(func(item string) string { return item })

		mux := http.NewServeMux()
		mux.HandleFunc("POST /note", func(w http.ResponseWriter, r *http.Request) {
			handled, err := httpapi.Handle(r, api, toNote, noteDecider())
			if err == nil {
				err = architecturekit.WaitForWritten(r.Context(), view, handled.Events, time.Millisecond)
			}

			httpapi.Respond(w, r, api, handled.Events, err)
		})

		request := postingTo("/note", `{"id":"1","text":"hello"}`)
		request.Pattern = "POST /note"
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)

		assert.Equal(t, http.StatusInternalServerError, response.Code)
		assert.JSONEq(t, `{"message": "the request succeeded, but its result is not visible yet"}`, response.Body.String())
		assert.Contains(t, logs.String(), `level=ERROR msg="httpapi: internal failure"`)
		assert.Contains(t, logs.String(), "did not catch up within 1ms")
	})
}
