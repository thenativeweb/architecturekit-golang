package architecturekit_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/v2/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/v2/eventsourcingdb"
)

// Once the request of a write has left, the database may store the events at
// any moment, so whatever goes wrong afterwards leaves open whether it did.
// Only a write that certainly stored nothing keeps its category, so that
// ErrTransient never invites trying again in a way that stores the events
// twice.

// remark is an event of a size the test chooses, such as one too large to be
// written completely before the database hangs up.
type remark struct {
	Text string `json:"text"`
}

func (remark) EventType() string { return "io.thenativeweb.test.remark" }

// kitWrites are the two ways the kit writes, which fail alike. Each of them
// writes the given event to the subject /test, Execute with a decider that
// decides on it, whatever the command. doing is how the message names the
// write.
var kitWrites = []struct {
	name  string
	doing string
	write func(ctx context.Context, store *architecturekit.Store, event architecturekit.Event) error
}{
	{"Execute", `writing "/test"`, func(ctx context.Context, store *architecturekit.Store, event architecturekit.Event) error {
		_, err := architecturekit.Execute(ctx, store, emittingDecider(counterState().Ignore[remark](), event),
			increment{subject: "/test", By: 1})

		return err
	}},
	{"Write", `writing 1 events, the first to "/test"`, func(ctx context.Context, store *architecturekit.Store, event architecturekit.Event) error {
		_, err := architecturekit.Write(ctx, store,
			[]architecturekit.EventOn{{Subject: "/test", Event: event}}, architecturekit.Unconditionally())

		return err
	}},
}

// assertOutcomeUnknown asserts that a write failed with ErrOutcomeUnknown,
// which belongs to no category, and is not the end of the context either.
func assertOutcomeUnknown(t *testing.T, err error) {
	t.Helper()

	assert.ErrorIs(t, err, architecturekit.ErrOutcomeUnknown)
	for _, other := range []error{
		architecturekit.ErrDomain, architecturekit.ErrTransient, architecturekit.ErrPermanent,
		context.Canceled, context.DeadlineExceeded,
	} {
		assert.NotErrorIs(t, err, other, "an unknown outcome is no %v", other)
	}
}

// answering answers with the given status and reason, as the database does
// when it refuses a request.
func answering(status int, reason string) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(status)
		_, _ = fmt.Fprint(writer, reason)
	}
}

// fromAProxy answers with the given status, without saying that the answer
// comes from an EventSourcingDB, as a proxy in front of the database does.
func fromAProxy(status int) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Del("Server")
		writer.WriteHeader(status)
	}
}

