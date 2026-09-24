# architecturekit

Building blocks for DDD-based applications with CQRS and event sourcing in Go, on top of [EventSourcingDB](https://www.eventsourcingdb.io) – a purpose-built database for event sourcing.

architecturekit covers both sides of an event-sourced application: commands, events, and the state to decide on for writing, and projections, views, and queries for reading. An optional package exposes commands and queries over HTTP.

For more information on EventSourcingDB, see its [official documentation](https://docs.eventsourcingdb.io/).

architecturekit includes a test package to test deciders, projections, and queries without a database. For details, see [Testing Deciders](#testing-deciders).

## Getting Started

Install the package:

```shell
go get github.com/thenativeweb/architecturekit-golang
```

Import the package, create an EventSourcingDB client, and create a store by providing the client and the source to use for all events you write:

```go
import (
  "net/url"

  "github.com/thenativeweb/architecturekit-golang/architecturekit"
  "github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// ...

baseURL, err := url.Parse("http://localhost:3000")
if err != nil {
  // ...
}

apiToken := "secret"

client, err := eventsourcingdb.NewClient(baseURL, apiToken)
if err != nil {
  // ...
}

store := architecturekit.NewStore(client, "https://library.eventsourcingdb.io")
```

The `NewStore` function returns a `*Store`, which reads and writes the events of all commands. For details on the client, see the [client SDK for Go](https://github.com/thenativeweb/eventsourcingdb-client-golang).

### Defining Commands

A command describes what someone wants to do. Define it as a struct and implement the `Subject` function, which returns the subject the command acts on. This makes the struct a `Command`:

```go
type AcquireBook struct {
  BookID string
  Title  string
  Author string
  ISBN   string
}

func (c AcquireBook) Subject() string {
  return "/books/" + c.BookID
}

type BorrowBook struct {
  BookID        string
  ReaderID      string
  BorrowedUntil string
}

func (c BorrowBook) Subject() string {
  return "/books/" + c.BookID
}

type ReturnBook struct {
  BookID string
}

func (c ReturnBook) Subject() string {
  return "/books/" + c.BookID
}
```

### Defining Events

An event describes what has happened. Define it as a struct with JSON annotations and implement the `EventType` function, which returns the event type. This makes the struct an `Event`:

```go
type BookAcquired struct {
  Title  string `json:"title"`
  Author string `json:"author"`
  ISBN   string `json:"isbn"`
}

func (BookAcquired) EventType() string {
  return "io.eventsourcingdb.library.book-acquired"
}

type BookBorrowed struct {
  BorrowedBy    string `json:"borrowedBy"`
  BorrowedUntil string `json:"borrowedUntil"`
}

func (BookBorrowed) EventType() string {
  return "io.eventsourcingdb.library.book-borrowed"
}

type BookReturned struct{}

func (BookReturned) EventType() string {
  return "io.eventsourcingdb.library.book-returned"
}
```

The struct becomes the event's data. The subject is taken from the command, and the source from the store.

### Defining State

The state holds what a command needs to decide on. Define it as a struct, call the `NewState` function with its initial value, and call the `Evolve` function for every event type that changes it:

```go
type Book struct {
  IsAcquired bool
  IsBorrowed bool
}

var bookState = architecturekit.NewState(Book{}).
  Evolve(func(book Book, event BookAcquired) Book {
    book.IsAcquired = true
    return book
  }).
  Evolve(func(book Book, event BookBorrowed) Book {
    book.IsBorrowed = true
    return book
  }).
  Evolve(func(book Book, event BookReturned) Book {
    book.IsBorrowed = false
    return book
  })
```

The event type is taken from the event's `EventType` function, so it does not have to be repeated.

*Note that calling `Evolve` twice for the same event type panics.*

### Making Decisions

A decider connects a state with the decision made on it. Create a `Decider`, hand over the state, and provide a `Decide` function that receives the command and the current state, and returns the events to write:

```go
var acquireBook = architecturekit.Decider[AcquireBook, Book]{
  State: bookState,
  Decide: func(ctx context.Context, cmd AcquireBook, book Book) ([]architecturekit.Event, error) {
    if book.IsAcquired {
      return nil, architecturekit.NewDomainError("book %s has already been acquired", cmd.BookID)
    }

    return []architecturekit.Event{
      BookAcquired{
        Title:  cmd.Title,
        Author: cmd.Author,
        ISBN:   cmd.ISBN,
      },
    }, nil
  },
}
```

To reject a command, return an error created with the `NewDomainError` function. It takes a format string and arguments, like `fmt.Errorf`, and returns a `*DomainError`, whose message is exactly the formatted text, and which belongs to the category `ErrDomain` (see [Handling Errors](#handling-errors)).

A decider may check several rules:

```go
var borrowBook = architecturekit.Decider[BorrowBook, Book]{
  State: bookState,
  Decide: func(ctx context.Context, cmd BorrowBook, book Book) ([]architecturekit.Event, error) {
    if !book.IsAcquired {
      return nil, architecturekit.NewDomainError("book %s does not exist", cmd.BookID)
    }
    if book.IsBorrowed {
      return nil, architecturekit.NewDomainError("book %s is already borrowed", cmd.BookID)
    }

    return []architecturekit.Event{
      BookBorrowed{
        BorrowedBy:    cmd.ReaderID,
        BorrowedUntil: cmd.BorrowedUntil,
      },
    }, nil
  },
}
```

If there is nothing to do, return neither events nor an error:

```go
var returnBook = architecturekit.Decider[ReturnBook, Book]{
  State: bookState,
  Decide: func(ctx context.Context, cmd ReturnBook, book Book) ([]architecturekit.Event, error) {
    if !book.IsBorrowed {
      return nil, nil
    }

    return []architecturekit.Event{
      BookReturned{},
    }, nil
  },
}
```

### Executing Commands

To execute a command, call the `Execute` function with a context, the store, the decider, and the command:

```go
writtenEvents, err := architecturekit.Execute(
  context.TODO(),
  store,
  acquireBook,
  AcquireBook{
    BookID: "42",
    Title:  "2001 – A Space Odyssey",
    Author: "Arthur C. Clarke",
    ISBN:   "978-0756906788",
  },
)
if err != nil {
  // ...
}
```

`Execute` reads the events of the command's subject, evolves the state from them, calls the decider, and writes the events it returns. The function returns the written events, including the fields added by the server. If the decider returns no events, nothing is written, and the function returns `nil`.

*Note that `Execute` only reads the events of the command's subject itself, not those of nested subjects.*

### Using Preconditions

By default, `Execute` writes events without any preconditions. To add some, implement the `Preconditions` function on the command and return the preconditions to use, which makes the command `Preconditioned`. Create the preconditions with the functions of the client SDK.

If a precondition does not hold, nothing is written, and `Execute` returns an error of the category `ErrConflict` (see [Handling Errors](#handling-errors)).

#### Preventing Duplicates

If a command may only write events in case its subject does not yet have any events, use the `NewIsSubjectPristinePrecondition` function:

```go
func (c AcquireBook) Preconditions() []eventsourcingdb.Precondition {
  return []eventsourcingdb.Precondition{
    eventsourcingdb.NewIsSubjectPristinePrecondition(c.Subject()),
  }
}
```

#### Requiring an Existing Subject

If a command may only write events in case its subject already has at least one event, use the `NewIsSubjectPopulatedPrecondition` function:

```go
func (c ReturnBook) Preconditions() []eventsourcingdb.Precondition {
  return []eventsourcingdb.Precondition{
    eventsourcingdb.NewIsSubjectPopulatedPrecondition(c.Subject()),
  }
}
```

#### Guarding Against Concurrent Changes

If a command may only write events in case its subject has not changed since the caller last read it, use the `NewIsSubjectOnEventIDPrecondition` function. For that, add a field for the ID of the last event the caller has seen:

```go
type BorrowBook struct {
  BookID          string
  ReaderID        string
  BorrowedUntil   string
  ExpectedEventID string
}

func (c BorrowBook) Preconditions() []eventsourcingdb.Precondition {
  return []eventsourcingdb.Precondition{
    eventsourcingdb.NewIsSubjectOnEventIDPrecondition(c.Subject(), c.ExpectedEventID),
  }
}
```

*Note that the caller has to provide the event ID. A view can keep it for that purpose (see [Defining Views](#defining-views)).*

#### Enforcing Rules Across Subjects

If a command may only write events depending on an EventQL query, use the `NewIsEventQLQueryTruePrecondition` function. Preconditions can be combined, and all of them must hold. For example, to acquire every ISBN only once, extend the preconditions of `AcquireBook`:

```go
func (c AcquireBook) Preconditions() []eventsourcingdb.Precondition {
  return []eventsourcingdb.Precondition{
    eventsourcingdb.NewIsSubjectPristinePrecondition(c.Subject()),
    eventsourcingdb.NewIsEventQLQueryTruePrecondition(fmt.Sprintf(
      "FROM e IN events WHERE e.type == 'io.eventsourcingdb.library.book-acquired' AND e.data.isbn == '%s' PROJECT INTO COUNT() == 0",
      c.ISBN,
    )),
  }
}
```

*Note that the query must return a single row with a single value, which is interpreted as a boolean.*

### Handling Errors

Every error architecturekit returns belongs to one of four categories. Use `errors.Is` to check for a category rather than for a concrete error:

- `ErrDomain` means that a business rule rejected the command, as with `NewDomainError`.
- `ErrConflict` means that a precondition did not hold.
- `ErrTransient` means that trying again may help, for example if reading from the database failed.
- `ErrPermanent` means that trying again will not help, for example if an event could not be decoded, or if a subject contains an event type the state has no `Evolve` rule for.

```go
writtenEvents, err := architecturekit.Execute(
  // ...
)

switch {
case errors.Is(err, architecturekit.ErrDomain):
  // A business rule rejected the command.
case errors.Is(err, architecturekit.ErrConflict):
  // A precondition did not hold.
case errors.Is(err, architecturekit.ErrTransient):
  // Trying again may help.
case errors.Is(err, architecturekit.ErrPermanent):
  // Trying again will not help.
}
```

*Note that `ErrConflict` is a special case of `ErrTransient`, so check for it first.*

*Note that `Execute` does not retry. To try again, for example after a conflict, call `Execute` again.*

### Registering Event Schemas

To have the database validate events of a type, implement the `Schema` function on the event and return a JSON schema. This makes the event a `SchemaProvider`:

```go
func (BookAcquired) Schema() map[string]any {
  return map[string]any{
    "type": "object",
    "properties": map[string]any{
      "title":  map[string]any{"type": "string"},
      "author": map[string]any{"type": "string"},
      "isbn":   map[string]any{"type": "string"},
    },
    "required": []string{
      "title",
      "author",
      "isbn",
    },
    "additionalProperties": false,
  }
}
```

The `Evolve` function collects the schemas of all events that implement `Schema`. To get them as a slice of `EventSchema`, each with the fields `EventType` and `Schema`, call the `Schemas` function on the state. Then hand them over to the `RegisterSchemas` function of the store:

```go
err := store.RegisterSchemas(bookState.Schemas())
if err != nil {
  // ...
}
```

`RegisterSchemas` accepts the schemas of several states at once. Event types that are already registered count as success, so you can call the function on every start.

### Versioning Events

The database keeps the schema of an event type forever. If the shape of an event changes, introduce a new event type, and translate the stored events of the old type with an upcaster.

Suppose an earlier version of the library wrote events of the type `io.eventsourcingdb.library.book-lent`, with the fields `lentTo` and `until`. To translate them into `BookBorrowed` events, call the `Upcast` function on the state and hand over the old event type and a function that receives the stored event and returns the translated events:

```go
bookState.Upcast(
  "io.eventsourcingdb.library.book-lent",
  func(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
    var old struct {
      LentTo string `json:"lentTo"`
      Until  string `json:"until"`
    }
    if err := json.Unmarshal(event.Data, &old); err != nil {
      return nil, err
    }

    data, err := json.Marshal(BookBorrowed{
      BorrowedBy:    old.LentTo,
      BorrowedUntil: old.Until,
    })
    if err != nil {
      return nil, err
    }

    event.Type = BookBorrowed{}.EventType()
    event.Data = data

    return []eventsourcingdb.Event{event}, nil
  },
)
```

The function has the type `Upcaster`. Upcasters run before the `Evolve` rules, and they may return more than one event. If a returned event has an upcaster of its own, that one runs as well, so every version needs only a single step to the next one. The translated events are never written back.

*Note that calling `Upcast` twice for the same event type panics.*

*Note that upcasters only apply to the state. Projections receive events as they are stored.*

### Composing Subjects

So far, subjects have been composed by hand. To define their structure once, call the `NewSubjectScheme` function with a pattern, and use placeholders in braces for the variable parts:

```go
var bookSubject = architecturekit.NewSubjectScheme("/books/{book}")
```

The function returns a `*SubjectScheme`. To compose a subject, call the `Build` function with one value per placeholder, in the order in which they appear in the pattern. Use it in every command that acts on a book:

```go
func (c AcquireBook) Subject() string {
  return bookSubject.Build(c.BookID)
}

func (c BorrowBook) Subject() string {
  return bookSubject.Build(c.BookID)
}

func (c ReturnBook) Subject() string {
  return bookSubject.Build(c.BookID)
}
```

To take a subject apart, call the `Match` function. It returns the values by placeholder name, and `false` if the subject does not follow the pattern:

```go
values, ok := bookSubject.Match("/books/42")
// values["book"] == "42"
```

To get the pattern and the names of the placeholders, call the `Pattern` and the `Placeholders` function respectively.

*Note that a malformed pattern panics, as does calling `Build` with the wrong number of values, with an empty value, or with a value that contains a slash.*

### Defining Views

A view holds the data that queries read. Define the shape of an item as a struct, and call the `NewItemView` function to create a view that holds such items in memory:

```go
type BookItem struct {
  ID            string `json:"id"`
  Title         string `json:"title"`
  Author        string `json:"author"`
  IsBorrowed    bool   `json:"isBorrowed"`
  BorrowedUntil string `json:"borrowedUntil"`
  EventID       string `json:"eventId"`
}

catalog := architecturekit.NewItemView[BookItem]()
```

Keep the ID of the last event in every item, so that a caller can hand it over to a command that uses the `NewIsSubjectOnEventIDPrecondition` function (see [Guarding Against Concurrent Changes](#guarding-against-concurrent-changes)).

To add an item, call the `Insert` function:

```go
catalog.Insert(BookItem{
  ID:     "42",
  Title:  "2001 – A Space Odyssey",
  Author: "Arthur C. Clarke",
})
```

To change items, call the `Update` function with a function that selects the items and a function that changes them. It returns the number of changed items:

```go
isBook42 := func(item BookItem) bool {
  return item.ID == "42"
}

changed := catalog.Update(isBook42, func(item *BookItem) {
  item.IsBorrowed = true
})
```

To change items or add an item if none matches, call the `Upsert` function and additionally hand over the item to add. It returns the number of changed items, which is `0` if the item was added:

```go
changed := catalog.Upsert(isBook42, func(item *BookItem) {
  item.IsBorrowed = true
}, BookItem{
  ID:         "42",
  IsBorrowed: true,
})
```

To remove items, call the `Delete` function. It returns the number of removed items:

```go
removed := catalog.Delete(isBook42)
```

To read all items, call the `All` function. It returns an iterator over a copy of the items, which you can use e.g. inside a `for range` loop:

```go
items, err := catalog.All(context.TODO())
if err != nil {
  // ...
}

for item := range items {
  // ...
}
```

To keep items somewhere else, for example in a database, implement the `View` interface, which consists of the `All` function:

```go
type BookTable struct {
  // ...
}

func (t *BookTable) All(ctx context.Context) (iter.Seq[BookItem], error) {
  // ...
}
```

### Defining Projections

A projection turns events into a view. Define a type and implement the `Apply` function, which receives every event as it is stored. This makes the type a `Projection`:

```go
type CatalogProjection struct {
  catalog *architecturekit.ItemView[BookItem]
}

func (p CatalogProjection) Apply(ctx context.Context, event eventsourcingdb.Event) error {
  values, ok := bookSubject.Match(event.Subject)
  if !ok {
    return nil
  }

  bookID := values["book"]
  isBook := func(item BookItem) bool {
    return item.ID == bookID
  }

  switch event.Type {
  case BookAcquired{}.EventType():
    var data BookAcquired
    if err := json.Unmarshal(event.Data, &data); err != nil {
      return err
    }

    p.catalog.Insert(BookItem{
      ID:      bookID,
      Title:   data.Title,
      Author:  data.Author,
      EventID: event.ID,
    })

  case BookBorrowed{}.EventType():
    var data BookBorrowed
    if err := json.Unmarshal(event.Data, &data); err != nil {
      return err
    }

    p.catalog.Update(isBook, func(item *BookItem) {
      item.IsBorrowed = true
      item.BorrowedUntil = data.BorrowedUntil
      item.EventID = event.ID
    })

  case BookReturned{}.EventType():
    p.catalog.Update(isBook, func(item *BookItem) {
      item.IsBorrowed = false
      item.BorrowedUntil = ""
      item.EventID = event.ID
    })
  }

  return nil
}

catalogProjection := CatalogProjection{catalog: catalog}
```

For a projection that needs no type of its own, use `ProjectionFunc`, which turns a function into a projection:

```go
logProjection := architecturekit.ProjectionFunc(func(ctx context.Context, event eventsourcingdb.Event) error {
  log.Println(event.Subject, event.Type)
  return nil
})
```

### Running Projections

To run a projection, call the `RunProjection` function with a context, the store, the subject, whether to read recursively, and the projection. The function first applies all events that are already stored, then observes new events until the context is canceled. Since it blocks, run it in a goroutine:

```go
ctx, cancel := context.WithCancel(context.TODO())

go func() {
  err := architecturekit.RunProjection(ctx, store, "/books", true, catalogProjection)
  if err != nil {
    // ...
  }
}()

// Somewhere else, cancel the context, which will cause
// the projection to stop.
cancel()
```

Canceling the context is not an error. If `Apply` returns an error, the function stops and returns it.

To only apply the events that are already stored, call the `CatchUpProjection` function instead. It takes the same arguments and returns once all stored events have been applied:

```go
err := architecturekit.CatchUpProjection(context.TODO(), store, "/books", true, catalogProjection)
if err != nil {
  // ...
}
```

### Resuming Projections

By default, a projection starts from the first event every time it runs, which fits a view held in memory. For a view that keeps its data, the projection can resume where it stopped instead.

If the view can store a checkpoint, but not together with the data, additionally implement the `Resumable` interface. `Checkpoint` returns the ID of the last event saved, or an empty string if there is none, and `SaveCheckpoint` saves it:

```go
type BookTableProjection struct {
  // ...
}

func (p *BookTableProjection) Apply(ctx context.Context, event eventsourcingdb.Event) error {
  // ...
}

func (p *BookTableProjection) Checkpoint(ctx context.Context) (string, error) {
  // ...
}

func (p *BookTableProjection) SaveCheckpoint(ctx context.Context, eventID string) error {
  // ...
}
```

*Note that the checkpoint is saved after the events have been applied. After a crash, events may therefore be applied a second time, so `Apply` must be idempotent.*

To find out how a projection will be run, call the `ModeOf` function. It returns a `Mode`, which is `ModeRebuild` or `ModeResumable`:

```go
mode := architecturekit.ModeOf(catalogProjection)
// architecturekit.ModeRebuild
```

*Note that the mode depends on which interfaces a projection implements. If a function's signature does not match, the projection silently runs in `ModeRebuild`. To catch that, check the mode in a test (see [Testing Projections](#testing-projections)).*

If the view can store the data and the checkpoint together, as a relational database can, implement the `Transactional` interface instead of `Projection`. A transactional projection applies events only within a transaction, so it has no `Apply` function of its own. `Begin` starts a transaction and returns a `Tx`, which applies the events, and commits them together with the ID of the last event, or rolls them back:

```go
type TransactionalBookTableProjection struct {
  // ...
}

func (p *TransactionalBookTableProjection) Checkpoint(ctx context.Context) (string, error) {
  // ...
}

func (p *TransactionalBookTableProjection) Begin(ctx context.Context) (architecturekit.Tx, error) {
  // ...
}

type bookTableTx struct {
  // ...
}

func (tx *bookTableTx) Apply(ctx context.Context, event eventsourcingdb.Event) error {
  // ...
}

func (tx *bookTableTx) Commit(ctx context.Context, lastEventID string) error {
  // ...
}

func (tx *bookTableTx) Rollback(ctx context.Context) error {
  // ...
}
```

To run a transactional projection, call the `RunTransactionalProjection` or the `CatchUpTransactionalProjection` function instead of `RunProjection` or `CatchUpProjection`. They take the same arguments:

```go
err := architecturekit.RunTransactionalProjection(ctx, store, "/books", true, &TransactionalBookTableProjection{})
if err != nil {
  // ...
}
```

*Note that `RunProjection`, `CatchUpProjection`, and `Tracking` panic for a projection that implements `Transactional` in addition to `Apply`, since calling `Apply` would bypass the transactions.*

### Batching Events

By default, the checkpoint is saved, or the transaction is committed, after every event. To do so less often, implement the `Batched` interface on a resumable or transactional projection, and return how many events to apply at once, separately for catching up and for observing:

```go
func (p *BookTableProjection) BatchSizes() (catchUp, live int) {
  return 1000, 1
}
```

Values below `1` count as `1`.

*Note that a resumable projection may apply up to that many events a second time after a crash.*

### Defining Queries

A query describes what someone wants to know. Define it as a struct, and answer it with a function that reads a view. To turn the items into a slice, use `slices.Collect`:

```go
type ListBooks struct {
  OnlyAvailable bool
  Limit         int
}

func listBooks(catalog architecturekit.View[BookItem]) func(context.Context, ListBooks) ([]BookItem, error) {
  return func(ctx context.Context, q ListBooks) ([]BookItem, error) {
    items, err := catalog.All(ctx)
    if err != nil {
      return nil, err
    }

    // ...

    return slices.Collect(items), nil
  }
}
```

To filter, order, page, and transform the items, use the `query` package:

```go
import "github.com/thenativeweb/architecturekit-golang/architecturekit/query"
```

All of its functions take an iterator, and those that return items return an iterator again, so they can be combined without collecting anything in between.

#### Filtering and Transforming Items

To keep only some items, call the `Where` function with a function that selects them:

```go
if q.OnlyAvailable {
  items = query.Where(items, func(item BookItem) bool {
    return !item.IsBorrowed
  })
}
```

To turn every item into something else, call the `Select` function:

```go
titles := query.Select(items, func(item BookItem) string {
  return item.Title
})
```

#### Ordering Items

To order items by a key, call the `OrderBy` function, or the `OrderByDescending` function for the reverse order:

```go
items = query.OrderBy(items, func(item BookItem) string {
  return item.Title
})
```

To order items by a comparison function, for example by several fields, call the `OrderByFunc` function. It expects the same kind of function as `slices.SortFunc`, so `cmp.Or` and `strings.Compare` work with it:

```go
items = query.OrderByFunc(items, func(left, right BookItem) int {
  return cmp.Or(
    strings.Compare(left.Author, right.Author),
    strings.Compare(left.Title, right.Title),
  )
})
```

*Note that ordering reads all items, and that it is stable, so items that compare as equal keep their order.*

#### Paging Items

To skip a number of items, call the `Skip` function. To stop after a number of items, call the `Take` function:

```go
if q.Limit > 0 {
  items = query.Take(items, q.Limit)
}
```

Both can be combined, for example to get the third page of ten items:

```go
items = query.Take(query.Skip(items, 20), 10)
```

#### Getting a Single Item

To get the first item, call the `First` function. It returns `false` if there is none:

```go
first, ok := query.First(items)
```

To get the only item, call the `Single` function. It returns `query.ErrNoItems` if there is none, and `query.ErrTooManyItems` if there are several:

```go
type GetBook struct {
  BookID string
}

func getBook(catalog architecturekit.View[BookItem]) func(context.Context, GetBook) (BookItem, error) {
  return func(ctx context.Context, q GetBook) (BookItem, error) {
    items, err := catalog.All(ctx)
    if err != nil {
      return BookItem{}, err
    }

    return query.Single(query.Where(items, func(item BookItem) bool {
      return item.ID == q.BookID
    }))
  }
}
```

#### Counting Items

To count items, call the `Count` function. To check whether at least one item matches, call the `Any` function, which stops at the first match:

```go
count := query.Count(items)

hasBorrowedBooks := query.Any(items, func(item BookItem) bool {
  return item.IsBorrowed
})
```

### Reading Your Own Writes

A view lags behind the events that have been written, by however long its projection takes. To read your own writes, wait until the view has seen the events you have written.

The ID of the last event a view has seen is its revision. Since the database assigns event IDs in ascending order across all subjects, revisions can be compared.

#### Tracking Revisions

To track the revision of a view, wrap the projection with the `Tracking` function and hand over the view. It records every event that reaches the projection, including the ones the projection ignores:

```go
trackedProjection := architecturekit.Tracking(catalog, catalogProjection)
```

Then run `trackedProjection` instead of `catalogProjection` (see [Running Projections](#running-projections)).

`Tracking` accepts every view that implements the `RevisionSink` interface, which consists of the `Seen` function. `ItemView` implements it.

The tracked projection keeps the mode and the batch sizes of the projection it wraps. A transactional projection can not be tracked, since it has no `Apply` function. Record its revision within the transaction instead.

To get the revision your own write has produced, call the `RevisionOf` function with the written events. It returns the highest event ID, or an empty string if no events were written:

```go
revision := architecturekit.RevisionOf(writtenEvents)
```

#### Waiting for Revisions

To wait until a view has reached a revision, call the `WaitFor` function with a context and the revision. It returns immediately if the view has already reached the revision, and otherwise once it does, or when the context ends:

```go
ctx, cancel := context.WithTimeout(context.TODO(), 5*time.Second)
defer cancel()

err := catalog.WaitFor(ctx, revision)
if err != nil {
  // ...
}
```

To get the current revision of a view, call the `Revision` function. It returns an empty string as long as the view has not seen any event:

```go
current := catalog.Revision()
```

Both functions form the `Revisioned` interface, which `ItemView` implements. To wait for revisions of a view of your own, implement it as well.

#### Comparing Revisions

To compare two revisions, call the `CompareRevisions` function. Like `cmp.Compare`, it returns `-1`, `0`, or `1`. An empty revision comes before every other one. If a value is not a revision, it returns `ErrNotARevision`:

```go
result, err := architecturekit.CompareRevisions("9", "10")
// result == -1
```

### Setting Up an HTTP API

To expose commands and queries over HTTP, use the `httpapi` package:

```go
import "github.com/thenativeweb/architecturekit-golang/architecturekit/httpapi"
```

Define a type that describes who is making a request, and a function that determines it from the request. Then call the `NewAPI` function with the store and this function, and create a mux:

```go
type User struct {
  ID          string
  IsLibrarian bool
}

func userFrom(r *http.Request) (User, error) {
  // ...
}

api := httpapi.NewAPI(store, userFrom)
mux := http.NewServeMux()
```

If the function returns an error, the request is answered with `401 Unauthorized`, and neither a command nor a query is run.

For an application without authentication, call the `NewPublicAPI` function instead. Commands and queries then receive `httpapi.NoUser` as user:

```go
api := httpapi.NewPublicAPI(store)
```

#### Determining the User

To determine the user in a handler of your own, call the `UserOf` function. If the user cannot be determined, it returns an error that wraps `httpapi.ErrUnauthorized`:

```go
mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
  user, err := httpapi.UserOf(r, api)
  if err != nil {
    // ...
  }

  // ...
})
```

### Handling Commands over HTTP

To accept a command over HTTP, define a request type with JSON annotations, and implement the `ToCommand` function, which receives the user and returns the command:

```go
type borrowBookRequest struct {
  BookID          string `json:"bookId"`
  BorrowedUntil   string `json:"borrowedUntil"`
  ExpectedEventID string `json:"expectedEventId"`
}

func (r borrowBookRequest) ToCommand(user User) (BorrowBook, error) {
  return BorrowBook{
    BookID:          r.BookID,
    ReaderID:        user.ID,
    BorrowedUntil:   r.BorrowedUntil,
    ExpectedEventID: r.ExpectedEventID,
  }, nil
}
```

Then call the `Route` function with the request type, the API, the mux, a pattern, and the decider:

```go
httpapi.Route[borrowBookRequest](api, mux, "POST /api/borrow-book", borrowBook)
```

The route decodes the request body, builds the command, and executes it:

```shell
curl -X POST http://localhost:8080/api/borrow-book \
  -H "Content-Type: application/json" \
  -d '{"bookId":"42","borrowedUntil":"2026-10-24","expectedEventId":"0"}'
```

If this succeeds, it answers with `200 OK` and the IDs of the written events:

```json
{ "eventIds": [ "1" ], "message": "ok" }
```

Otherwise, it answers with the status code that matches the error (see [Mapping Errors to Status Codes](#mapping-errors-to-status-codes)) and the error message. For status codes of `500` and above, the message is `internal server error`.

To answer this way in a handler of your own, call the `Respond` function with the response writer, the written events, and the error.

#### Answering Commands in Your Own Format

To answer in a format of your own, call the `Handle` function in a handler of your own. It does the same as a route, but writes nothing to the response. Instead, it returns a `Handled` value with the command it has built and the written events.

This allows you to answer with something that `ToCommand` has generated, for example the ID of a new book:

```go
type acquireBookRequest struct {
  Title  string `json:"title"`
  Author string `json:"author"`
  ISBN   string `json:"isbn"`
}

func (r acquireBookRequest) ToCommand(user User) (AcquireBook, error) {
  return AcquireBook{
    BookID: rand.Text(),
    Title:  r.Title,
    Author: r.Author,
    ISBN:   r.ISBN,
  }, nil
}

mux.HandleFunc("POST /api/acquire-book", func(w http.ResponseWriter, r *http.Request) {
  handled, err := httpapi.Handle[acquireBookRequest](r, api, acquireBook)
  if err != nil {
    httpapi.Respond(w, nil, err)
    return
  }

  w.Header().Set("Content-Type", "application/json")
  w.WriteHeader(http.StatusCreated)
  json.NewEncoder(w).Encode(map[string]string{
    "id": handled.Command.BookID,
  })
})
```

*Note that `Handled` contains the command even if executing it fails.*

#### Authorizing Commands

To refuse a command, return `httpapi.ErrForbidden` from `ToCommand`. The request is then answered with `403 Forbidden`, and the command is not executed:

```go
func (r acquireBookRequest) ToCommand(user User) (AcquireBook, error) {
  if !user.IsLibrarian {
    return AcquireBook{}, httpapi.ErrForbidden
  }

  return AcquireBook{
    BookID: rand.Text(),
    Title:  r.Title,
    Author: r.Author,
    ISBN:   r.ISBN,
  }, nil
}
```

The same applies to the other errors of the `httpapi` package, such as `httpapi.ErrNotFound`, and to errors of the category `ErrDomain`: they keep their status code. Any other error returned from `ToCommand` is answered with `400 Bad Request`.

#### Validating Requests

Before a request reaches `ToCommand`, it is validated:

- The `Content-Type` header must be `application/json`, otherwise the request is answered with `415 Unsupported Media Type`, and the error is `httpapi.ErrUnsupportedMediaType`.
- The body must not be larger than `httpapi.MaxRequestBody`, which is one mebibyte, otherwise the request is answered with `413 Request Entity Too Large`, and the error is `httpapi.ErrTooLarge`.
- The body must be valid JSON without unknown fields, otherwise the request is answered with `400 Bad Request`, and the error is `httpapi.ErrMalformed`.

### Handling Queries over HTTP

To answer a query over HTTP, define a function that receives the request and the user, and returns the query:

```go
toListBooks := func(r *http.Request, user User) (ListBooks, error) {
  return ListBooks{
    OnlyAvailable: r.URL.Query().Get("available") == "true",
  }, nil
}
```

Then call the `Query` function with the API, the mux, a pattern, this function, and the function that answers the query:

```go
httpapi.Query(api, mux, "GET /api/books", toListBooks, listBooks(catalog))
```

The route answers with `200 OK` and the result as JSON. Errors are answered as for commands, and errors returned from the first function are treated as they are from `ToCommand` (see [Authorizing Commands](#authorizing-commands)).

To answer this way in a handler of your own, call the `RespondResult` function with the response writer, the result, and the error.

*Note that the functions have the types `httpapi.ToQuery` and `httpapi.Answer`. The answering function receives neither the request nor the user.*

#### Answering Queries in Your Own Format

To answer in a format of your own, call the `Ask` function in a handler of your own. It does the same as a route, but writes nothing to the response. Instead, it returns the result:

```go
mux.HandleFunc("GET /api/books", func(w http.ResponseWriter, r *http.Request) {
  books, err := httpapi.Ask(r, api, toListBooks, listBooks(catalog))
  if err != nil {
    // ...
  }

  // ...
})
```

#### Reporting Missing Items

If the answering function returns `query.ErrNoItems`, as `query.Single` does if no item matches, the request is answered with `404 Not Found`:

```go
httpapi.Query(
  api,
  mux,
  "GET /api/books/{id}",
  func(r *http.Request, user User) (GetBook, error) {
    return GetBook{BookID: r.PathValue("id")}, nil
  },
  getBook(catalog),
)
```

To report a missing item yourself, return `httpapi.ErrNotFound`.

### Mapping Errors to Status Codes

To get the status code that matches an error, call the `StatusFor` function:

```go
status := httpapi.StatusFor(err)
```

It checks the categories in this order:

| Error | Status code |
|---|---|
| `nil` | `200 OK` |
| `httpapi.ErrUnauthorized` | `401 Unauthorized` |
| `httpapi.ErrForbidden` | `403 Forbidden` |
| `httpapi.ErrTooLarge` | `413 Request Entity Too Large` |
| `httpapi.ErrUnsupportedMediaType` | `415 Unsupported Media Type` |
| `httpapi.ErrMalformed` | `400 Bad Request` |
| `httpapi.ErrNotFound`, `query.ErrNoItems` | `404 Not Found` |
| `architecturekit.ErrDomain` | `422 Unprocessable Entity` |
| `architecturekit.ErrConflict` | `409 Conflict` |
| `architecturekit.ErrTransient` | `503 Service Unavailable` |
| any other error | `500 Internal Server Error` |

### Reading Your Own Writes over HTTP

To let a caller read its own writes over HTTP, call the `QueryRevisioned` function instead of `Query`. Additionally, hand over a view that implements `Revisioned`, whose projection is tracked (see [Tracking Revisions](#tracking-revisions)), and how long to wait at most:

```go
httpapi.QueryRevisioned(
  api,
  mux,
  "GET /api/books",
  catalog,
  toListBooks,
  listBooks(catalog),
  httpapi.DefaultWait,
)
```

After sending a command, the caller takes the highest ID from `eventIds` and sends it in the `Wait-For-Revision` header of the query:

```shell
curl http://localhost:8080/api/books \
  -H "Wait-For-Revision: 1"
```

The route waits until the view has reached this revision, but at most for the given duration, which is five seconds for `httpapi.DefaultWait`. Then it answers with what the view holds, even if the time has run out. If the header does not contain a revision, the request is answered with `400 Bad Request`.

Once the view has seen at least one event, the response contains the revision it shows in the `X-Revision` header, as well as an `ETag` header and `Cache-Control: no-cache`. If the caller sends the `ETag` in the `If-None-Match` header and the view has not changed since, the request is answered with `304 Not Modified`.

*Note that the constants `httpapi.HeaderWaitFor` and `httpapi.HeaderRevision` contain the names of the two headers.*

#### Depending on More Than the Read Model

If an answer depends on more than the view, for example on the current date, call the `QueryVarying` function instead, and additionally hand over a function of the type `httpapi.Volatile`. It receives the request and returns a value that changes whenever the answer would, and that becomes part of the `ETag`:

```go
type ListOverdueBooks struct {
  Today string
}

func listOverdueBooks(catalog architecturekit.View[BookItem]) func(context.Context, ListOverdueBooks) ([]BookItem, error) {
  return func(ctx context.Context, q ListOverdueBooks) ([]BookItem, error) {
    items, err := catalog.All(ctx)
    if err != nil {
      return nil, err
    }

    return slices.Collect(query.Where(items, func(item BookItem) bool {
      return item.IsBorrowed && item.BorrowedUntil < q.Today
    })), nil
  }
}

func today(*http.Request) string {
  return time.Now().Format(time.DateOnly)
}

httpapi.QueryVarying(
  api,
  mux,
  "GET /api/overdue-books",
  catalog,
  func(r *http.Request, user User) (ListOverdueBooks, error) {
    return ListOverdueBooks{Today: today(r)}, nil
  },
  listOverdueBooks(catalog),
  httpapi.DefaultWait,
  today,
)
```

#### Building Your Own Revisioned Handler

To build a handler of your own that works like `QueryRevisioned`, use these three functions:

- `Await` waits for the revision the request asks for. Running out of time is not an error. It returns an error if the header does not contain a revision, or if waiting fails for another reason.
- `ServeUnchanged` answers with `304 Not Modified` if the caller already holds the given revision, and reports whether it did.
- `RespondResultAt` answers like `RespondResult`, and adds the headers for the given revision.

The last argument of `ServeUnchanged` and `RespondResultAt` is a `Volatile` function, or `nil`:

```go
mux.HandleFunc("GET /api/books", func(w http.ResponseWriter, r *http.Request) {
  if _, err := httpapi.UserOf(r, api); err != nil {
    httpapi.RespondResult(w, struct{}{}, err)
    return
  }

  if err := httpapi.Await(r.Context(), r, catalog, httpapi.DefaultWait); err != nil {
    httpapi.RespondResult(w, struct{}{}, err)
    return
  }

  revision := catalog.Revision()

  if httpapi.ServeUnchanged(w, r, revision, nil) {
    return
  }

  books, err := httpapi.Ask(r, api, toListBooks, listBooks(catalog))
  httpapi.RespondResultAt(w, r, revision, books, err, nil)
})
```

### Testing Deciders

To test deciders without a database, use the `architecturekittest` package:

```go
import "github.com/thenativeweb/architecturekit-golang/architecturekit/architecturekittest"
```

Call the `Given` function with a `*testing.T`, the decider, and the events that have happened so far. Then call the `When` function with the command, and check the outcome:

```go
func TestBorrowBook(t *testing.T) {
  architecturekittest.Given(t, borrowBook,
    BookAcquired{
      Title:  "2001 – A Space Odyssey",
      Author: "Arthur C. Clarke",
      ISBN:   "978-0756906788",
    },
  ).
    When(BorrowBook{
      BookID:          "42",
      ReaderID:        "23",
      BorrowedUntil:   "2026-10-24",
      ExpectedEventID: "0",
    }).
    ThenEvents(BookBorrowed{
      BorrowedBy:    "23",
      BorrowedUntil: "2026-10-24",
    })
}
```

`Given` returns a `*Fixture`, and `When` returns an `*Outcome`. The functions that check the outcome return the outcome again, so they can be chained.

*Note that `Given` accepts any value that provides the `Helper` and `Fatalf` functions, as described by the `TestingT` interface.*

#### Expecting Events

To expect exactly the given events, in the given order, call the `ThenEvents` function, as shown above. To expect neither events nor an error, call the `ThenNothing` function:

```go
architecturekittest.Given(t, returnBook, BookAcquired{}).
  When(ReturnBook{BookID: "42"}).
  ThenNothing()
```

To check the events with a function, call the `ThenSomeEvent` function to expect at least one matching event, the `ThenEveryEvent` function to expect only matching events, and at least one, or the `ThenNoEvent` function to expect no matching event:

```go
isBookBorrowed := func(event architecturekit.Event) bool {
  _, ok := event.(BookBorrowed)
  return ok
}

architecturekittest.Given(t, borrowBook, BookAcquired{}).
  When(BorrowBook{BookID: "42", ReaderID: "23"}).
  ThenEveryEvent(isBookBorrowed)
```

#### Expecting Rejections

To expect that a command is rejected with exactly the given message, call the `ThenRejected` function:

```go
architecturekittest.Given(t, acquireBook, BookAcquired{}).
  When(AcquireBook{BookID: "42"}).
  ThenRejected("book 42 has already been acquired")
```

To expect an error of a category instead, call the `ThenFailed` function:

```go
architecturekittest.Given(t, borrowBook).
  When(BorrowBook{BookID: "42"}).
  ThenFailed(architecturekit.ErrDomain)
```

#### Expecting Preconditions

To expect exactly the given preconditions, in the given order, call the `ThenPreconditions` function. Describe the preconditions with the `OnSubject`, `OnEventID`, and `OnQuery` functions:

```go
architecturekittest.Given(t, borrowBook, BookAcquired{}).
  When(BorrowBook{BookID: "42", ReaderID: "23", ExpectedEventID: "0"}).
  ThenPreconditions(architecturekittest.OnEventID("/books/42", "0"))
```

For a command without preconditions, call `ThenPreconditions` without arguments.

To get the preconditions of a command directly, call the `PreconditionsOf` function. It returns a slice of `Precondition`, with the fields `Subject`, `EventID`, and `Query`:

```go
preconditions := architecturekittest.PreconditionsOf(ReturnBook{BookID: "42"})
```

*Note that the preconditions created with `NewIsSubjectPristinePrecondition` and `NewIsSubjectPopulatedPrecondition` can not be told apart. Both are described with `OnSubject`.*

#### Inspecting State

To check the state the command has been decided on, call the `ThenState` function with a function that receives the state:

```go
architecturekittest.Given(t, returnBook, BookAcquired{}, BookBorrowed{}).
  When(ReturnBook{BookID: "42"}).
  ThenState(func(book Book) {
    if !book.IsBorrowed {
      t.Fatal("expected the book to be borrowed")
    }
  })
```

#### Testing Upcasters

To test an upcaster, call the `GivenStored` function instead of `Given`, and hand over the events as they are stored. They run through the upcasters, as they do when reading from the database. To turn a typed event into a stored one, call the `StoredEvent` function with the subject, the event ID, and the event:

```go
architecturekittest.GivenStored(t, borrowBook,
  architecturekittest.StoredEvent("/books/42", "0", BookAcquired{
    Title:  "2001 – A Space Odyssey",
    Author: "Arthur C. Clarke",
    ISBN:   "978-0756906788",
  }),
  eventsourcingdb.Event{
    Subject: "/books/42",
    Type:    "io.eventsourcingdb.library.book-lent",
    ID:      "1",
    Data:    json.RawMessage(`{"lentTo":"23","until":"2026-10-24"}`),
  },
).
  When(BorrowBook{BookID: "42", ReaderID: "17"}).
  ThenRejected("book 42 is already borrowed")
```

#### Replaying Events Directly

To evolve a state from events without a decider, call the `Replay` function with the state and typed events, or the `ReplayStored` function with the state and stored events:

```go
book, err := architecturekit.Replay(bookState, BookAcquired{}, BookBorrowed{})
if err != nil {
  // ...
}
```

### Testing Projections

To test a projection without a database, call the `Project` function with a `*testing.T`, the projection, and the stored events. To turn typed events for the same subject into stored ones, call the `StoredEvents` function, which numbers them from `0`:

```go
func TestCatalogProjection(t *testing.T) {
  catalog := architecturekit.NewItemView[BookItem]()
  catalogProjection := CatalogProjection{catalog: catalog}

  architecturekittest.Project(t, catalogProjection,
    architecturekittest.StoredEvents("/books/42",
      BookAcquired{
        Title:  "2001 – A Space Odyssey",
        Author: "Arthur C. Clarke",
        ISBN:   "978-0756906788",
      },
      BookBorrowed{
        BorrowedBy:    "23",
        BorrowedUntil: "2026-10-24",
      },
    )...,
  )

  architecturekittest.ExpectItems(t, catalog, BookItem{
    ID:            "42",
    Title:         "2001 – A Space Odyssey",
    Author:        "Arthur C. Clarke",
    IsBorrowed:    true,
    BorrowedUntil: "2026-10-24",
    EventID:       "1",
  })
}
```

The `ExpectItems` function expects the view to hold exactly the given items, in the given order. It requires an item type that is comparable. To get the items as a slice instead, call the `ItemsOf` function:

```go
items := architecturekittest.ItemsOf(t, catalog)
```

To check how a projection will be run, call the `ExpectMode` function:

```go
architecturekittest.ExpectMode(t, catalogProjection, architecturekit.ModeRebuild)
```

To test a transactional projection, call the `ProjectTransactional` function instead of `Project`. It begins a transaction, applies the events, and commits the transaction with the ID of the last event. If an event is refused, it rolls the transaction back:

```go
architecturekittest.ProjectTransactional(t, &TransactionalBookTableProjection{},
  architecturekittest.StoredEvents("/books/42",
    BookAcquired{
      Title:  "2001 – A Space Odyssey",
      Author: "Arthur C. Clarke",
      ISBN:   "978-0756906788",
    },
  )...,
)
```

*Note that `Project` fails the test for a projection that implements `Transactional` in addition to `Apply`.*

### Testing Queries

To test a query, call the function that answers it directly, with a view filled by a projection:

```go
books, err := listBooks(catalog)(context.TODO(), ListBooks{OnlyAvailable: true})
if err != nil {
  // ...
}
```
