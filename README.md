# architecturekit

Building blocks for DDD-based applications with CQRS and event sourcing in Go, on top of [EventSourcingDB](https://www.eventsourcingdb.io) – a purpose-built database for event sourcing.

architecturekit covers both sides of an event-sourced application: commands, events, and the state to decide on for writing, and projections, views, and queries for reading. An optional package exposes commands and queries over HTTP.

For more information on EventSourcingDB, see its [official documentation](https://docs.eventsourcingdb.io/).

architecturekit includes a test package to test deciders, projections, and queries without a database. For details, see [Testing Deciders](#testing-deciders).

## Getting Started

Install the packages:

```shell
go get github.com/thenativeweb/architecturekit-golang/architecturekit github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb
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

A command describes what someone wants to do. Define it as a struct and implement two functions: `Subject`, which returns the subject the command acts on, and `Preconditions`, which returns the conditions under which its events may be written. This makes the struct a `Command`:

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

func (c AcquireBook) Preconditions() []architecturekit.Precondition {
  return []architecturekit.Precondition{
    architecturekit.OnStateRead(),
  }
}

type BorrowBook struct {
  BookID        string
  ReaderID      string
  BorrowedUntil string
}

func (c BorrowBook) Subject() string {
  return "/books/" + c.BookID
}

func (c BorrowBook) Preconditions() []architecturekit.Precondition {
  return []architecturekit.Precondition{
    architecturekit.OnStateRead(),
  }
}

type ReturnBook struct {
  BookID string
}

func (c ReturnBook) Subject() string {
  return "/books/" + c.BookID
}

func (c ReturnBook) Preconditions() []architecturekit.Precondition {
  return []architecturekit.Precondition{
    architecturekit.OnStateRead(),
  }
}
```

`OnStateRead` makes sure that the events are only written if the state the command is decided on still holds. For the other preconditions, see [Using Preconditions](#using-preconditions).

### Defining Events

An event describes what has happened. Define it as a struct with JSON annotations and implement two functions: `EventType`, which returns the event type, and `Schema`, which returns a JSON schema for the event's data. This makes the struct an `Event`:

```go
type BookAcquired struct {
  Title  string `json:"title"`
  Author string `json:"author"`
  ISBN   string `json:"isbn"`
}

func (BookAcquired) EventType() string {
  return "io.eventsourcingdb.library.book-acquired"
}

func (BookAcquired) Schema() map[string]any {
  return map[string]any{
    "type": "object",
    "properties": map[string]any{
      "title":  map[string]any{"type": "string"},
      "author": map[string]any{"type": "string"},
      "isbn":   map[string]any{"type": "string"},
    },
    "required":             []string{"title", "author", "isbn"},
    "additionalProperties": false,
  }
}

type BookBorrowed struct {
  BorrowedBy    string `json:"borrowedBy"`
  BorrowedUntil string `json:"borrowedUntil"`
}

func (BookBorrowed) EventType() string {
  return "io.eventsourcingdb.library.book-borrowed"
}

func (BookBorrowed) Schema() map[string]any {
  return map[string]any{
    "type": "object",
    "properties": map[string]any{
      "borrowedBy":    map[string]any{"type": "string"},
      "borrowedUntil": map[string]any{"type": "string", "format": "date"},
    },
    "required":             []string{"borrowedBy", "borrowedUntil"},
    "additionalProperties": false,
  }
}

type BookReturned struct{}

func (BookReturned) EventType() string {
  return "io.eventsourcingdb.library.book-returned"
}

func (BookReturned) Schema() map[string]any {
  return map[string]any{
    "type":                 "object",
    "properties":           map[string]any{},
    "additionalProperties": false,
  }
}
```

The struct becomes the event's data. The subject is taken from the command, and the source from the store. The database checks every event against the schema of its type, once the schema is registered (see [Registering Event Schemas](#registering-event-schemas)).

*Note that the schema is required, so that no event type can be forgotten. An event without a `Schema` function does not compile.*

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

Every command declares at least one precondition, so that writing without any check is always a decision, never an oversight. There are three kinds:

- `OnStateRead` guards the state the command is decided on.
- `Require` turns a precondition of the client SDK into one of the command, for example to check a revision the caller hands over.
- `Unconditionally` writes without any check.

Preconditions can be combined, and all of them must hold. If a precondition does not hold, nothing is written, and `Execute` returns an error of the category `ErrConflict` (see [Handling Errors](#handling-errors)). If a command declares no preconditions, or combines `Unconditionally` with others, `Execute` returns an error of the category `ErrPermanent` before reading anything.

#### Guarding Against Concurrent Changes

If a command may only write events in case nothing has been written to its subject since `Execute` read the state, use the `OnStateRead` function. This fits most commands, since the decider decides on exactly that state:

```go
func (c ReturnBook) Preconditions() []architecturekit.Precondition {
  return []architecturekit.Precondition{
    architecturekit.OnStateRead(),
  }
}
```

`Execute` fills in the ID of the last event it has read. For a subject without any events, it requires the subject to still be pristine instead. This also holds with several processes writing to the same subjects, and with a state cache (see [Caching States](#caching-states)).

#### Checking the Revision of the Caller

If a command may only write events in case its subject has not changed since the caller last read it, for example in a user interface, use the `NewIsSubjectOnEventIDPrecondition` function of the client SDK, and wrap it with the `Require` function. For that, add a field for the ID of the last event the caller has seen:

```go
type BorrowBook struct {
  BookID          string
  ReaderID        string
  BorrowedUntil   string
  ExpectedEventID string
}

func (c BorrowBook) Preconditions() []architecturekit.Precondition {
  return []architecturekit.Precondition{
    architecturekit.Require(eventsourcingdb.NewIsSubjectOnEventIDPrecondition(c.Subject(), c.ExpectedEventID)),
  }
}
```

*Note that the caller has to provide the event ID. A view can keep it for that purpose (see [Defining Views](#defining-views)).*

#### Preventing Duplicates

If a command may only write events in case its subject does not yet have any events, use the `NewIsSubjectPristinePrecondition` function of the client SDK:

```go
func (c AcquireBook) Preconditions() []architecturekit.Precondition {
  return []architecturekit.Precondition{
    architecturekit.Require(eventsourcingdb.NewIsSubjectPristinePrecondition(c.Subject())),
  }
}
```

#### Requiring an Existing Subject

If a command may only write events in case its subject already has at least one event, use the `NewIsSubjectPopulatedPrecondition` function of the client SDK. Combine it with `OnStateRead` to also guard the state:

```go
func (c ReturnBook) Preconditions() []architecturekit.Precondition {
  return []architecturekit.Precondition{
    architecturekit.Require(eventsourcingdb.NewIsSubjectPopulatedPrecondition(c.Subject())),
    architecturekit.OnStateRead(),
  }
}
```

#### Enforcing Rules Across Subjects

If a command may only write events depending on an EventQL query, use the `NewIsEventQLQueryTruePrecondition` function of the client SDK. For example, to acquire every ISBN only once, extend the preconditions of `AcquireBook`:

```go
func (c AcquireBook) Preconditions() []architecturekit.Precondition {
  return []architecturekit.Precondition{
    architecturekit.Require(eventsourcingdb.NewIsSubjectPristinePrecondition(c.Subject())),
    architecturekit.Require(eventsourcingdb.NewIsEventQLQueryTruePrecondition(fmt.Sprintf(
      "FROM e IN events WHERE e.type == 'io.eventsourcingdb.library.book-acquired' AND e.data.isbn == '%s' PROJECT INTO COUNT() == 0",
      c.ISBN,
    ))),
  }
}
```

*Note that the query must return a single row with a single value, which is interpreted as a boolean.*

#### Writing Unconditionally

If a command may write its events whatever has been written to its subject in the meantime, for example because it only records a comment that does not depend on the state, use the `Unconditionally` function:

```go
func (c CommentOnBook) Preconditions() []architecturekit.Precondition {
  return []architecturekit.Precondition{
    architecturekit.Unconditionally(),
  }
}
```

*Note that `Unconditionally` can not be combined with other preconditions.*

### Handling Errors

Every error architecturekit returns belongs to one of four categories. Use `errors.Is` to check for a category rather than for a concrete error:

- `ErrDomain` means that a business rule rejected the command, as with `NewDomainError`.
- `ErrConflict` means that a precondition did not hold.
- `ErrTransient` means that trying again may help, for example if reading from the database failed.
- `ErrPermanent` means that trying again will not help, for example if an event could not be decoded, if it does not match the schema of its type, or if a subject contains an event type the state has no `Evolve` rule for.

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

*Note that `ErrUnverified` is a special case of `ErrPermanent`, which means that an event failed its verification (see [Verifying Events](#verifying-events)). Since that may point to a security incident rather than a mistake, check for it before `ErrPermanent` if you want to treat it differently, for example to raise an alarm.*

*Note that `Execute` does not retry. To try again, for example after a conflict, call `Execute` again.*

### Registering Event Schemas

The database only checks events against a schema once it is registered. The `Evolve` function collects the schemas of all events of a state. To get them as a slice of `EventSchema`, each with the fields `EventType` and `Schema`, call the `Schemas` function on the state. Then hand them over to the `RegisterSchemas` function of the store:

```go
err := store.RegisterSchemas(bookState.Schemas())
if err != nil {
  // ...
}
```

`RegisterSchemas` accepts the schemas of several states at once. Call it on every start, before the application serves requests: for an event type the database knows already, it checks that the registered schema is exactly the one from the code.

A registered schema can not change. If it differs from the one from the code, `RegisterSchemas` returns an error of the category `ErrPermanent`, and so it does if the database refuses a schema, for example because stored events of the type do not match it. To change the shape of an event, introduce a new event type instead (see [Versioning Events](#versioning-events)).

Once a schema is registered, the database rejects every event of the type that does not match it. `Execute` then returns an error of the category `ErrPermanent`, because writing the same event again gives the same result.

### Versioning Events

The database keeps the schema of an event type forever. If the shape of an event changes, introduce a new event type, and translate the stored events of the old type with an upcaster.

Suppose an earlier version of the library wrote events of the type `io.eventsourcingdb.library.book-lent`, with the fields `lentTo` and `until`. To translate them into `BookBorrowed` events, create a set of upcasters with the `NewUpcasters` function. Then call the `Upcast` function on the set, and hand over the old event type and a function that receives the stored event and returns the translated events:

```go
var libraryUpcasters = architecturekit.NewUpcasters().
  Upcast(
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

The function has the type `Upcaster`. Upcasters may return more than one event. If a returned event has an upcaster of its own, that one runs as well, so every version needs only a single step to the next one. The translated events are never written back.

To use the upcasters, call the `UpcastWith` function on the state and hand over the set. The upcasters then run before the `Evolve` rules:

```go
bookState.UpcastWith(libraryUpcasters)
```

Upcasting belongs to the event types, not to a single state, so register the upcasters once and hand the same set to every state, and to every projection that reads these events (see [Defining Projections](#defining-projections)). That way, the write side and the read side see the same events.

*Note that calling `Upcast` twice for the same event type panics, as does calling `UpcastWith` twice, or with `nil`.*

### Reading Long Streams

By default, `Execute` reads all events of a subject every time it runs a command. For a subject that collects many events over time, this gets slower with every event. If an event of one type carries everything the state needs from the events before it, the state can start from the latest event of that type instead.

Suppose every book is audited regularly, and every audit records the complete status of the book:

```go
type BookAudited struct {
  IsAcquired bool `json:"isAcquired"`
  IsBorrowed bool `json:"isBorrowed"`
}

func (BookAudited) EventType() string {
  return "io.eventsourcingdb.library.book-audited"
}

func (BookAudited) Schema() map[string]any {
  return map[string]any{
    "type": "object",
    "properties": map[string]any{
      "isAcquired": map[string]any{"type": "boolean"},
      "isBorrowed": map[string]any{"type": "boolean"},
    },
    "required":             []string{"isAcquired", "isBorrowed"},
    "additionalProperties": false,
  }
}
```

Add an `Evolve` rule that sets the state from the event alone, without looking at the state before it. Then call the `FromLatest` function on the state, with the event type as type parameter:

```go
bookState.
  Evolve(func(book Book, event BookAudited) Book {
    return Book{
      IsAcquired: event.IsAcquired,
      IsBorrowed: event.IsBorrowed,
    }
  }).
  FromLatest[BookAudited]()
```

`Execute` then reads the events of a subject from its latest `BookAudited` event onwards. If the subject has no such event yet, it reads all events, as before. `Replay` and `ReplayStored` start from the same event, so tests of a decider see exactly what `Execute` sees (see [Testing Deciders](#testing-deciders)).

*Note that the state is only correct if the `Evolve` rule of that event type does not depend on the state before it. Events before the latest one of that type are never read.*

*Note that the database looks for the type under which an event is stored. If the event type is the result of an upcaster, events stored under the older type are not found, and all events are read.*

*Note that calling `FromLatest` for an event type without an `Evolve` rule, or calling it twice, panics.*

### Caching States

If no event carries the whole state, the store can keep the states of the most recently used subjects in memory instead, so that the next command on one of them reads only the events written since. To do so, hand over the `WithStateCache` option with the number of subjects to keep when creating the store:

```go
store := architecturekit.NewStore(client, "https://library.eventsourcingdb.io", architecturekit.WithStateCache(10_000))
```

Once the cache is full, the least recently used subject makes room. Such a subject is read in full again the next time, or from the latest event of the type given to `FromLatest`, if there is one (see [Reading Long Streams](#reading-long-streams)).

The cache only holds what was read, never what a command has written. Every command reads all events after the ones its state was built from, including those written by other processes, so the cache stays correct if several processes write to the same subjects.

A cached state is handed to several commands, possibly at the same time. That is safe for a state that consists of values only, such as the `Book` state above. A state that holds slices, maps or pointers is only cached if it has a `Clone` function, which returns a copy that shares no data with the original:

```go
type Shelf struct {
  BookIDs []string
}

var shelfState = architecturekit.NewState(Shelf{}).
  Evolve(func(shelf Shelf, event BookShelved) Shelf {
    shelf.BookIDs = append(shelf.BookIDs, event.BookID)
    return shelf
  }).
  Clone(func(shelf Shelf) Shelf {
    return Shelf{BookIDs: slices.Clone(shelf.BookIDs)}
  })
```

Without a `Clone` function, such a state is read as without a cache.

*Note that a `time.Time` counts as a value, since its location never changes.*

*Note that values below `1` count as `1`, and that calling `Clone` twice panics.*

### Loading States

To read the state of a single subject without executing a command, for example to answer a query about it, call the `Load` function with a context, the store, the state, and the subject:

```go
book, err := architecturekit.Load(context.TODO(), store, bookState, "/books/42")
if err != nil {
  // ...
}
```

`Load` reads the events exactly the way `Execute` does before it decides, including `FromLatest` and the state cache. For a query across many subjects, use a view instead (see [Defining Views](#defining-views)).

### Verifying Events

EventSourcingDB gives every event a hash, which covers its metadata, its data, and the hash of the event before it. If the database runs with a signing key, it also signs every event it hands out. To have the store check the hash of every event it reads, hand over the `WithHashVerification` option when creating the store:

```go
store := architecturekit.NewStore(client, "https://library.eventsourcingdb.io", architecturekit.WithHashVerification())
```

To check the signatures as well, hand over the `WithSignatureVerification` option with the verification key of the database instead. It checks the hashes, too:

```go
store := architecturekit.NewStore(client, "https://library.eventsourcingdb.io", architecturekit.WithSignatureVerification(verificationKey))
```

The verification key is an `ed25519.PublicKey`. To read it from a PEM file, use the `pem` and `x509` packages of the standard library:

```go
block, _ := pem.Decode(pemBytes)
if block == nil {
  // ...
}

publicKey, err := x509.ParsePKIXPublicKey(block.Bytes)
if err != nil {
  // ...
}

verificationKey, ok := publicKey.(ed25519.PublicKey)
if !ok {
  // ...
}
```

The store checks every event it reads, for `Execute` and `Load` as well as for every kind of projection, and it does so before any upcaster sees the event. The events that `Execute` has just written are not checked, since they are not read. If an event fails its verification, reading fails with an error of the category `ErrUnverified`, which is a special case of `ErrPermanent` (see [Handling Errors](#handling-errors)). A projection stops rather than skipping the event.

The two checks prove different things:

- A matching hash proves that an event is what was written. It proves no more than that, since whoever can change the stored data can compute a new hash as well.
- A matching signature proves that an event comes from a database that holds the signing key. The database signs events when handing them out, so the signature guards the way from the database to your application, but not the stored data itself.

Check the hashes wherever reading is under your control, and the signatures where events cross a trust boundary, for example when reading from a database that another organization runs. For details, see [Verifying Event Signatures](https://www.eventfoundation.io/docs/eventsourcingdb/verifying-event-signatures).

*Note that checking a signature takes some tens of microseconds per event, which adds up when a projection catches up on millions of events.*

*Note that the database signs with the key it has at the moment, so after the signing key is rotated, the store needs the new verification key.*

*Note that neither check detects a history that has been rewritten as a whole. For that, audit the chain of hashes (see [Auditing the Event Store](https://www.eventfoundation.io/docs/eventsourcingdb/auditing-the-event-store)).*

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

Values that come from outside, such as an ID in a request, may well be empty or contain a slash, and that is not a programming error. To check them before building a subject, call the `Check` function with the same values as `Build`. It returns an error that says what is wrong, instead of panicking:

```go
if err := bookSubject.Check(bookID); err != nil {
  // ...
}
```

### Defining Views

A view holds the data that queries read. Define the shape of an item as a struct, and call the `NewInMemoryView` function with a function that returns the key of an item, to create a view that holds such items in memory:

```go
type BookItem struct {
  ID            string `json:"id"`
  Title         string `json:"title"`
  Author        string `json:"author"`
  IsBorrowed    bool   `json:"isBorrowed"`
  BorrowedUntil string `json:"borrowedUntil"`
  EventID       string `json:"eventId"`
}

func newCatalog() *architecturekit.InMemoryView[string, BookItem] {
  return architecturekit.NewInMemoryView(
    func(item BookItem) string { return item.ID },
    architecturekit.RevisionIn(func(item *BookItem) *string { return &item.EventID }),
  )
}

catalog := newCatalog()
```

Every item has a revision of its own, which is the ID of the last event that changed it. The `RevisionIn` option makes the view keep it in a field of the item, so that a caller can hand it over to a command that uses the `NewIsSubjectOnEventIDPrecondition` function (see [Checking the Revision of the Caller](#checking-the-revision-of-the-caller)). The view sets the field whenever it changes an item, so you never set it yourself. Without the option, the view keeps the revisions to itself.

Every function that changes the view takes the ID of the event it applies. An event that is not newer than the item it is about is skipped, so applying the same event twice changes nothing. All functions take a context and return an error, which the view in memory hardly needs, but a view in a database would. So a view in a database can offer the same functions later on, without the projections that write to it having to change.

*Note that the view as a whole has a revision as well, which is the last event it has seen at all, rather than the last one that changed a particular item (see [Reading Your Own Writes](#reading-your-own-writes)).*

#### Adding Items

To add an item, call the `Insert` function with a context, the ID of the event, and the item:

```go
err := catalog.Insert(ctx, event.ID, BookItem{
  ID:     "42",
  Title:  "2001 – A Space Odyssey",
  Author: "Arthur C. Clarke",
})
if err != nil {
  // ...
}
```

If the key of the item is already taken, and the event is newer than the item with that key, `Insert` fails with an error of the category `ErrPermanent`, since two items with the same key point to a mistake in the events or in the key. To add an item or change the existing one, call the `Upsert` function instead, and additionally hand over a function that changes the existing item:

```go
err := catalog.Upsert(ctx, event.ID, BookItem{
  ID:         "42",
  IsBorrowed: true,
}, func(item *BookItem) {
  item.IsBorrowed = true
})
```

#### Reading Items

To read the item with a given key, call the `Get` function. It returns `false` if there is none:

```go
book, isFound, err := catalog.Get(ctx, "42")
if err != nil {
  // ...
}
```

To read all items, call the `All` function. It returns an iterator over a copy of the items, in the order in which they were added, which you can use e.g. inside a `for range` loop:

```go
items, err := catalog.All(ctx)
if err != nil {
  // ...
}

for item := range items {
  // ...
}
```

#### Changing and Removing Items

To change the item with a given key, call the `Update` function with a function that changes it. To remove it, call the `Delete` function. Both report whether they changed anything:

```go
isChanged, err := catalog.Update(ctx, "42", event.ID, func(item *BookItem) {
  item.IsBorrowed = true
})

isRemoved, err := catalog.Delete(ctx, "42", event.ID)
```

Neither changes anything if there is no such item, or if the event is not newer than the item. Neither is an error, since both happen when events are applied a second time, as a later event may have removed the item already.

*Note that changing the key of an item fails with an error of the category `ErrPermanent`.*

To change or remove several items at once, call the `UpdateWhere` or the `DeleteWhere` function with a function that selects them. Both return the number of items they changed or removed:

```go
isByClarke := func(item BookItem) bool {
  return item.Author == "Arthur C. Clarke"
}

changed, err := catalog.UpdateWhere(ctx, isByClarke, event.ID, func(item *BookItem) {
  item.Author = "Sir Arthur C. Clarke"
})

removed, err := catalog.DeleteWhere(ctx, isByClarke, event.ID)
```

#### Indexing Items

Selecting items with a function reads every item. To find items by a value directly, for example all books by an author, add a secondary index with the `Index` function. It takes a function that returns the value of an item, and returns an index, which offers the `Lookup`, `Update`, and `Delete` functions:

```go
byAuthor := catalog.Index(func(item BookItem) string { return item.Author })

books, err := byAuthor.Lookup(ctx, "Arthur C. Clarke")

changed, err := byAuthor.Update(ctx, "Arthur C. Clarke", event.ID, func(item *BookItem) {
  item.IsBorrowed = false
})

removed, err := byAuthor.Delete(ctx, "Arthur C. Clarke", event.ID)
```

Several items may share a value. The index follows every change to the view, also when the value of an item changes, and hands out items in the order in which they were added.

*Note that adding an index reads every item, so add indexes before the view is used.*

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

A projection turns events into a view. Call the `NewProjection` function, and call the `On` function for every event type the view depends on. Each handler receives an `Envelope`, which holds the metadata of the event, such as its `ID`, `Time`, and `Subject`, and its data, decoded into the Go type of the event:

```go
func newCatalogProjection(catalog *architecturekit.InMemoryView[string, BookItem]) *architecturekit.TypedProjection {
  return architecturekit.NewProjection().
    On(func(ctx context.Context, event architecturekit.Envelope[BookAcquired]) error {
      return catalog.Insert(ctx, event.ID, BookItem{
        ID:     bookIDOf(event.Subject),
        Title:  event.Data.Title,
        Author: event.Data.Author,
      })
    }).
    On(func(ctx context.Context, event architecturekit.Envelope[BookBorrowed]) error {
      _, err := catalog.Update(ctx, bookIDOf(event.Subject), event.ID, func(item *BookItem) {
        item.IsBorrowed = true
        item.BorrowedUntil = event.Data.BorrowedUntil
      })
      return err
    }).
    On(func(ctx context.Context, event architecturekit.Envelope[BookReturned]) error {
      _, err := catalog.Update(ctx, bookIDOf(event.Subject), event.ID, func(item *BookItem) {
        item.IsBorrowed = false
        item.BorrowedUntil = ""
      })
      return err
    })
}

catalogProjection := newCatalogProjection(catalog)
```

The event type is taken from the event's `EventType` function, so it does not have to be repeated, and the type of an event and the type its data is decoded into can not drift apart.

Which parts of the subject a view needs is up to the application. In this example, a small function takes the ID of the book out of the subject (see [Composing Subjects](#composing-subjects)):

```go
func bookIDOf(subject string) string {
  values, _ := bookSubject.Match(subject)
  return values["book"]
}
```

`NewProjection` returns a `*TypedProjection`, which is a `Projection` like any other, so you can run, track, and test it as described below. Events without a handler are skipped, since a projection usually reads more events than it depends on. If the data of an event can not be decoded, the projection returns an error of the category `ErrPermanent`. An error returned by a handler is passed on unchanged.

To have the projection see the same events as the state, hand over the same set of upcasters with the `UpcastWith` function (see [Versioning Events](#versioning-events)):

```go
catalogProjection.UpcastWith(libraryUpcasters)
```

*Note that calling `On` twice for the same event type panics.*

#### Handling Every Event

A projection that has to see every event, for example to log it, implements the `Projection` interface directly. Its `Apply` function receives every event as it is stored, without running any upcasters:

```go
type LogProjection struct{}

func (LogProjection) Apply(ctx context.Context, event eventsourcingdb.Event) error {
  log.Println(event.Subject, event.Type)
  return nil
}
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

If reading fails, or if the database ends the stream, for example because it restarts, the function waits and continues after the last event it has applied, until the context is canceled. The delay starts at one second, doubles with every attempt in a row, and never exceeds one minute. It starts over once the projection has applied an event again. To use other delays, or to learn about every attempt, for example to log it, hand over the `WithReconnectDelays` and `WithReconnectObserver` options when creating the store:

```go
store := architecturekit.NewStore(client, "https://library.eventsourcingdb.io",
  architecturekit.WithReconnectDelays(500*time.Millisecond, 30*time.Second),
  architecturekit.WithReconnectObserver(func(err error, delay time.Duration) {
    log.Println("observing again", err, delay)
  }),
)
```

The observer receives the reason, which is `nil` if the database ended the stream, and the delay before the next attempt.

*Note that a database that can not be reached is retried as well, since that is usually transient. The observer is how to notice a database that stays unreachable.*

To only apply the events that are already stored, call the `CatchUpProjection` function instead. It takes the same arguments and returns once all stored events have been applied:

```go
err := architecturekit.CatchUpProjection(context.TODO(), store, "/books", true, catalogProjection)
if err != nil {
  // ...
}
```

### Starting Projections

An application usually answers queries only once its views have caught up, since a half-built view answers wrongly rather than slowly. Calling `CatchUpProjection` first and `RunProjection` afterwards reads the whole history twice, and applies it to the view twice. Call the `StartProjection` function instead. It takes the same arguments as `RunProjection`, runs the projection in the background, and returns a `*ProjectionRun` at once:

```go
run := architecturekit.StartProjection(ctx, store, "/books", true, catalogProjection)

select {
case <-run.CaughtUp():
  // The view holds every event that was stored when the run started.
case <-run.Done():
  // The run ended before it caught up.
  return run.Err()
}

// Start to answer queries.
```

`CaughtUp` returns a channel that is closed once the run has applied the events that were stored when it started. It is closed only once, and stays closed while the run reconnects later on. `Done` returns a channel that is closed once the run has ended, which happens when the context ends, or on a failure that trying again will not fix, as with `RunProjection`. `Err` returns why the run has ended. It returns `nil` as long as the run has not ended, and if it ended because its context did.

*Note that if the database can not be reached at the start, the run keeps trying, and `CaughtUp` stays open. To wait for a limited time only, add a case with `time.After` to the `select` statement.*

To find out where a run stands, for example for a health check, call the `Status` function. It returns a `ProjectionStatus` with these fields:

- `Phase` is `PhaseCatchingUp`, `PhaseLive`, `PhaseReconnecting`, or `PhaseStopped`.
- `Since` is when the phase began. For `PhaseReconnecting`, that is when the disruption began, not when the latest attempt did.
- `Err` is why the run is reconnecting or has stopped. It is `nil` if the database ended the stream, or if the run stopped because its context ended.
- `Attempts` counts the attempts to read again within the current disruption.
- `Revision` is the ID of the last event the run has applied and committed.

```go
status := run.Status()

if status.Phase == architecturekit.PhaseReconnecting && time.Since(status.Since) > 5*time.Minute {
  // The view has not been up to date for more than five minutes.
}
```

For a transactional projection, call the `StartTransactionalProjection` function instead (see [Resuming Projections](#resuming-projections)).

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

To make a projection created with `NewProjection` resumable, embed it in a type of your own, and add the two functions there:

```go
type BookTable struct {
  *architecturekit.TypedProjection
  // ...
}

func (t *BookTable) Checkpoint(ctx context.Context) (string, error) {
  // ...
}

func (t *BookTable) SaveCheckpoint(ctx context.Context, eventID string) error {
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

Instead of implementing `Apply` on the `Tx` yourself, you can use handlers created with `NewProjection`. Build them in `Begin`, so that they write into the transaction that has just been started, and embed them in the `Tx`, which then only needs `Commit` and `Rollback`:

```go
func (p *TransactionalBookTableProjection) Begin(ctx context.Context) (architecturekit.Tx, error) {
  tx, err := p.db.BeginTx(ctx, nil)
  if err != nil {
    return nil, err
  }

  return &bookTableTx{
    tx: tx,
    TypedProjection: architecturekit.NewProjection().
      On(func(ctx context.Context, event architecturekit.Envelope[BookAcquired]) error {
        _, err := tx.ExecContext(ctx, "INSERT INTO books ...")
        return err
      }),
  }, nil
}

type bookTableTx struct {
  *architecturekit.TypedProjection
  tx *sql.Tx
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

*Note that the query package reads every item it is handed. To get an item by its key, call the `Get` function of the view instead, and to get items by the value of a secondary index, call the `Lookup` function of the index (see [Defining Views](#defining-views)).*

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

`Tracking` accepts every view that implements the `RevisionSink` interface, which consists of the `Seen` function. `InMemoryView` implements it.

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

Both functions form the `Revisioned` interface, which `InMemoryView` implements. To wait for revisions of a view of your own, implement it as well.

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
  if err := bookSubject.Check(r.BookID); err != nil {
    return BorrowBook{}, err
  }
  if _, err := time.Parse(time.DateOnly, r.BorrowedUntil); err != nil {
    return BorrowBook{}, errors.New("borrowedUntil must be a date")
  }

  return BorrowBook{
    BookID:          r.BookID,
    ReaderID:        user.ID,
    BorrowedUntil:   r.BorrowedUntil,
    ExpectedEventID: r.ExpectedEventID,
  }, nil
}
```

`ToCommand` is the place to validate a request, since an error it returns is answered with `400 Bad Request`. Check at least what would otherwise fail later: the ID of the book becomes part of a subject, and `Build` panics on an empty ID or one with a slash (see [Composing Subjects](#composing-subjects)). And a value that does not match the schema of its event is refused by the database, which is a permanent failure answered with `500 Internal Server Error` – although it is the caller's mistake.

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

To expect exactly the given preconditions, in the given order, call the `ThenPreconditions` function. Describe the preconditions of the kit with the `OnStateRead` and `Unconditionally` functions, and those of the client SDK with the `OnSubject`, `OnEventID`, and `OnQuery` functions:

```go
architecturekittest.Given(t, borrowBook, BookAcquired{}).
  When(BorrowBook{BookID: "42", ReaderID: "23", ExpectedEventID: "0"}).
  ThenPreconditions(architecturekittest.OnEventID("/books/42", "0"))
```

To get the preconditions of a command directly, call the `PreconditionsOf` function. It returns a slice of `Precondition`, with the fields `Subject`, `EventID`, `Query`, `OnStateRead`, and `Unconditional`:

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
  catalog := newCatalog()
  catalogProjection := newCatalogProjection(catalog)

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

`Project` hands over the events as they are stored, so a projection that uses upcasters runs them, just as it does with a database. To test that a projection handles an older event type, hand over an event of that type.

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