func TestAFailedWrite(t *testing.T) {
	t.Run("keeps its category if the database can not be reached", func(t *testing.T) {
		for _, write := range kitWrites {
			t.Run(write.name, func(t *testing.T) {
				// Execute reads before it writes, so its database has to answer
				// the read, and go away before the write.
				client := deadClient(t)
				if write.name == "Execute" {
					client = vanishingDatabase(t)
				}
				store := architecturekit.NewStore(client, "https://thenativeweb.io")

				err := write.write(context.Background(), store, incremented{By: 1})

				assert.ErrorIs(t, err, architecturekit.ErrTransient, "trying again can not store the events twice")
				assert.NotErrorIs(t, err, architecturekit.ErrOutcomeUnknown, "the database never got the request")
				assert.ErrorContains(t, err, "transient failure: "+write.doing+": ")
				assert.ErrorContains(t, err, "connection refused")
			})
		}
	})

	t.Run("leaves the outcome unknown if the database hung up while the request was sent, as the client does", func(t *testing.T) {
		// The client reports a write as certainly not stored only if it could
		// not connect, by the same rule in every SDK, so a request that was cut
		// off after connecting, but before all of it was sent, leaves the
		// outcome open, although the database can not have stored it.
		for _, write := range kitWrites {
			t.Run(write.name, func(t *testing.T) {
				store := architecturekit.NewStore(hangingUpDatabase(t), "https://thenativeweb.io")

				err := write.write(context.Background(), store, remark{Text: strings.Repeat("x", 16<<20)})

				assertOutcomeUnknown(t, err)
			})
		}
	})

	t.Run("keeps its category if the database refused it before writing", func(t *testing.T) {
		refusals := []struct {
			name   string
			status int
			reason string
			want   error
		}{
			{"a bad request is permanent", http.StatusBadRequest, "bad request", architecturekit.ErrPermanent},
			{"a rejected API token is permanent", http.StatusUnauthorized, "unauthorized", architecturekit.ErrPermanent},
			{"a forbidden request is permanent", http.StatusForbidden, "forbidden", architecturekit.ErrPermanent},
			{"an unknown route is permanent", http.StatusNotFound, "not found", architecturekit.ErrPermanent},
			{"a failed precondition is a conflict", http.StatusConflict, "state conflict: precondition failed", architecturekit.ErrConflict},
			{"a schema violation is permanent", http.StatusConflict, "schema conflict: event does not match", architecturekit.ErrPermanent},
			{"a request that is too large is permanent", http.StatusRequestEntityTooLarge, "too large", architecturekit.ErrPermanent},
			{"a media type that is not supported is permanent", http.StatusUnsupportedMediaType, "unsupported", architecturekit.ErrPermanent},
			{"a request that can not be processed is permanent", http.StatusUnprocessableEntity, "unprocessable", architecturekit.ErrPermanent},
			{"too many requests are transient", http.StatusTooManyRequests, "slow down", architecturekit.ErrTransient},
			{"a database that shuts down is transient", http.StatusServiceUnavailable, "server is shutting down", architecturekit.ErrTransient},
			{"a database without a valid license is transient", http.StatusInsufficientStorage, "insufficient storage", architecturekit.ErrTransient},
		}

		for _, write := range kitWrites {
			for _, refusal := range refusals {
				t.Run(write.name+": "+refusal.name, func(t *testing.T) {
					store := architecturekit.NewStore(
						writingDatabase(t, "/api/v1/write-events", answering(refusal.status, refusal.reason)), "https://thenativeweb.io")

					err := write.write(context.Background(), store, incremented{By: 1})

					assert.ErrorIs(t, err, refusal.want)
					assert.NotErrorIs(t, err, otherCategoryThan(refusal.want))
					assert.NotErrorIs(t, err, architecturekit.ErrOutcomeUnknown, "the database refused the write before writing")

					answer, isAnswer := errors.AsType[*eventsourcingdb.DBAPIError](err)
					require.True(t, isAnswer, "errors.As has to reach the answer of the database, got: %v", err)
					assert.Equal(t, refusal.status, answer.Status)
					assert.Equal(t, refusal.reason, answer.Description)
				})
			}
		}
	})

	t.Run("leaves the outcome unknown if the request has left, but no answer says that nothing was written", func(t *testing.T) {
		failures := []struct {
			name   string
			answer http.HandlerFunc
			detail string
		}{
			{"the connection breaks", func(http.ResponseWriter, *http.Request) {
				panic(http.ErrAbortHandler)
			}, "EOF"},
			{"the answer is cut off", func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Length", strconv.Itoa(len(writtenAnswer)))
				_, _ = fmt.Fprint(writer, writtenAnswer[:len(writtenAnswer)/2])
				writer.(http.Flusher).Flush()
				panic(http.ErrAbortHandler)
			}, "unexpected EOF"},
			{"the answer can not be decoded", func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(writer, `{"these are":"no events"}`)
			}, "cannot unmarshal object"},
			{"the database fails with 500, which may follow storing the events", answering(http.StatusInternalServerError,
				"failed to write events: failed to flush the write-ahead log"), "failed to flush the write-ahead log"},
			{"the answer is 502, which the database never sends", answering(http.StatusBadGateway, "bad gateway"), "bad gateway"},
			{"the answer is 504, which the database never sends", answering(http.StatusGatewayTimeout, "gateway timeout"), "gateway timeout"},
			{"the answer does not come from an EventSourcingDB, such as a 502 of a proxy", fromAProxy(http.StatusBadGateway),
				"the answer does not come from an EventSourcingDB: "},
			{"the answer does not come from an EventSourcingDB, such as a 504 of a proxy", fromAProxy(http.StatusGatewayTimeout),
				"the answer does not come from an EventSourcingDB: "},
		}

		for _, write := range kitWrites {
			for _, failure := range failures {
				t.Run(write.name+": "+failure.name, func(t *testing.T) {
					store := architecturekit.NewStore(
						writingDatabase(t, "/api/v1/write-events", failure.answer), "https://thenativeweb.io")

					err := write.write(context.Background(), store, incremented{By: 1})

					assertOutcomeUnknown(t, err)
					assert.True(t, strings.HasPrefix(err.Error(), "outcome unknown: "+write.doing+": "),
						"the message has to name the write after the outcome, got: %v", err)
					assert.ErrorContains(t, err, failure.detail)
				})
			}
		}
	})

	t.Run("keeps the error of the client reachable when the outcome is unknown", func(t *testing.T) {
		for _, write := range kitWrites {
			t.Run(write.name, func(t *testing.T) {
				refused := architecturekit.NewStore(writingDatabase(t, "/api/v1/write-events",
					answering(http.StatusInternalServerError, "failed")), "https://thenativeweb.io")
				proxied := architecturekit.NewStore(writingDatabase(t, "/api/v1/write-events",
					fromAProxy(http.StatusBadGateway)), "https://thenativeweb.io")

				failed := write.write(context.Background(), refused, incremented{By: 1})
				answer, isAnswer := errors.AsType[*eventsourcingdb.DBAPIError](failed)
				require.True(t, isAnswer, "errors.As has to reach the answer of the database, got: %v", failed)
				assert.Equal(t, http.StatusInternalServerError, answer.Status)
				assert.Equal(t, "failed", answer.Description)

				assert.ErrorIs(t, write.write(context.Background(), proxied, incremented{By: 1}), eventsourcingdb.ErrInvalidServerHeader)
			})
		}
	})

	t.Run("follows a redirect of a proxy as the client does", func(t *testing.T) {
		// The client follows 301, 302, and 303 with a GET, which the database
		// refuses before writing, and 307 and 308 with the body once more, so
		// the answer of the database decides then, here a 500, which may
		// follow storing the events.
		redirects := []struct {
			status    int
			isUnknown bool
		}{
			{http.StatusMovedPermanently, false},
			{http.StatusFound, false},
			{http.StatusSeeOther, false},
			{http.StatusTemporaryRedirect, true},
			{http.StatusPermanentRedirect, true},
		}

		for _, write := range kitWrites {
			for _, redirect := range redirects {
				t.Run(write.name+": "+strconv.Itoa(redirect.status), func(t *testing.T) {
					server := httptest.NewServer(speakingV1(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
						switch request.URL.Path {
						case "/api/v1/write-events":
							_, _ = io.ReadAll(request.Body)
							http.Redirect(writer, request, "/moved/write-events", redirect.status)
						case "/moved/write-events":
							writer.Header().Set("Server", "EventSourcingDB/test")
							if request.Method != http.MethodPost {
								writer.WriteHeader(http.StatusMethodNotAllowed)
								return
							}
							writer.WriteHeader(http.StatusInternalServerError)
						default:
							writer.Header().Set("Server", "EventSourcingDB/test")
						}
					})))
					t.Cleanup(server.Close)
					store := architecturekit.NewStore(clientFor(t, server), "https://thenativeweb.io")

					err := write.write(context.Background(), store, incremented{By: 1})

					if redirect.isUnknown {
						assertOutcomeUnknown(t, err)
						answer, isAnswer := errors.AsType[*eventsourcingdb.DBAPIError](err)
						require.True(t, isAnswer, "errors.As has to reach the answer of the database, got: %v", err)
						assert.Equal(t, http.StatusInternalServerError, answer.Status)
						return
					}

					assert.ErrorIs(t, err, architecturekit.ErrPermanent, "the database refused the GET before writing")
					assert.NotErrorIs(t, err, architecturekit.ErrOutcomeUnknown)
					answer, isAnswer := errors.AsType[*eventsourcingdb.DBAPIError](err)
					require.True(t, isAnswer, "errors.As has to reach the answer of the database, got: %v", err)
					assert.Equal(t, http.StatusMethodNotAllowed, answer.Status)
				})
			}
		}
	})

	t.Run("with an unknown outcome is never tried again by the kit, not even by a store that decides again on conflicts", func(t *testing.T) {
		var writes atomic.Int32
		store := architecturekit.NewStore(
			writingDatabase(t, "/api/v1/write-events", func(http.ResponseWriter, *http.Request) {
				writes.Add(1)
				panic(http.ErrAbortHandler)
			}),
			"https://thenativeweb.io", architecturekit.WithConflictRetries(3))

		_, err := architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: "/test", By: 1}.onStateRead())

		assertOutcomeUnknown(t, err)
		assert.Equal(t, int32(1), writes.Load(), "trying again may store the events twice")
	})
}

func TestAStallWhileWaitingForTheAnswer(t *testing.T) {
	// The client ends a request on which nothing has arrived for its stall
	// timeout. A write that waits that long for its answer has left, so
	// whether the database stored the events is unknown.
	for _, write := range kitWrites {
		t.Run(write.name+" leaves the outcome unknown", func(t *testing.T) {
			store := architecturekit.NewStore(writingDatabase(t, "/api/v1/write-events",
				func(_ http.ResponseWriter, request *http.Request) {
					// The client goes away once its stall timeout has run out.
					select {
					case <-request.Context().Done():
					case <-time.After(5 * time.Second):
					}
				}, eventsourcingdb.WithStallTimeout(200*time.Millisecond)), "https://thenativeweb.io")

			err := write.write(context.Background(), store, incremented{By: 1})

			assertOutcomeUnknown(t, err)
			assert.ErrorContains(t, err, "connection stalled")
			assert.ErrorIs(t, err, eventsourcingdb.ErrConnectionStalled, "errors.Is has to reach the stall")
			assert.ErrorIs(t, err, eventsourcingdb.ErrOutcomeUnknown, "errors.Is has to reach the unknown outcome of the client")
		})
	}
}
