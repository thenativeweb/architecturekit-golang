# architecturekit-golang

Building blocks for DDD-based applications with CQRS and event sourcing, in Go,
on top of the [EventSourcingDB](https://www.eventsourcingdb.io).

A kit, not a framework: everything here can be used on its own or left alone.
There is no dispatcher, no configuration object, no response format you have to
accept, and no behaviour that happens behind your back.

This is v0. Breaking changes can happen in any release.

## Concepts

| Concept | What it is |
|---|---|
| **Event** | What happened. A struct that knows its own event type. |
| **State** | The state a command decides on, and the rules that build it from events. |
| **Command** | What someone wants. A struct that knows its subject and its preconditions. |
| **Decider** | The decision: state plus command become events. |
| **Projection** | Events become a view. |
| **View** | What is stored, in memory or in a database. |
| **Query** | What someone wants to know, answered from a view. |

## The write side

```go
type Scheduled struct {
	Title    string `json:"title"`
	Capacity int    `json:"capacity"`
}

func (Scheduled) EventType() string { return "io.thenativeweb.workshop.scheduled" }

state := architecturekit.NewState(Workshop{})
state.Evolve(func(current Workshop, event Scheduled) Workshop {
	current.IsScheduled = true
	current.Capacity = event.Capacity
	return current
})
```

The decision is a pure function of command and state, so it is testable without
a database. See **Testing** below.

### Commands carry their own preconditions

The kit adds **no** preconditions of its own. A command says under which
conditions its events may be appended, and it can build them from its fields:

```go
func (c Schedule) Preconditions() []eventsourcingdb.Precondition {
	return []eventsourcingdb.Precondition{
		eventsourcingdb.NewIsSubjectPristinePrecondition(c.Subject()),
	}
}

func (c ReserveSeat) Preconditions() []eventsourcingdb.Precondition {
	return []eventsourcingdb.Precondition{
		eventsourcingdb.NewIsSubjectOnEventIDPrecondition(c.Subject(), c.ExpectedEventID),
	}
}
```

Optimistic concurrency, idempotency and uniqueness are therefore the same
mechanism. A command that declares a revision precondition needs the event ID
its caller decided on, and a caller that does not supply one gets an error
rather than an unguarded write.

### No retries

`Execute` reports a conflict and returns. It does not try again, because only
the caller knows whether a second attempt is worth it, and because a retry
hides contention instead of showing it.

### Errors carry categories

Ask for the category, not for the concrete error, and new failures will not
break your code:

```go
errors.Is(err, architecturekit.ErrDomain)     // a business rule said no
errors.Is(err, architecturekit.ErrConflict)   // a precondition did not hold
errors.Is(err, architecturekit.ErrTransient)  // trying again may help
errors.Is(err, architecturekit.ErrPermanent)  // trying again will not help
```

The HTTP layer maps them: 422 for a domain rule, 409 for a conflict, 503 for
anything transient, 500 otherwise, plus 401, 403, 404, 413 and 415 for the
cases it detects itself. A query that finds nothing reports `query.ErrNoItems`
and becomes a 404 without the application translating it.

## The query side

A **view** is what is stored. A **query** is what runs over it, and the two are
not the same: a query filters, orders, limits and projects, so what a caller
sees is never dictated by how the view happens to be kept.

```go
func ListWorkshopsIn(view architecturekit.View[WorkshopView]) func(context.Context, ListWorkshops) ([]WorkshopView, error) {
	return func(ctx context.Context, ask ListWorkshops) ([]WorkshopView, error) {
		items, err := view.All(ctx)
		if err != nil {
			return nil, err
		}

		if ask.OnlyAvailable {
			items = query.Where(items, func(item WorkshopView) bool { return item.Free() > 0 })
		}
		items = query.OrderBy(items, func(item WorkshopView) string { return item.Title })
		if ask.Limit > 0 {
			items = query.Take(items, ask.Limit)
		}

		return slices.Collect(items), nil
	}
}
```

The `query` package has `Where`, `Select`, `OrderBy`, `OrderByDescending`,
`OrderByFunc`, `Skip`, `Take`, `First`, `Single`, `Count` and `Any`.

`OrderByFunc` takes a comparison rather than a key, for orders no single key
can express: several fields, a field that is not ordered on its own such as a
bool, or a custom collation. Its signature is the one the standard library
uses, so `cmp.Compare` and `cmp.Or` compose with it:

```go
query.OrderByFunc(items, func(left, right ToDo) int {
	return cmp.Or(
		left.DueDate.Compare(right.DueDate),
		cmp.Compare(rank(right.IsPrioritized), rank(left.IsPrioritized)),
		strings.Compare(left.Title, right.Title),
	)
})
```

The scope stops there: no joins, no aggregation across views, no query
language. Everything works on `iter.Seq`, so the steps compose and nothing is
materialised in between; sorting is the exception, because it has to read
everything. What the standard library already does is not repeated: use
`slices.Collect` and `slices.Sorted`.

Note that the query takes an `architecturekit.View`, not an implementation.
`ItemView` is the one the kit ships and it keeps its items in memory; a
database-backed view satisfies the same interface.

A projection says what it can do, and the kit drives it accordingly. `ModeOf`
reports which mode it picked; log it at startup.

| Interface | Mode | Meaning |
|---|---|---|
| `Projection` | rebuild | No checkpoint, read from the beginning on every start. |
| `+ Resumable` | resumable | Resumes; events can repeat after a crash, so `Apply` has to be idempotent. |
| `+ Transactional` | transactional | Data and checkpoint become durable together. |

`ItemView` is the one view the kit ships, with `Insert`, `Update`, `Upsert`,
`Delete` and `All`. It holds no checkpoint, so its projection is rebuilt on
every start.

A read model **has to carry the event ID**, because a caller that wants to send
a command with a revision precondition needs it.

Batch sizes default to one, separately for catching up and for live operation.
That is the safe choice and also works for projections that are not idempotent.
A projection that knows its target can raise them through `Batched`; a rebuild
over millions of events needs that.

## Reading your own writes

A read model lags behind, by however long its projection takes. A caller that
has just written something and reads again right away may not see it yet. The
usual answer is to wait a moment and hope; the kit turns that into a condition
the server can check.

Every event has an ID, and the database hands them out as one ascending
sequence across all streams. The ID of the last event a projection has seen is
therefore a **revision**: it says how far a read model has come.

```go
// The write says which revision it produced.
handled, err := httpapi.Handle[rememberRequest](r, api, todo.RememberDecider(now))
revision := architecturekit.RevisionOf(handled.Events)
```

The caller sends that revision with its next query, in the `Wait-For-Revision`
header. The query waits until the view has reached it and only then answers:

```go
view := architecturekit.NewItemView[toDoItem]()

// Tracking records every event that reaches the projection, so the view knows
// how far it has come.
go architecturekit.RunProjection(ctx, store, "/", true,
	architecturekit.Tracking(view, toDoProjection(view)))

httpapi.QueryRevisioned(api, mux, "GET /api/to-dos", view,
	toQuery, answerToDos, httpapi.DefaultWait)
```

This is the read side's counterpart to a write precondition. A command says *I
decided on this revision*; a query says *I want to see at least this revision*.

A few decisions worth knowing about:

- **Waiting in the query, not in the command.** The command stays fast, and the
  wait is paid only by a caller that actually depends on its own write. It also
  works across instances: whichever one answers can wait, so no request has to
  be routed back to the instance that wrote.
- **Running out of time is not an error.** The query answers with what the view
  has, and says in the `ETag` and in `X-Revision` which revision that is. A
  caller that needs more can ask again.
- **`Tracking` records events the projection ignores.** A projection skips what
  does not concern it, but a caller may be waiting for exactly that event's ID.
- **The `ETag` is not the revision alone.** Every query over the same view
  shares a revision, so the tag also identifies the resource. Otherwise one
  query's tag would match another's, and a caller would be told, wrongly, that
  nothing had changed.
- **`If-None-Match` is not used for waiting.** An entity tag is opaque and
  compares only for equality, while a revision is ordered. A server that is
  behind would answer "not equal, here you go" and hand out stale data.

A view knows its revision when it implements `Revisioned`; `ItemView` does. A
view that keeps a transaction should write the revision inside it, because data
and revision have to become durable together.

## Event versioning

The EventSourcingDB keeps one schema per event type, forever, so a new shape
means a new type. Upcasters translate the old ones when reading, chained, and
their result is never written back:

```go
state.Upcast("io.thenativeweb.workshop.scheduled.v1", func(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
	// ... return the event with its new type and shape
})
```

## Subjects

A scheme works in both directions, in the style of the patterns that
`http.ServeMux` understands:

```go
var Subject = architecturekit.NewSubjectScheme("/tenant/{tenant}/workshop/{workshop}")

Subject.Build("acme", "go-cqrs")   // for a command
Subject.Match(event.Subject)       // for a projection
```

## HTTP

The core knows nothing about transports. The optional `httpapi` package adds
the convenience:

```go
// the short way
httpapi.Route[scheduleRequest](api, mux, "POST /api/v1/schedule-workshop", workshop.ScheduleDecider())

// your own format
mux.HandleFunc("POST /api/v1/schedule-workshop", func(w http.ResponseWriter, r *http.Request) {
	handled, err := httpapi.Handle[scheduleRequest](r, api, workshop.ScheduleDecider())
	problemDetails(w, httpapi.StatusFor(err), err)
})
```

`Handle` gives back the command it built, not only the written events. That
matters when the server, rather than the client, decides what a new aggregate
is called:

```go
mux.HandleFunc("POST /api/v1/todos", func(w http.ResponseWriter, r *http.Request) {
	handled, err := httpapi.Handle[rememberRequest](r, api, todo.RememberDecider())
	if err != nil {
		httpapi.Respond(w, nil, err)
		return
	}

	// The id was made up in ToCommand and is readable here, instead of having
	// to be dug back out of the subject of a written event.
	writeJSON(w, http.StatusCreated, map[string]string{"id": handled.Command.TodoID})
})
```

The request DTO carries the JSON tags and builds the command, so the command
stays free of transport concerns and a GraphQL resolver can build the same one.

The read side has the same split, in two steps: `toQuery` turns request and
user into a query, and the answer function sees neither the request nor HTTP.

```go
httpapi.Query(api, mux, "GET /api/v1/workshops",
	func(r *http.Request, user User) (workshop.ListWorkshops, error) {
		return workshop.ListWorkshops{OnlyAvailable: r.URL.Query().Get("available") == "true"}, nil
	},
	answerListWorkshops(view),
)
```

### Users

Who is asking is a type of your own, so a token's roles, tenant or claims all
fit in:

```go
type User struct {
	ID    string
	Roles []string
}

api := httpapi.NewAPI(store, UserFrom)
```

A request whose user cannot be determined is answered with 401 and never
reaches a command or a query. An application without authentication uses
`httpapi.NewPublicAPI(store)` and receives `httpapi.NoUser`.

Authorisation belongs in `ToCommand` or `toQuery`: return `httpapi.ErrForbidden`
and the caller gets a 403, before a command exists at all.

Three rules that are not negotiable in this package, each for a reason:

- **Unknown fields are rejected.** A misspelled field would otherwise turn into
  a zero value in silence.
- **`application/json` is required**, parsed rather than matched as a substring.
  Only urlencoded, multipart and `text/plain` are simple requests, so demanding
  JSON shuts out cross-origin writes whatever you use to authenticate.
- **One mebibyte**, then 413. The same limit the database has.

## Requirements

Go 1.27 or later. The kit uses generic methods, which earlier versions do not
support.

## Testing your own code

`architecturekittest` drives deciders and projections without a database.

### Deciders

```go
architecturekittest.Given(t, workshop.ScheduleDecider(), workshop.Scheduled{Capacity: 2}).
	When(workshop.Schedule{WorkshopID: "go-cqrs", Capacity: 2}).
	ThenRejected("workshop go-cqrs has already been scheduled")
```

| | |
|---|---|
| `Given(t, decider, events...)` | history as typed events |
| `GivenStored(t, decider, stored...)` | history in its stored shape, running the upcasters |
| `When(cmd)` | run the command |
| `ThenEvents(...)` | exactly these events, in this order |
| `ThenNothing()` | neither events nor a failure |
| `ThenRejected(message)` | rejected with exactly this message |
| `ThenFailed(category)` | failed with this category, for instance `ErrDomain` |
| `ThenSomeEvent(match)` | at least one event matches |
| `ThenEveryEvent(match)` | all of them match |
| `ThenNoEvent(match)` | none of them matches |
| `ThenState(check)` | inspect the state that was decided on |
| `ThenPreconditions(...)` | the command declares exactly these |

Every assertion returns the outcome, so they chain.

`GivenStored` is worth knowing: it is the only way to test an upcaster, because
an old event type usually has no Go struct any more. A fixture that resolves
types eagerly cannot do this.

`ThenPreconditions` matters because preconditions are how this kit guards
concurrency and idempotency. A command that forgets its revision check would
otherwise pass every test:

```go
architecturekittest.Given(t, workshop.ReserveSeatDecider(), workshop.Scheduled{Capacity: 2}).
	When(workshop.ReserveSeat{WorkshopID: "go-cqrs", Attendee: "golo", ExpectedEventID: "0"}).
	ThenEvents(workshop.SeatReserved{Attendee: "golo"}).
	ThenPreconditions(architecturekittest.OnEventID("/workshop/go-cqrs", "0"))
```

Note that a pristine and a populated check are indistinguishable here, both
surfacing as `OnSubject`, because the client exposes only the subject for
either one.

### Projections

```go
view := workshop.NewCatalog()

architecturekittest.ExpectMode(t, workshop.CatalogProjection(view), architecturekit.ModeRebuild)

architecturekittest.Project(t, workshop.CatalogProjection(view),
	architecturekittest.StoredEvents("/workshop/go-cqrs",
		workshop.Scheduled{Title: "Event Sourcing", Capacity: 2},
		workshop.SeatReserved{Attendee: "golo"},
	)...)

items := architecturekittest.ItemsOf(t, view)
```

`ExpectMode` is the one to remember: the three projection modes come from
optional interfaces, so a typo in a method signature leaves one unfulfilled and
the projection quietly falls back to being rebuilt on every start. Asserting
the mode is what catches that.

Queries need no fixture, because they are ordinary functions:

```go
found, err := workshop.ListWorkshopsIn(view)(ctx, workshop.ListWorkshops{OnlyAvailable: true})
```

## Tests of this repository

```shell
make test        # everything, starts EventSourcingDB in Docker
make test-short  # unit tests only, no Docker required
make qa          # vet, staticcheck and tests
```

Statement coverage is at 100 percent.

There is no example application in this repository. The kit shows what it does
through its own tests: `architecturekit/testdomain_test.go` holds a domain small
enough to read in one sitting, and every section above has a test that runs it.

## Known limitations

- **HTTP 409 is ambiguous.** The database answers a violated precondition and a
  schema violation with the same status, and its Go client exports no typed
  error, so the kit cannot tell them apart. A schema violation therefore
  arrives as `ErrConflict`, although it is permanent.
- **The write path has no timeout.** `WriteEvents` and `RegisterEventSchema` of
  the client take no `context.Context` and use `http.DefaultClient`, which has
  no timeout of its own.
- **A re-registered schema goes unnoticed.** Registration is idempotent,
  because otherwise no application could start twice, and the database answers
  the same whether the schema changed or not.
- **No snapshots.** Long streams are folded in full on every command.

## License

MIT
