# architecturekit

Building blocks for DDD-based applications with CQRS and event sourcing in Go, on top of [EventSourcingDB](https://www.eventsourcingdb.io) – a purpose-built database for event sourcing.

architecturekit covers both sides of an event-sourced application: commands, events, and the state to decide on for writing, and projections, views, and queries for reading. An optional package exposes commands and queries over HTTP.

For more information on EventSourcingDB, see its [official documentation](https://docs.eventsourcingdb.io/).

architecturekit includes a test package to test deciders, projections, and queries without a database. For details, see [Testing Deciders](#testing-deciders). For the tests that need a real database, a second package starts one in a container (see [Testing with a Database](#testing-with-a-database)).

## Getting Started

architecturekit needs Go 1.27.1 or later, and a running EventSourcingDB. To start one on your machine, see the [quickstart of EventSourcingDB](https://www.eventfoundation.io/docs/eventsourcingdb/quickstart).

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

*Note that calling `NewStore` with `nil` as the client, or as one of the options, panics.*

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

### Describing Events with Schemas

Every event type has a JSON schema, which the database checks every event of the type against, once the schema is registered (see [Registering Event Schemas](#registering-event-schemas)). The kit derives it from the struct, so that it describes exactly what `encoding/json` writes for the event:

- A struct is an object with the fields `encoding/json` writes: named by their `json` tags, without fields tagged `-` and unexported ones, and with the fields of embedded structs in place of the embedded struct. Fields with `omitempty` or `omitzero`, and fields of an embedded pointer, are optional, all others are required, and no other fields are allowed.
- A `string` is a string, a `bool` a boolean, an `int` or any other integer type an integer, and a `float64` or a `float32` a number. The `string` option of a `json` tag turns such a field into a string.
- A slice is an array, a `[]byte` a string, and an array an array of exactly its length. A map is an object whose values all have the same schema.
- A pointer, a slice and a map may also be `null`, since `encoding/json` writes `null` for `nil`, unless a field of such a type is optional and therefore left out instead.
- A `time.Time` is a string in the `date-time` format, a `json.Number` a number, unless the `string` option makes it a string, and a type with a `MarshalText` or an `AppendText` function a string. An interface allows any value, even if it declares a `Schema` function. So does a `json.RawMessage`, which is the same type as `jsontext.Value` of `encoding/json/v2`, since `encoding/json` writes the JSON it holds, or `null` if it is `nil`.

For `BookBorrowed`, this yields the following schema:

```json
{
  "type": "object",
  "properties": {
    "borrowedBy": { "type": "string" },
    "borrowedUntil": { "type": "string" }
  },
  "required": ["borrowedBy", "borrowedUntil"],
  "additionalProperties": false
}
```

These rules do not change, since a registered schema can not change either.

*Note that EventSourcingDB currently reads every number in the data of an event as a 64-bit floating-point number, which holds an integer exactly only if its magnitude is at most 2^53 - 1, which is 9007199254740991. A larger integer is stored changed, without an error: 9007199254740993 comes back as 9007199254740992, and a time in nanoseconds, as `UnixNano` returns it, loses its last digits. Close to the largest `int64` or `uint64`, the stored number may even lie beyond the range of the type, so that the event can not be read back any more, and `Load`, `Execute`, and a projection fail with an error of the category `ErrPermanent`. To keep a 64-bit integer exact, give its field the `string` option of the `json` tag, as in `json:"atNanos,string"`, which writes it as a string.*

To constrain a value further than its Go type does, declare a type for it with a `Schema` function, which returns the JSON schema of the type. Wherever a field has that type, the derived schema takes it over. For example, `borrowedUntil` accepts any string so far. To make sure that it is a date, `BookBorrowed` could use a `Date` type for the field:

```go
type Date string

func (Date) Schema() map[string]any {
  return map[string]any{"type": "string", "format": "date"}
}

type BookBorrowed struct {
  BorrowedBy    string `json:"borrowedBy"`
  BorrowedUntil Date   `json:"borrowedUntil"`
}
```

*Note that the other examples in this README keep `BorrowedUntil` a `string`. With the `Date` type, they would convert between the two, as in `Date(cmd.BorrowedUntil)`.*

Apart from `time.Time`, `json.Number` and `json.RawMessage`, a type that encodes itself with a `MarshalJSON` function, or with `MarshalJSONTo` of `encoding/json/v2`, which `encoding/json` calls as well, needs such a `Schema` function, too, since the kit can not know what the function writes. So does a type that has its `MarshalText` or `AppendText` function on a pointer receiver only: `encoding/json` calls it only for a value it can take the address of, so whether such a value is written as a string depends on where it is. If the schema of an event can not be derived, for example because of such a type, a recursive type, or a channel, `Evolve` or `Ignore` panics and names the field.

*Note that the derived schema never takes over the `Schema` function of an interface. `encoding/json` writes the value a field of the interface holds, or `null`, so there is no value whose `Schema` function could be asked, and the field is described as if the interface had none. To constrain such a field, or one whose interface encodes itself with `MarshalJSON`, give the event a `Schema` function of its own.*

*Note that a field of type `json.RawMessage` is constrained the same way, with a `Schema` function of the event. A type declared from `json.RawMessage`, as in `type Details json.RawMessage`, does not help, since it does not take over the `MarshalJSON` function: `encoding/json` writes such a value as a `[]byte`, which is a string.*

If an event needs a schema that its fields can not express, give the event itself a `Schema` function. It takes precedence over the derived schema. To start from the derived schema, call the `DeriveSchema` function, which derives the schema of a type without calling its own `Schema` function. For example, to require at least one of two optional fields:

```go
type BookCorrected struct {
  Title  string `json:"title,omitempty"`
  Author string `json:"author,omitempty"`
}

func (BookCorrected) EventType() string {
  return "io.eventsourcingdb.library.book-corrected"
}

func (BookCorrected) Schema() map[string]any {
  schema := architecturekit.DeriveSchema[BookCorrected]()
  schema["minProperties"] = 1

  return schema
}
```

`DeriveSchema` returns a new schema on every call, with objects as `map[string]any` and arrays as `[]any`, as `encoding/json` decodes them.

A `Schema` function describes the type that declares it. Go also promotes it to a struct that embeds the type, but there it still describes the embedded type alone, while `encoding/json` writes the fields of the embedded struct next to the other fields of the struct. So if an event, or the type of a field, has its `Schema` function only from an embedded field, `Evolve` or `Ignore` panics and names the embedded field, and so does `DeriveSchema` for such a type. Give the struct a `Schema` function of its own, or make the embedded type a named field.

*Note that the derived schema describes the fields of an embedded struct by their own types, as it describes all other fields, even if the embedded struct has a `Schema` function. A struct that starts its own `Schema` function from `DeriveSchema` therefore does not get the constraints from the `Schema` function of the embedded struct. Add them there if the struct needs them.*

*Note that a json tag name that `encoding/json` considers invalid also makes `Evolve` or `Ignore` panic, since `encoding/json` reads such a name differently depending on the Go version the application declares.*

#### Keeping Schemas in Files

An event's own schema does not have to be written in Go. To keep it in a JSON file instead, embed the file with the `embed` package, and decode it in the `Schema` function of the event:

```go
import (
  _ "embed"
  "encoding/json"
)

//go:embed schemas/book-acquired.json
var bookAcquiredSchema []byte

func (BookAcquired) Schema() map[string]any {
  var schema map[string]any
  if err := json.Unmarshal(bookAcquiredSchema, &schema); err != nil {
    panic(err)
  }

  return schema
}
```

The `Evolve` and `Ignore` functions call `Schema` when the state is built, so a file that does not contain valid JSON stops the application on start, before it writes anything.

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

*Note that calling `Evolve` twice for the same event type panics, and so does calling it with `nil` as the function, or with a pointer as the event type, such as `*BookBorrowed` rather than `BookBorrowed`, or with an interface, such as `architecturekit.Event`.*

Every read starts from a copy of the initial value, so an `Evolve` function may change the state it gets without changing what the next read starts from. A copy shares nothing with a value like `Book{}`, and neither with an initial value whose maps, slices, pointers and channels are `nil`, so leave them `nil`, and let the `Evolve` functions create them when they need them. If the initial value holds a map, a pointer or a channel that is not `nil`, or a slice with room for elements, every copy shares it, and the state needs a `Clone` function that copies it (see [Caching States](#caching-states)). Without one, reading the state fails with an error of the category `ErrPermanent` (see [Handling Errors](#handling-errors)), rather than let an `Evolve` function change the initial value of every later read.

Reading an event without a rule fails, since it usually points to a missing rule or a wrong subject. If a subject holds events that matter for no decision, such as `BookInspected`, which only records that somebody looked at a book, call the `Ignore` function for their type. The state then takes them without changing, and says so, rather than an `Evolve` function that returns the state unchanged and needs a comment to explain why:

```go
var bookState = architecturekit.NewState(Book{}).
  Evolve(func(book Book, event BookAcquired) Book {
    book.IsAcquired = true
    return book
  }).
  // ...
  Ignore[BookInspected]()
```

*Note that the data of an ignored event is not decoded, but its schema is still part of `Schemas`, since the event is still written. Ignoring an event type that has an `Evolve` rule, or ignoring it twice, panics, and so does calling `FromLatest` for it. So does ignoring a pointer or an interface as the event type, as with `Evolve`.*

*Note that `Execute` refuses to write an event that the state of the decider has no rule for, since the state could not read the subject any more afterwards (see [Executing Commands](#executing-commands)). To find out whether a state has a rule for an event type, call the `HasRule` function on the state with the event type.*

### Making Decisions

A decider connects a state with the decision made on it. To create one, call the `NewDecider` function with the state and a function that receives the command and the current state, and returns the events to write:

```go
var ErrBookAlreadyAcquired = architecturekit.NewDomainError("book has already been acquired")

var acquireBook = architecturekit.NewDecider(bookState,
  func(ctx context.Context, cmd AcquireBook, book Book) ([]architecturekit.Event, error) {
    if book.IsAcquired {
      return nil, fmt.Errorf("%w: %s", ErrBookAlreadyAcquired, cmd.BookID)
    }

    return []architecturekit.Event{
      BookAcquired{
        Title:  cmd.Title,
        Author: cmd.Author,
        ISBN:   cmd.ISBN,
      },
    }, nil
  })
```

The types of the command and the state are taken from the function, so they do not have to be repeated. `NewDecider` returns a `Decider`. Its `State` function returns the state, and its `Decide` function decides on a command, given the state, with the function you handed over.

*Note that calling `NewDecider` with `nil` as the state or as the function panics. A `Decider` that was declared but never created with `NewDecider` has neither, so calling its `Decide` function, or handing it to `Execute`, to the test fixture, or to `Route` panics as well, and `Handle` fails with an error (see [Handling Commands over HTTP](#handling-commands-over-http)). Either way, the text names the mistake.*

To reject a command, return an error created with the `NewDomainError` function. It takes a format string and arguments, like `fmt.Errorf`, and returns an error of the type `*DomainError`, whose message is exactly the formatted text, and which belongs to the category `ErrDomain` (see [Handling Errors](#handling-errors)).

To let a caller or a test tell a rejection apart from the others, create it once, as a variable such as `ErrBookAlreadyAcquired`, and return it, or wrap it with `fmt.Errorf` and `%w` to add details, such as the ID of the book. `errors.Is` then finds the variable in the error, as well as the category `ErrDomain` (see [Expecting Rejections](#expecting-rejections)).

A decider may check several rules:

```go
var borrowBook = architecturekit.NewDecider(bookState,
  func(ctx context.Context, cmd BorrowBook, book Book) ([]architecturekit.Event, error) {
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
  })
```

If there is nothing to do, return neither events nor an error:

```go
var returnBook = architecturekit.NewDecider(bookState,
  func(ctx context.Context, cmd ReturnBook, book Book) ([]architecturekit.Event, error) {
    if !book.IsBorrowed {
      return nil, nil
    }

    return []architecturekit.Event{
      BookReturned{},
    }, nil
  })
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

If one of the events the decider returns is `nil`, `Execute` writes none of them, and fails with an error of the category `ErrPermanent` that names the index of the event, since an event that is `nil` has neither a type nor data. A `nil` pointer counts as `nil`, too, such as a `*BookBorrowed` that was declared but never set. The test fixture reports such an event the same way (see [Testing Deciders](#testing-deciders)).

Every event the decider returns needs a rule on the state of the decider, an `Evolve` function or `Ignore`. `Execute` writes the events to the subject that the same state reads for the next command, and the database keeps every event, so an event without a rule would leave a subject that the state can not read any more. If the state has no rule for one of the events, `Execute` writes none of them, and fails with an error of the category `ErrPermanent` that names the event type (see [Handling Errors](#handling-errors)). The test fixture reports such an event the same way (see [Testing Deciders](#testing-deciders)).

Nor does `Execute` write any of the events if the data of one of them can not be encoded as JSON, for example because it holds a float `NaN`. It then fails with an error of the category `ErrPermanent` that names the event type and the reason, since trying again would fail the same way. `Execute` checks all events for `nil` first, then all of them for a rule, and encodes them last. The test fixture checks in the same order, and reports such an event the same way (see [Testing Deciders](#testing-deciders)).

The written events come as they are stored, with their data as JSON. To read the data of one of them, call the `Decode` function with the type of the event. It returns an `Envelope`, the same a projection gets (see [Defining Projections](#defining-projections)), with the data in its `Data` field:

```go
var titles []string

for _, event := range writtenEvents {
  switch event.Type {
  case (BookAcquired{}).EventType():
    acquired, err := architecturekit.Decode[BookAcquired](event)
    if err != nil {
      // ...
    }

    titles = append(titles, acquired.Data.Title)
  }
}
```

*Note that `Decode` fails with an error of the category `ErrPermanent` for an event of another type, rather than leaving the fields of the wrong struct empty, and for data that does not fit the type. A pointer as the type, such as `*BookAcquired`, or an interface, such as `architecturekit.Event`, makes it panic, as with `Evolve`.*

*Note that `Execute` only reads the events of the command's subject itself, not those of nested subjects.*

### Using Preconditions

Every command declares at least one precondition, so that writing without any check is always a decision, never an oversight. There are these kinds:

- `OnStateRead` guards the state the command is decided on.
- `OnPristineSubject`, `OnPopulatedSubject`, and `OnEventID` require the subject to have no events yet, to have events, or to be on a given event, for example the revision the caller hands over.
- `Require` turns any other precondition of the client SDK into one of the command, such as an EventQL query.
- `Unconditionally` writes without any check.

Preconditions can be combined, and all of them must hold. If a precondition does not hold, nothing is written, and `Execute` returns an error of the category `ErrConflict` (see [Handling Errors](#handling-errors)). If a command declares no preconditions, or combines `Unconditionally` with others, `Execute` returns an error of the category `ErrPermanent` before reading anything. To check the preconditions of a command this way without executing it, call the `CheckPreconditions` function with the command, which returns the same error, or `nil`. The `CheckReadOptions` function does the same for the options of a read (see [Reading Events](#reading-events)).

To find out what kind a precondition is, call its `IsOnStateRead` or `IsUnconditional` function. Its `Database` function returns the precondition of the client SDK that the database checks for it: the one `Require` made it from, which is how `OnPristineSubject` and `OnPopulatedSubject` make theirs, or the one of `NewIsSubjectOnEventIDPrecondition` for `OnEventID`. For any other, it returns `false`.

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

If something else was written in between, the command fails with an error of the category `ErrConflict`. Since the decider decided on the state that `Execute` read, deciding again on the new state is what would have happened had the command arrived a moment later. To have `Execute` do so, hand over the `WithConflictRetries` option when creating the store, with the number of times to decide again:

```go
store := architecturekit.NewStore(client, "https://library.eventsourcingdb.io", architecturekit.WithConflictRetries(4))
```

`Execute` then reads the state anew, and the decider decides on it, up to four more times. Once the retries are used up, the command fails with `ErrConflict`, as without the option.

*Note that a negative number of retries makes `WithConflictRetries` panic.*

*Note that this only applies to commands whose preconditions include `OnStateRead`. A command that checks only a revision the caller hands over is never decided again, since the caller has to learn about the conflict, and deciding again would fail the same way (see [Checking the Revision of the Caller](#checking-the-revision-of-the-caller)). A command that checks both is decided again, since the database does not say which precondition did not hold. If the revision of the caller is outdated, every attempt fails the same way, until the retries are used up. On the same subject, one of the two is enough: if the revision of the caller holds, so does `OnStateRead`.*

#### Checking the Revision of the Caller

If a command may only write events in case its subject has not changed since the caller last read it, for example in a user interface, use the `OnEventID` function with the subject and the ID of the last event the caller has seen. For that, add a field for the ID:

```go
type BorrowBook struct {
  BookID          string
  ReaderID        string
  BorrowedUntil   string
  ExpectedEventID string
}

func (c BorrowBook) Preconditions() []architecturekit.Precondition {
  return []architecturekit.Precondition{
    architecturekit.OnEventID(c.Subject(), c.ExpectedEventID),
  }
}
```

*Note that the caller has to provide the event ID. A view can keep it for that purpose, as long as its items follow every event of their subject (see [Defining Views](#defining-views)).*

*Note that the event ID usually comes from the caller, so `Execute` checks it before it reads anything. An empty event ID, or one that is not an event ID at all, makes `Execute` fail with an error that wraps `ErrNotARevision` and names the ID, as in `not a revision: "abc"`, as `Read` does for a bound (see [Reading Events](#reading-events)), and the `httpapi` package answers it with `400 Bad Request` and the error as the message (see [Mapping Errors to Status Codes](#mapping-errors-to-status-codes)). It does so only once nothing else is wrong with the preconditions, so that a mistake in the code is not hidden behind one of the caller, and `Write`, `CheckPreconditions`, and the test fixture of `architecturekittest` refuse the ID the same way. `Require` with `NewIsSubjectOnEventIDPrecondition` of the client SDK leaves the ID to the database instead, which refuses such an ID as a malformed request, so that `Execute` fails with an error of the category `ErrPermanent`.*

#### Preventing Duplicates

If a command may only write events in case its subject does not yet have any events, use the `OnPristineSubject` function:

```go
func (c AcquireBook) Preconditions() []architecturekit.Precondition {
  return []architecturekit.Precondition{
    architecturekit.OnPristineSubject(c.Subject()),
  }
}
```

#### Requiring an Existing Subject

If a command may only write events in case its subject already has at least one event, use the `OnPopulatedSubject` function. Combine it with `OnStateRead` to also guard the state:

```go
func (c ReturnBook) Preconditions() []architecturekit.Precondition {
  return []architecturekit.Precondition{
    architecturekit.OnPopulatedSubject(c.Subject()),
    architecturekit.OnStateRead(),
  }
}
```

#### Enforcing Rules Across Subjects

If a command may only write events depending on an EventQL query, use the `NewIsEventQLQueryTruePrecondition` function of the client SDK, and wrap it with the `Require` function. For example, to acquire every ISBN only once, extend the preconditions of `AcquireBook`:

```go
func (c AcquireBook) Preconditions() []architecturekit.Precondition {
  return []architecturekit.Precondition{
    architecturekit.OnPristineSubject(c.Subject()),
    architecturekit.Require(eventsourcingdb.NewIsEventQLQueryTruePrecondition(fmt.Sprintf(
      "FROM e IN events WHERE e.type == 'io.eventsourcingdb.library.book-acquired' AND e.data.isbn == '%s' PROJECT INTO COUNT() == 0",
      c.ISBN,
    ))),
  }
}
```

*Note that the query must return a single row with a single value, which is interpreted as a boolean.*

The ISBN comes from outside, but becomes part of the query as text, so check it first. `Execute` sends the query to the database only together with the events, once the decider has decided, so a check in the decider keeps every value that is not an ISBN out of the query. Here, an ISBN consists of digits and hyphens, and may end with an `X`:

```go
var isbnPattern = regexp.MustCompile(`^[0-9][0-9-]*[0-9X]$`)

var acquireBook = architecturekit.NewDecider(bookState,
  func(ctx context.Context, cmd AcquireBook, book Book) ([]architecturekit.Event, error) {
    if !isbnPattern.MatchString(cmd.ISBN) {
      return nil, architecturekit.NewDomainError("%q is not an ISBN", cmd.ISBN)
    }

    // ...
  })
```

*Note that a value must never go into an EventQL query unchecked. A quote, as in `it's`, breaks the query, so that the write fails, and a value made up for that purpose changes what the query checks.*

When the ISBN is taken, the database refuses the write with `precondition failed`, the same answer as for a subject that has changed since it was read. It does not say which precondition failed, so the kit can not tell the two apart:

- The command fails with an error of the category `ErrConflict`, which is transient (see [Handling Errors](#handling-errors)).
- The `httpapi` package answers it with `409 Conflict` and the message `conflict: the data has changed since it was read`, which tells the caller to try again, although that never helps (see [Handling Commands over HTTP](#handling-commands-over-http)).
- If the command declares `OnStateRead` as well, and the store decides again on conflicts, `Execute` decides again and again in vain, until the retries are used up (see [Guarding Against Concurrent Changes](#guarding-against-concurrent-changes)).

To answer a taken ISBN with a domain error instead, check it in the decider, against a view that holds the acquired books by their ISBN (see [Defining Views](#defining-views)). The catalog does not hold the ISBN, so here, a small view of its own does, which a projection fills (see [Defining Projections](#defining-projections)):

```go
type ISBNItem struct {
  ISBN   string
  BookID string
}

func newISBNs() *architecturekit.InMemoryView[string, ISBNItem] {
  return architecturekit.NewInMemoryView(func(item ISBNItem) string { return item.ISBN })
}

func newISBNProjection(isbns *architecturekit.InMemoryView[string, ISBNItem]) *architecturekit.TypedProjection {
  return architecturekit.NewTypedProjection().
    On(func(ctx context.Context, event architecturekit.Envelope[BookAcquired]) error {
      _, err := isbns.Insert(ctx, event.ID, ISBNItem{
        ISBN:   event.Data.ISBN,
        BookID: bookIDOf(event.Subject),
      })
      return err
    })
}
```

The decider gets the view from a function that creates it, as it gets a clock (see [Using the Current Time](#using-the-current-time)). If the ISBN is taken, it returns a domain error, which the `httpapi` package answers with `422 Unprocessable Entity` and the error as the message, as in `ISBN has already been acquired: 978-0756906788`:

```go
var ErrISBNAlreadyAcquired = architecturekit.NewDomainError("ISBN has already been acquired")

func acquireBookDecider(isbns architecturekit.KeyedView[string, ISBNItem]) architecturekit.Decider[AcquireBook, Book] {
  return architecturekit.NewDecider(bookState,
    func(ctx context.Context, cmd AcquireBook, book Book) ([]architecturekit.Event, error) {
      if !isbnPattern.MatchString(cmd.ISBN) {
        return nil, architecturekit.NewDomainError("%q is not an ISBN", cmd.ISBN)
      }

      _, isTaken, err := isbns.Get(ctx, cmd.ISBN)
      if err != nil {
        return nil, err
      }
      if isTaken {
        return nil, fmt.Errorf("%w: %s", ErrISBNAlreadyAcquired, cmd.ISBN)
      }

      // ...
    })
}

isbns := newISBNs()
acquireBook := acquireBookDecider(isbns)
```

Keep the EventQL precondition all the same, as the guard. A view lags behind the events, so when two acquisitions of the same ISBN arrive at the same moment, both pass the view, since it has seen neither of them. The database writes only one of them, and the other fails with `ErrConflict`, which is the right answer in that rare case: once the view has seen the first acquisition, trying again gets the domain error.

#### Writing Unconditionally

If a command may write its events whatever has been written to its subject in the meantime, for example `CommentOnBook`, which only records a comment that does not depend on the state, use the `Unconditionally` function:

```go
func (c CommentOnBook) Preconditions() []architecturekit.Precondition {
  return []architecturekit.Precondition{
    architecturekit.Unconditionally(),
  }
}
```

*Note that `Unconditionally` can not be combined with other preconditions.*

### Sharing Code Between Commands

The commands of one aggregate usually act on the same kind of subject, guarded by the same preconditions, and their deciders check the same things first. Write these once instead of for every command.

#### Sharing Subjects and Preconditions

Define `Subject` and `Preconditions` on a struct of their own, and embed it in every command. Go promotes the functions and fields of an embedded struct, so every command that embeds it is a `Command`:

```go
type BookTarget struct {
  BookID string
}

func (t BookTarget) Subject() string {
  return "/books/" + t.BookID
}

func (t BookTarget) Preconditions() []architecturekit.Precondition {
  return []architecturekit.Precondition{
    architecturekit.OnStateRead(),
  }
}

type AcquireBook struct {
  BookTarget
  Title  string
  Author string
  ISBN   string
}

type BorrowBook struct {
  BookTarget
  ReaderID      string
  BorrowedUntil string
}

type ReturnBook struct {
  BookTarget
}
```

A command then names its book through the embedded struct, as in `BorrowBook{BookTarget: BookTarget{BookID: "42"}, ReaderID: "23"}`, and a decider reads it as `cmd.BookID`.

The command that creates a book can embed the struct, too, since `OnStateRead` requires a subject without any events to still be pristine (see [Guarding Against Concurrent Changes](#guarding-against-concurrent-changes)). That is different if the struct checks the revision of the caller instead (see [Checking the Revision of the Caller](#checking-the-revision-of-the-caller)): a new book has no revision yet. Then the creating command does not embed the struct, and prevents duplicates itself:

```go
type BookTarget struct {
  BookID          string
  ExpectedEventID string
}

func (t BookTarget) Subject() string {
  return "/books/" + t.BookID
}

func (t BookTarget) Preconditions() []architecturekit.Precondition {
  return []architecturekit.Precondition{
    architecturekit.OnEventID(t.Subject(), t.ExpectedEventID),
  }
}

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
    architecturekit.OnPristineSubject(c.Subject()),
  }
}
```

#### Sharing Checks

Most deciders of an aggregate first check that it exists. Write that check once, as a function that returns a domain error, and call it at the beginning of every decider that needs it:

```go
func requireAcquired(book Book, bookID string) error {
  if !book.IsAcquired {
    return architecturekit.NewDomainError("book %s does not exist", bookID)
  }

  return nil
}

var borrowBook = architecturekit.NewDecider(bookState,
  func(ctx context.Context, cmd BorrowBook, book Book) ([]architecturekit.Event, error) {
    if err := requireAcquired(book, cmd.BookID); err != nil {
      return nil, err
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
  })
```

*Note that only the state knows what it means for an aggregate to exist, which is why the kit does not check it. The precondition that requires an existing subject protects the write (see [Requiring an Existing Subject](#requiring-an-existing-subject)), but it fails with an error of the category `ErrConflict`, which is transient. A check in the decider answers a command on a book that does not exist with a domain error instead.*

### Using the Current Time

Some rules depend on the current time. For example, a reader may borrow a book for at most four weeks, so the decider has to know what day it is. Rather than calling `time.Now` in the decider, hand a clock to a function that creates the decider:

```go
type Clock func() time.Time

func borrowBookDecider(now Clock) architecturekit.Decider[BorrowBook, Book] {
  return architecturekit.NewDecider(bookState,
    func(ctx context.Context, cmd BorrowBook, book Book) ([]architecturekit.Event, error) {
      // ...

      today := now()
      if cmd.BorrowedUntil < today.Format(time.DateOnly) ||
        cmd.BorrowedUntil > today.AddDate(0, 0, 28).Format(time.DateOnly) {
        return nil, architecturekit.NewDomainError("book %s can be borrowed for at most four weeks", cmd.BookID)
      }

      return []architecturekit.Event{
        BookBorrowed{
          BorrowedBy:    cmd.ReaderID,
          BorrowedUntil: cmd.BorrowedUntil,
        },
      }, nil
    })
}
```

The application hands over the real clock, and a test a fixed one, so that it does not have to compute around the actual date:

```go
borrowBook := borrowBookDecider(time.Now)

borrowBookOnFirstOfOctober := borrowBookDecider(func() time.Time {
  return time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
})
```

The parameter shows which deciders depend on the time, and the clock can return what the domain thinks in, such as a date in a particular time zone instead of a moment.

#### Keeping the Time in Events

Only the decider reads the clock. Whatever it decides based on the time, such as the day a book was borrowed until, it writes into the event. Everything that comes after relies on the event, not on the clock:

- `Evolve` never reads the clock. It runs again every time the state is loaded, so a state that depends on the clock would be different tomorrow from what it is today, for the very same events.
- A projection never reads the clock either. It runs again whenever the read model is rebuilt, which would turn yesterday's events into today's answers.

To know when an event was written, use the time that the database records for every event, which a projection receives in the `Time` field of the envelope. A question that depends on the moment it is asked, such as which books are overdue, belongs into the query, which gets the current time as a parameter (see [Depending on More Than the Read Model](#depending-on-more-than-the-read-model)).

### Writing to Several Subjects

A command acts on a single subject, since it is a decision on the state of that subject. Some events are no such decision, for example a summary derived from many subjects, which has to be written together with a note on each of them, either all or none, such as an `InventoryTaken` event that counts the books, with a `BookCounted` event for each of them. To write events to several subjects at once, call the `Write` function with a context, the store, the events together with their subjects, and the preconditions:

```go
written, err := architecturekit.Write(context.TODO(), store, []architecturekit.EventOn{
  {Subject: "/inventories/2026", Event: InventoryTaken{BookCount: 2}},
  {Subject: "/books/42", Event: BookCounted{Inventory: "2026"}},
  {Subject: "/books/23", Event: BookCounted{Inventory: "2026"}},
}, architecturekit.OnPristineSubject("/inventories/2026"))
if errors.Is(err, architecturekit.ErrConflict) {
  // Somebody else has taken the inventory already.
}
```

`Write` writes the events with the source of the store, and returns them as the database recorded them. If a precondition does not hold, nothing is written, and the error belongs to the category `ErrConflict`. If the data of one of the events can not be encoded as JSON, nothing is written either, and the error belongs to the category `ErrPermanent`. So does an event without a subject, or one that is `nil`, a `nil` pointer included. Other failures belong to the same categories as for `Execute` (see [Handling Errors](#handling-errors)).

Like a command, a write declares at least one precondition, such as `OnPristineSubject` or one made with `Require`, or `Unconditionally` to write without any. `OnStateRead` has nothing to guard, since `Write` reads no state, so `Write` refuses it with an error of the category `ErrPermanent`, without writing anything. An event ID of `OnEventID` that is not a revision makes `Write` fail with an error that wraps `ErrNotARevision`, as with `Execute`, without writing anything either (see [Checking the Revision of the Caller](#checking-the-revision-of-the-caller)).

*Note that `Write` never decides again, since there is nothing to decide. If nothing is to be written, call it with no events, which writes nothing. It still needs preconditions, though, as any other write, and fails with an error of the category `ErrPermanent` without them, so declare them, for example with `Unconditionally()`.*

*Note that `Write` knows no state, so unlike `Execute`, it does not refuse an event that a state has no rule for. Every state that reads one of the subjects needs a rule for the events written to it, since reading the subject fails otherwise.*

### Handling Errors

Apart from the end of the context, `ErrNotARevision`, and `ErrEmptyRange`, which belong to no category (see below), every failure of architecturekit itself in reading and writing belongs to one of four categories. Use `errors.Is` to check for a category rather than for a concrete error:

- `ErrDomain` means that a business rule rejected the command, as with `NewDomainError`.
- `ErrConflict` means that a precondition did not hold.
- `ErrTransient` means that trying again may help, for example if the database can not be reached.
- `ErrPermanent` means that trying again will not help, for example if an event could not be decoded, if the data of an event can not be encoded as JSON, such as a float `NaN`, if an event does not match the schema of its type, if a subject contains an event type the state has no rule for, if a decider returns one, or if it returns an event that is `nil`.

A failure of the database is sorted by what its answer means, the same way for reading and for writing, apart from `409`:

- If the database can not be reached, if the connection breaks, if the database asks to slow down (`429`), or if it is unable to answer for now (`5xx`), for example because it is shutting down, the error belongs to `ErrTransient`.
- If the answer does not come from an EventSourcingDB, the error belongs to `ErrTransient` as well, since a proxy in front of the database answers on its own while the database restarts. The message says so, so that a wrong address stands out in the log.
- If the database rejects the API token (`401`), or if it rejects the request itself, for example because it is malformed (`400`) or too large (`413`), the error belongs to `ErrPermanent`. For a rejected API token, the message says so.
- If a precondition did not hold when writing (`409`), the error belongs to `ErrConflict`. If an event does not match its schema, which the database answers with the same status, it belongs to `ErrPermanent`.
- Reading has no preconditions, so when reading, a `409` is a refusal of the request like any other, and the error belongs to `ErrPermanent`. The database answers a read with `409` if the latest event of the type given to `FromLatestEvent` comes after the upper bound, and trying again never helps, since new events only move the latest one further away (see [Reading Events](#reading-events)).

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

A failure of the database wraps the error of the client after its category. So `errors.As` finds the `*eventsourcingdb.DBAPIError` with the status code and the reason the database gave, for example to log them, and `errors.Is` finds `eventsourcingdb.ErrInvalidServerHeader` and `eventsourcingdb.ErrHeartbeatTimeout`. It is the category, though, that tells what the failure means, so check for it first.

An error that your own code returns, for example from a decider or a projection, passes through unchanged, so it belongs to a category only if you wrap it with one, as `NewDomainError` does. A panic in a projection, on the other hand, comes back as an error of the category `ErrPermanent` (see [Running Projections](#running-projections)). The `httpapi` and `query` packages have errors of their own, which `StatusFor` maps to status codes (see [Mapping Errors to Status Codes](#mapping-errors-to-status-codes) and [Getting a Single Item](#getting-a-single-item)).

If the context ends, reading and writing stop, and the error is the one of the context, `context.Canceled` or `context.DeadlineExceeded`, which belongs to no category. Check for it with `errors.Is` as well. This is never a partial success: a read that the context cut short fails rather than handing out part of a state, and `Execute` writes nothing once the context has ended, also if it ends while the decider decides.

Another exception is `ErrNotARevision`, which belongs to no category either. It means that a value that was handed over is not a revision, such as a bound of `Read`, the event ID of `OnEventID`, the revision for `WaitFor`, or a value for `CompareRevisions` or `ParseRevision` (see [Comparing Revisions](#comparing-revisions)). The error wraps `ErrNotARevision` and names the value, so check for it with `errors.Is` as well. Such a value usually comes from a request, so `StatusFor` maps it to `400 Bad Request` (see [Mapping Errors to Status Codes](#mapping-errors-to-status-codes)). Only a function of a view, which refuses the ID of an event it is to apply, wraps it together with `ErrPermanent` (see [Defining Views](#defining-views)).

The last exception is `ErrEmptyRange`, which belongs to no category either. It means that the bounds of `Read` leave no room for any event, such as `BeforeEvent("0")` (see [Reading Events](#reading-events)). The error wraps `ErrEmptyRange` and names the values, so check for it with `errors.Is` as well. Such bounds usually come from a request, so `StatusFor` maps it to `400 Bad Request` as well.

*Note that only the end of the context matches `context.Canceled` or `context.DeadlineExceeded`. If an error of the client matches one of them all the same, for example because connecting to the database timed out, which the standard library reports as `context.DeadlineExceeded`, the failure of the database keeps that error in its message only, so that it does not look like the end of the context.*

*Note that `ErrUnverified` is a special case of `ErrPermanent`, which means that an event failed its verification (see [Verifying Events](#verifying-events)). Since that may point to a security incident rather than a mistake, check for it before `ErrPermanent` if you want to treat it differently, for example to raise an alarm.*

*Note that `Execute` does not try again by itself, unless the store decides again on conflicts (see [Guarding Against Concurrent Changes](#guarding-against-concurrent-changes)). To try again, for example after a transient failure, call `Execute` again.*

### Registering Event Schemas

The database only checks events against a schema once it is registered. The `Evolve` and `Ignore` functions collect the schemas of all events of a state, derived or their own (see [Describing Events with Schemas](#describing-events-with-schemas)). To get them as a slice of `EventSchema`, each with the fields `EventType` and `Schema`, call the `Schemas` function on the state. Then hand them over to the `RegisterSchemas` function, together with a context and the store:

```go
err := architecturekit.RegisterSchemas(context.TODO(), store, bookState.Schemas())
if err != nil {
  // ...
}
```

`RegisterSchemas` accepts the schemas of several states at once. Call it on every start, before the application serves requests: for an event type the database knows already, it checks that the registered schema is exactly the one from the code. If the context ends first, it returns the error of the context. Since the client registers a schema without a context, a registration that has begun is finished, but none begins once the context has ended.

An event that no state has a rule for, such as `InventoryTaken`, which only `Write` writes (see [Writing to Several Subjects](#writing-to-several-subjects)), is part of no `Schemas`. To register its schema as well, call the `SchemaOf` function with the type of the event. It returns an `EventSchema` by the rule that `Evolve` applies, the event's own schema or the derived one. Since `RegisterSchemas` takes slices of them, hand it over in a slice of its own:

```go
err := architecturekit.RegisterSchemas(context.TODO(), store,
  bookState.Schemas(),
  []architecturekit.EventSchema{architecturekit.SchemaOf[InventoryTaken]()},
)
if err != nil {
  // ...
}
```

*Note that `SchemaOf` panics where `Evolve` would, for example for an event that has its `Schema` function only from an embedded field, or for a pointer or an interface as the event type.*

A registered schema can not change. If it differs from the one from the code, `RegisterSchemas` returns an error of the category `ErrPermanent`, and so it does if the database refuses a schema, for example because stored events of the type do not match it. To change the shape of an event, introduce a new event type instead (see [Versioning Events](#versioning-events)).

*Note that this also holds if you remove the `Schema` function of an event whose schema is registered already: the derived schema has to be exactly the registered one, or `RegisterSchemas` fails. Keep the `Schema` function of such an event, unless you have compared both.*

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

The function has the type `Upcaster`. Upcasters may return more than one event, for example to split an event that recorded two facts into one event per fact. Derive each of them from the stored event, as above, so that they all keep its ID. A projection then applies every one of them to a view, also if several of them change the same item (see [Defining Views](#defining-views)). If a returned event has an upcaster of its own, that one runs as well, so every version needs only a single step to the next one. The translated events are never written back.

A chain ends after 16 steps: if the event still has an upcaster then, reading fails with an error of the category `ErrPermanent`, which catches an upcaster that keeps its event type and would otherwise run forever. An error that an upcaster returns belongs to `ErrPermanent` as well.

To use the upcasters, call the `UpcastWith` function on the state and hand over the set. The upcasters then run before the `Evolve` rules:

```go
bookState.UpcastWith(libraryUpcasters)
```

Upcasting belongs to the event types, not to a single state, so register the upcasters once and hand the same set to every state, and to every projection that reads these events (see [Defining Projections](#defining-projections)). That way, the write side and the read side see the same events.

The kit decodes the data of an event with `encoding/json`, which matches the names in the data to the fields of the struct regardless of case, and ignores names that match no field. So data that names a field `BorrowedBy` fills the field that `BookBorrowed` tags as `borrowedBy`, without an upcaster. A name that differs by more than case, such as `lentTo`, matches no field, and the field stays empty, so a field whose name changes by more than case still needs an upcaster, as above.

*Note that calling `Upcast` with `nil` as the function, or twice for the same event type, panics, as does calling `UpcastWith` twice, or with `nil`.*

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

*Note that calling `FromLatest` for an event type without an `Evolve` rule, or for a pointer or an interface as the event type, or calling it twice, panics.*

### Caching States

If no event carries the whole state, the store can keep the states of the most recently used subjects in memory instead, so that the next command on one of them reads only the events written since. To do so, hand over the `WithStateCache` option with the number of subjects to keep when creating the store:

```go
store := architecturekit.NewStore(client, "https://library.eventsourcingdb.io", architecturekit.WithStateCache(10_000))
```

Once the cache is full, the least recently used subject makes room. Such a subject is read in full again the next time, or from the latest event of the type given to `FromLatest`, if there is one (see [Reading Long Streams](#reading-long-streams)).

The cache only holds what was read, never what a command has written. Every command reads all events after the ones its state was built from, including those written by other processes, so the cache stays correct if several processes write to the same subjects.

The cache tells states apart by their type, not by the object. A state that is built anew for every command, for example by a function that returns it, is cached as well. This is why two different states that read the same subject need two different types: if one of them counts the loans of a book and the other one its returns, declare types such as `LoanCount` and `ReturnCount` rather than using `int` for both. If two states of the same type meet on the same subject, but differ in their initial value, in the event types they have `Evolve` rules for, ignore, or have upcasters for, or in `FromLatest`, `Execute` and `Load` return an error of the category `ErrPermanent` (see [Handling Errors](#handling-errors)).

*Note that the cache can not compare the `Evolve` functions themselves. Two states of the same type that are built alike, but compute something else, are not told apart.*

*Note that the very same state is not compared at all. The initial values of two different states of the same type are compared as `reflect.DeepEqual` compares them, except that every value equals itself: two functions in them are equal if both are `nil` or both are not, since functions can not be compared, and two floating-point numbers if they are equal or both `NaN`, as are the parts of two complex numbers. Only a map with a `NaN` key does not equal itself, since Go never finds such a key again.*

A cached state is handed to several commands, possibly at the same time. That is safe for a state that consists of values only, such as the `Book` state above. A state that holds slices, maps, pointers, channels, functions, or interfaces, such as a field of type `error` or `any`, is only cached if it has a `Clone` function, which returns a copy that shares no data with the original. An interface counts as well, since it can hold any of the others. For example, a shelf that collects the IDs of the books that `BookShelved` events put on it needs one:

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

Without a `Clone` function, such a state is read as without a cache. The same function lets `Step` and `StepStored` leave a state unchanged (see [Stepping Through States](#stepping-through-states)), and copies the initial value at the start of every read, which an initial value with a map, a pointer or a channel that is not `nil`, or a slice with room for elements, requires (see [Defining State](#defining-state)).

*Note that as an initial value, `Shelf{}`, whose `BookIDs` are `nil`, does not require a `Clone` function, and neither does `Shelf{BookIDs: []string{}}`, since appending to a slice without room for elements allocates a new array. `Shelf{BookIDs: make([]string, 0, 10)}` does, since appending to it writes into the array that every copy shares.*

*Note that a `time.Time` counts as a value, since its location never changes.*

*Note that a number of `0` keeps no states, the same as leaving out the option, so that a configuration can turn the cache off. A negative number makes `WithStateCache` panic, and calling `Clone` twice, or with `nil`, panics as well.*

### Loading States

To read the state of a single subject without executing a command, for example to answer a query about it, call the `Load` function with a context, the store, the state, and the subject:

```go
book, err := architecturekit.Load(context.TODO(), store, bookState, "/books/42")
if err != nil {
  // ...
}
```

`Load` reads the events exactly the way `Execute` does before it decides, including `FromLatest` and the state cache. For a query across many subjects, use a view instead (see [Defining Views](#defining-views)).

### Reading Events

Some reads need the events themselves rather than a state, and fit neither `Load` nor a projection, for example one page of a long history, the events up to a certain one, or a single event. To read the events as they are stored, call the `Read` function with a context, the store, and the subjects to read. Say which subjects with the `SubjectTree` or the `ExactSubject` function, as for a projection (see [Running Projections](#running-projections)). It returns an iterator over the events and errors:

```go
for event, err := range architecturekit.Read(context.TODO(), store, architecturekit.ExactSubject("/books/42")) {
  if err != nil {
    // ...
  }

  // ...
}
```

Without options, `Read` hands out every event of the subjects, oldest first. To read fewer of them, or in the other order, hand over options:

- `FromEvent` reads from the event with the given ID on, including it, and `AfterEvent` reads the events after it.
- `UpToEvent` reads up to the event with the given ID, including it, and `BeforeEvent` reads the events before it.
- `NewestFirst` hands out the newest event first. The bounds stay what they are.
- `FromLatestEvent` starts at the latest event of a type on a subject, including it, as `FromLatest` does for a state (see [Reading Long Streams](#reading-long-streams)). If the subject has no such event, `ReadEverything` reads all events, and `ReadNothing` reads none.

For example, to read the next page of the history of a book, hand over the ID of the last event of the page before:

```go
events := architecturekit.Read(context.TODO(), store, architecturekit.ExactSubject("/books/42"),
  architecturekit.AfterEvent(lastEventID),
)
```

To read the history of a book from its latest audit on, up to a certain event:

```go
events := architecturekit.Read(context.TODO(), store, architecturekit.ExactSubject("/books/42"),
  architecturekit.FromLatestEvent("/books/42", BookAudited{}.EventType(), architecturekit.ReadEverything),
  architecturekit.UpToEvent(eventID),
)
```

The IDs are strings, as everywhere else in the kit. The database hands them out as one ascending sequence across all subjects, so a bound does not have to be an event of the subjects that are read. An ID that is not the one of an event, such as `abc` or an empty one, ends the iteration with an error that wraps `ErrNotARevision` and names the ID, as in `not a revision: "abc"`, before the database is asked. That way, an ID that comes from a request can be told apart from a failure of the database, and the `httpapi` package answers it with `400 Bad Request` and the error as the message (see [Mapping Errors to Status Codes](#mapping-errors-to-status-codes)).

*Note that a read has at most one lower bound, one upper bound, and one order, and that `FromLatestEvent` counts as a lower bound. Options that contradict each other, such as `FromEvent` together with `AfterEvent`, or `NewestFirst` given twice, make `Read` panic. So does `FromLatestEvent` together with `NewestFirst`, since the database reads from the latest event of a type only oldest first, and so does the zero value of `Subjects`, or a `nil` option. A subject for `FromLatestEvent` that does not start with a slash makes `FromLatestEvent` itself panic, and so does a value other than `ReadEverything` and `ReadNothing`.*

*Note that `FromLatestEvent` looks for the event on the given subject alone, not below it, and that this subject does not have to be one of those that are read.*

Bounds that leave no room for any event are a mistake, since no event can ever lie between them, such as `BeforeEvent("0")`, `AfterEvent` and `BeforeEvent` with two neighboring IDs, a lower bound above the upper one, or `AfterEvent` of the largest revision, `9223372036854775807`, after which no event can ever come. They end the iteration with an error that wraps `ErrEmptyRange` and names the values, before the database is asked, as in `empty range: no event can lie after "0" and before "1"` or `empty range: no event can lie from "2" up to "1"`. The error names the values only, never the options, since a caller of an API does not know them, and the `httpapi` package answers it with `400 Bad Request` and the error as the message (see [Mapping Errors to Status Codes](#mapping-errors-to-status-codes)). A range that is only empty for now, on the other hand, such as the one after the last event written so far, is no mistake, since events can still come there, so `Read` hands out no events for it, without an error. So a reader that pages backwards with `BeforeEvent` and `NewestFirst` stops at a page that ends with the event `0`, since no event can lie before it.

*Note that the lower bound of `FromLatestEvent` is only known once the database has found the latest event of the type, so together with it, only an upper bound that leaves no room on its own, `BeforeEvent("0")`, is refused that way. The database refuses an upper bound before the latest event of the type, and `Read` then fails with an error of the category `ErrPermanent`, since trying again never helps, and its message keeps the reason the database gives.*

To check the options of a read this way without reading, call the `CheckReadOptions` function with the options, which returns the same error, or `nil`, as `CheckPreconditions` does for the preconditions of a command (see [Using Preconditions](#using-preconditions)). That way, bounds that come from a request can be refused before doing something that must not happen for a read that `Read` refuses, for example recording who reads the history of a book:

```go
options := []architecturekit.ReadOption{
  architecturekit.AfterEvent(after),
  architecturekit.BeforeEvent(before),
}

if err := architecturekit.CheckReadOptions(options...); err != nil {
  // ...
}

// Record who reads the history of the book.

events := architecturekit.Read(context.TODO(), store, architecturekit.ExactSubject("/books/42"), options...)
```

*Note that `CheckReadOptions` panics where `Read` does, for options that contradict each other and for a `nil` option, before it checks any ID. It takes no subjects, since none of the checks depends on them, so only `Read` panics for the zero value of `Subjects`. Options that pass can still make the database refuse the read, such as an upper bound before the latest event of the type of `FromLatestEvent`.*

Every event is verified, like everything else the store reads, before the loop sees it (see [Verifying Events](#verifying-events)). A failure belongs to a category, as with `Load` (see [Handling Errors](#handling-errors)), and ends the iteration. The store stops reading as soon as the loop ends, so breaking out of it after a page is fine.

The events come as they are stored, without upcasters or rules, since there is no state. To get at the data of an event, call the `Decode` function (see [Executing Commands](#executing-commands)).

For anything that `Read` does not offer, call the `ReadEvents` function of the client SDK, which takes the options of the database as they are. The events it hands out are not verified then, and a failure belongs to no category.

### Stepping Through States

Sometimes the states before and after an event are needed, not only the latest one, for example for a history that tells what each event changed. To advance a state by a single stored event, call the `StepStored` function with the state, the state so far, and the event. It runs the upcasters and the `Evolve` rules exactly as reading from the database does, and returns the next state:

```go
books := map[string]Book{}

projection := architecturekit.ProjectionFunc(func(ctx context.Context, event eventsourcingdb.Event) error {
  before := books[event.Subject]

  after, err := architecturekit.StepStored(bookState, before, event)
  if err != nil {
    return err
  }

  if !before.IsBorrowed && after.IsBorrowed {
    // The book was borrowed by this event.
  }

  books[event.Subject] = after
  return nil
})
```

For typed events, for example in a test, call the `Step` function instead.

The given state stays unchanged, so both states are at hand afterwards. For a state that holds slices, maps, pointers, channels, functions, or interfaces, such as a field of type `error` or `any`, which can hold any of the others, that takes a `Clone` function (see [Caching States](#caching-states)). Without one, `Step` and `StepStored` return an error of the category `ErrPermanent`, rather than a next state that may share data with the one before. A state that consists of values only needs no `Clone` function.

*Note that a stored event that the upcasters turn into several events is applied in full, and that `FromLatest` has no effect on a single step.*

### Verifying Events

EventSourcingDB gives every event a hash, which covers its metadata, its data, and the hash of the event before it. If the database runs with a signing key, it also signs every event it hands out. The store checks the hash of every event it reads, without being asked to.

To check the signatures as well, hand over the `WithSignatureVerification` option with the verification key of the database:

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

*Note that a key of another length than `ed25519.PublicKeySize` makes `WithSignatureVerification` panic.*

The store checks every event it reads, for `Execute`, `Load`, and `Read` as well as for every kind of projection, and it does so before any upcaster sees the event. The events that `Execute` has just written are not checked, since they are not read. If an event fails its verification, reading fails with an error of the category `ErrUnverified`, which is a special case of `ErrPermanent` (see [Handling Errors](#handling-errors)). A projection stops rather than skipping the event.

The two checks prove different things:

- A matching hash proves that an event is what was written. It proves no more than that, since whoever can change the stored data can compute a new hash as well.
- A matching signature proves that an event comes from a database that holds the signing key. The database signs events when handing them out, so the signature guards the way from the database to your application, but not the stored data itself.

The hashes are enough wherever reading is under your control. Check the signatures as well where events cross a trust boundary, for example when reading from a database that another organization runs. For details, see [Verifying Event Signatures](https://www.eventfoundation.io/docs/eventsourcingdb/verifying-event-signatures).

*Note that checking a hash takes about a microsecond per event, so there is rarely a reason to turn it off. If there is one, hand over the `WithoutHashVerification` option. It can not be combined with `WithSignatureVerification`, since checking a signature includes checking the hash, so `NewStore` panics if it gets both. Checking a signature, on the other hand, takes some tens of microseconds per event, which adds up when a projection catches up on millions of events.*

*Note that the database signs with the key it has at the moment, so after the signing key is rotated, the store needs the new verification key.*

*Note that neither check detects a history that has been rewritten as a whole. For that, audit the chain of hashes (see [Auditing the Event Store](https://www.eventfoundation.io/docs/eventsourcingdb/auditing-the-event-store)).*

### Composing Subjects

So far, subjects have been composed by hand. To define their structure once, call the `NewSubjectScheme` function with a pattern, and use placeholders in braces for the variable parts:

```go
var bookSubject = architecturekit.NewSubjectScheme("/books/{book}")
```

Each segment of a subject may only contain the characters that EventSourcingDB allows: the letters `A-Z` and `a-z`, the digits `0-9`, underscores, and hyphens. This applies to the literal segments of the pattern as well as to the values that fill its placeholders.

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

To take a subject apart, call the `Match` function. It returns the values by placeholder name, and `false` if the subject does not follow the pattern or has a value that `Build` would refuse:

```go
values, ok := bookSubject.Match("/books/42")
// values["book"] == "42"
```

To take a single value out of a subject, call the `Value` function with the subject and the name of the placeholder. Like `Match`, it returns `false` if the subject does not follow the pattern:

```go
bookID, ok := bookSubject.Value("/books/42", "book")
// bookID == "42"
```

*Note that `Value` panics for a placeholder that the pattern does not have, such as one with a typo, whatever the subject is. The map that `Match` returns yields an empty string for such a name instead, so use `Value` for a single value, and `Match` for several values of the same subject.*

To get the pattern and the names of the placeholders, call the `Pattern` and the `Placeholders` function respectively.

To read or observe the events of all books, for example in a projection, start from the subject that all of them lie under. Call the `Root` function to get it from the scheme, rather than writing it down a second time. It returns the literal segments before the first placeholder, here `/books`, or `/` if the pattern starts with a placeholder:

```go
run := architecturekit.StartProjection(ctx, store, architecturekit.SubjectTree(bookSubject.Root()), projection)
```

*Note that other subjects may lie under the same root, such as `/books/42/reviews/7` under `/books`. Use `Match` in the projection to tell them apart.*

*Note that a malformed pattern panics, including one with a literal segment that contains a character EventSourcingDB does not allow, as does calling `Build` with the wrong number of values, with an empty value, or with a value that contains such a character, for example a slash, a dot, or a space.*

Values that come from outside, such as an ID in a request, may well be empty or contain such characters, and that is not a programming error. So always check them before building a subject: call the `Check` function with the same values as `Build`. It returns an error that says what is wrong, such as which characters a value may contain, instead of panicking. The error may reach the caller of an API, for example through the function that returns a command (see [Handling Commands over HTTP](#handling-commands-over-http)), so it names the placeholder of the value, but neither the package nor the pattern, as in `value for "book" must not be empty`:

```go
if err := bookSubject.Check(bookID); err != nil {
  // ...
}
```

### Defining Views

A view holds the data that queries read. Define the shape of an item as a struct, and call the `NewInMemoryView` function with a function that returns the key of an item, to create a view that holds such items in memory:

```go
type BookItem struct {
  ID            string
  Title         string
  Author        string
  IsBorrowed    bool
  BorrowedUntil string
  EventID       string
}

func newCatalog() *architecturekit.InMemoryView[string, BookItem] {
  return architecturekit.NewInMemoryView(
    func(item BookItem) string { return item.ID },
    architecturekit.RevisionIn(func(item *BookItem) *string { return &item.EventID }),
  )
}

catalog := newCatalog()
```

An item carries no JSON annotations, since what a caller sees is decided by a query and its answer, not by the view (see [Handling Queries over HTTP](#handling-queries-over-http)).

*Note that calling `NewInMemoryView` with `nil` as the function, or as one of the options, panics.*

Every item has a revision of its own, which is the ID of the last event that changed it. The `RevisionIn` option makes the view keep it in a field of the item, so that a caller can hand it over to a command that uses the `OnEventID` function (see [Checking the Revision of the Caller](#checking-the-revision-of-the-caller)). The view sets the field whenever it changes an item, so you never set it yourself. Without the option, the view keeps the revisions to itself.

*Note that calling `RevisionIn` with `nil`, or twice, panics.*

The revision of an item fits such a precondition only if the item stands for exactly one subject, and the projection applies every event type of that subject to the item. The precondition checks the last event of the subject, so as soon as an event lands in the subject that the view does not apply to the item, the two drift apart, and every command with the revision of the item fails with an error of the category `ErrConflict`, until an event changes the item again. Apply an event type that does not change the item, such as one the state ignores, with a change that does nothing (see [Defining Projections](#defining-projections)). For an item that gathers several subjects, such as all books a reader has borrowed, there is no single subject its revision could stand for, so use `OnStateRead` for the commands instead (see [Guarding Against Concurrent Changes](#guarding-against-concurrent-changes)).

Every function that changes the view takes the ID of the event it applies. An ID that is empty or not a revision, such as `abc`, is refused at once, rather than at the next change of the item: the function fails with an error of the category `ErrPermanent` that also wraps `ErrNotARevision`, and the view stays as it is. An event that is not newer than the item it is about is skipped, so applying the same event twice changes nothing, as long as the item is still there. The view forgets the revision of an item it removes, so an event that adds the item, applied again after a later event has removed it, adds it again. A projection that is rebuilt applies every event once, in order, so with a view in memory, that does not happen.

The functions that read and change items take a context and report an error, as a view in a database would need: `All` and `Lookup` hand it out along with the items, and every other one returns it. So a view in a database can offer functions of the same shape, which the handlers of a projection call the same way. The kit has no interface for the functions that change a view, though: a projection takes its view by its type, such as `*InMemoryView`, so moving it to a view in a database changes that type.

If an upcaster splits a stored event into several events, they all carry the ID of the stored event (see [Versioning Events](#versioning-events)). A projection created with `NewTypedProjection` hands each of them to its handler with a context that holds its position among them, and the view reads it from the context it gets. For the same ID, the view counts a later event as newer than an earlier one, so every one of them is applied, in order, also if several of them change the same item. Applying the stored event again still changes nothing, and the revision of the item stays the ID of the stored event. That is why a handler always hands the context it gets on to the view, rather than one of its own, such as `context.Background()`.

*Note that the view as a whole has a revision as well, which is the last event it has seen at all, rather than the last one that changed a particular item (see [Reading Your Own Writes](#reading-your-own-writes)).*

#### Adding Items

To add an item, call the `Insert` function with a context, the ID of the event, and the item:

```go
outcome, err := catalog.Insert(ctx, event.ID, BookItem{
  ID:     "42",
  Title:  "2001 – A Space Odyssey",
  Author: "Arthur C. Clarke",
})
if err != nil {
  // ...
}
```

`Insert` returns an `Outcome`, which tells what it did: `architecturekit.Added` if it added the item, and `architecturekit.AlreadyApplied` if an item with the same key has seen the event, or a newer one, so the event was skipped. The latter is not an error, since it happens when events are applied a second time. So a projection that does more than store the item, for example one that also counts the books, does so only for `architecturekit.Added` (see [Changing and Removing Items](#changing-and-removing-items)).

If the key of the item is already taken, and the event is newer than the item with that key, `Insert` fails with an error of the category `ErrPermanent`, since two items with the same key point to a mistake in the events or in the key. To add an item or change the existing one, call the `Upsert` function instead, with the key and a function that changes the item. It starts from the existing item, or from an empty one if there is none, so it has to set the fields that make up the key:

```go
outcome, err := catalog.Upsert(ctx, "42", event.ID, func(item *BookItem) {
  item.ID = "42"
  item.IsBorrowed = true
})
```

`Upsert` returns an `Outcome` as well: `architecturekit.Added` if it added the item, `architecturekit.Applied` if it changed the existing one, and `architecturekit.AlreadyApplied` if the existing item has seen the event, or a newer one, so the event was skipped.

*Note that an item whose key, after the change, differs from the given one fails with an error of the category `ErrPermanent`, which also catches a function that forgets to set the key.*

#### Reading Items

To read the item with a given key, call the `Get` function. It returns `false` if there is none:

```go
book, isFound, err := catalog.Get(ctx, "42")
if err != nil {
  // ...
}
```

To read all items, call the `All` function. It returns an iterator over a copy of the items, in the order in which they were added, which hands out each item together with an error. Use it e.g. inside a `for range` loop:

```go
for item, err := range catalog.All(ctx) {
  if err != nil {
    // ...
  }

  // ...
}
```

The view in memory never fails while it hands out its items, so the error is always `nil`. A view in a database, however, may fail halfway, which is why every view hands out an error, and every loop checks it.

*Note that the items you get share their slices, maps, and pointees with the view, so never change them (see [Sharing Items with Readers](#sharing-items-with-readers)).*

#### Changing and Removing Items

To change the item with a given key, call the `Update` function with a function that changes it. To remove it, call the `Delete` function. Like `Insert` and `Upsert`, both return an `Outcome`, which tells what they did:

```go
updated, err := catalog.Update(ctx, "42", event.ID, func(item *BookItem) {
  item.IsBorrowed = true
})

deleted, err := catalog.Delete(ctx, "42", event.ID)
```

| Outcome | Meaning |
|---|---|
| `architecturekit.Added` | A new item was added. Only `Insert` and `Upsert` report it. |
| `architecturekit.Applied` | An existing item was changed or removed. |
| `architecturekit.Missing` | There is no item with the key. Only `Update` and `Delete` report it. |
| `architecturekit.AlreadyApplied` | The item has seen the event, or a newer one, so nothing was changed. |

Neither `Missing` nor `AlreadyApplied` is an error, since both happen when events are applied a second time, as a later event may have removed the item already. If an event about an item that does not exist means that the view and the database disagree, say so in the projection:

```go
outcome, err := catalog.Update(ctx, bookID, event.ID, func(item *BookItem) {
  item.IsBorrowed = true
})
if err != nil {
  return err
}
if outcome == architecturekit.Missing {
  return fmt.Errorf("event %s is about book %s, which was never acquired", event.ID, bookID)
}
```

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

*Note that the view stays locked while it runs a function you hand over, such as a change, or the function that selects the items for `UpdateWhere` and `DeleteWhere`. Such a function must not use the same view, not even to read an item with `Get` or `All`, since that blocks forever. The same holds for `Upsert`, and for the `Update` function of an index. To copy data from another item of the view into the one you change, read it before, and use it inside the change.*

#### Sharing Items with Readers

`Get` and `All` hand out plain copies of the items, which share their slices, maps, and pointees with the view. So an item is shared with every reader who got it, also while a projection changes it. That is why a reader must never change an item it got, and why a change replaces a field that holds a slice, a map, or a pointer, rather than writing into it. Writing into it would change what the readers hold, possibly while they are reading it, and writing into a map while somebody reads it may crash the process. Replacing the field with one built anew is safe:

```go
type ReaderItem struct {
  ID       string
  BookIDs  []string
  DueDates map[string]string
}

outcome, err := readers.Update(ctx, event.Data.BorrowedBy, event.ID, func(item *ReaderItem) {
  item.BookIDs = append(slices.Clone(item.BookIDs), bookID)
})
```

*Note that `append(item.BookIDs, bookID)` alone does not build the slice anew: if its array has room left, `append` writes into the array that the readers share.*

To have the view take care of that, hand over the `CloneWith` option with a function that returns a copy of an item that shares no data with the original, like the `Clone` function of a state (see [Caching States](#caching-states)). The view then clones the item before every change, and hands the clone to the change, which may write into it freely:

```go
readers := architecturekit.NewInMemoryView(
  func(item ReaderItem) string { return item.ID },
  architecturekit.CloneWith(func(item ReaderItem) ReaderItem {
    item.BookIDs = slices.Clone(item.BookIDs)
    item.DueDates = maps.Clone(item.DueDates)
    return item
  }),
)

outcome, err := readers.Update(ctx, event.Data.BorrowedBy, event.ID, func(item *ReaderItem) {
  if item.DueDates == nil {
    item.DueDates = map[string]string{}
  }

  item.DueDates[bookID] = event.Data.BorrowedUntil
})
```

The change creates the map if it is `nil`, since an item may have been added without one, and `maps.Clone` keeps `nil`.

The view clones an item whenever it changes an existing one, with `Update`, `Upsert`, `UpdateWhere`, and the `Update` function of an index, but not when it adds one, since nobody has read a new item yet. Readers still never change an item they got, since the view hands out what it holds.

*Note that an index whose value is taken from a slice, a map, or a pointee loses track of an item if a change writes into that data without a clone, since the old item then holds the new value as well (see [Indexing Items](#indexing-items)).*

*Note that a `time.Time` counts as a value, since its location never changes, so an item whose only pointer is the one inside a `time.Time` needs no `CloneWith`.*

*Note that calling `CloneWith` with `nil`, or twice, panics.*

#### Indexing Items

Selecting items with a function reads every item. To find items by a value directly, for example all books by an author, add a secondary index with the `Index` function. It takes a function that returns the value of an item, and returns an index, which offers the `Lookup`, `Update`, and `Delete` functions:

```go
byAuthor := catalog.Index(func(item BookItem) string { return item.Author })

books := byAuthor.Lookup(ctx, "Arthur C. Clarke")

changed, err := byAuthor.Update(ctx, "Arthur C. Clarke", event.ID, func(item *BookItem) {
  item.IsBorrowed = false
})

removed, err := byAuthor.Delete(ctx, "Arthur C. Clarke", event.ID)
```

Several items may share a value. The index follows every change to the view, also when the value of an item changes, and `Lookup` hands out the items in the order in which they were added, together with an error, as `All` does.

*Note that adding an index reads every item, so add indexes before the view is used.*

*Note that calling `Index` with `nil` as the function panics.*

To keep items somewhere else, for example in a database, implement the `View` interface, which consists of the `All` function. It returns an iterator over the items and errors. A database fails not only before it reads the rows, but also while it reads them, for example when the connection breaks halfway. So hand out such an error with an empty item, and stop. Stop as well, and close the rows, as soon as the iterator is told to stop, which happens when the caller has seen enough, for example with `Take` or `First` (see [Defining Queries](#defining-queries)):

```go
type BookTable struct {
  db *sql.DB
}

func (t *BookTable) All(ctx context.Context) iter.Seq2[BookItem, error] {
  return func(yield func(BookItem, error) bool) {
    rows, err := t.db.QueryContext(ctx, "SELECT id, title, author, is_borrowed, borrowed_until, event_id FROM books")
    if err != nil {
      yield(BookItem{}, err)
      return
    }
    defer rows.Close()

    for rows.Next() {
      var item BookItem
      err := rows.Scan(&item.ID, &item.Title, &item.Author, &item.IsBorrowed, &item.BorrowedUntil, &item.EventID)
      if err != nil {
        yield(BookItem{}, err)
        return
      }

      if !yield(item, nil) {
        return
      }
    }

    if err := rows.Err(); err != nil {
      yield(BookItem{}, err)
    }
  }
}
```

The error reaches the caller unchanged, through every function of the `query` package, so a category it belongs to, such as `ErrTransient`, still decides which status code an HTTP route answers with (see [Mapping Errors to Status Codes](#mapping-errors-to-status-codes)).

If the view can find a single item by its key, also implement the `Get` function, which makes it a `KeyedView`:

```go
func (t *BookTable) Get(ctx context.Context, id string) (BookItem, bool, error) {
  // ...
}
```

A query that reads a single item can then take an `architecturekit.KeyedView[string, BookItem]` instead of running over all items, and work with the view in memory and the one in a database alike. `InMemoryView` is a `KeyedView`, too.

### Defining Projections

A projection turns events into a view. Call the `NewTypedProjection` function, and call the `On` function for every event type the view depends on. Each handler receives an `Envelope`, which holds the metadata of the event, such as its `ID`, `Time`, and `Subject`, and its data, decoded into the Go type of the event:

```go
func newCatalogProjection(catalog *architecturekit.InMemoryView[string, BookItem]) *architecturekit.TypedProjection {
  return architecturekit.NewTypedProjection().
    On(func(ctx context.Context, event architecturekit.Envelope[BookAcquired]) error {
      _, err := catalog.Insert(ctx, event.ID, BookItem{
        ID:     bookIDOf(event.Subject),
        Title:  event.Data.Title,
        Author: event.Data.Author,
      })
      return err
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
  bookID, _ := bookSubject.Value(subject, "book")
  return bookID
}
```

`NewTypedProjection` returns a `*TypedProjection`, which is a `Projection` like any other, so you can run, track, and test it as described below. Events without a handler are skipped, since a projection usually reads more events than it depends on. If the data of an event can not be decoded, the projection returns an error of the category `ErrPermanent`. An error returned by a handler is passed on unchanged.

If the view keeps the revisions of its items for a precondition, a skipped event makes the revision of its item fall behind the subject (see [Defining Views](#defining-views)). In that case, give every event type of the subject a handler, also one that does not change the item, such as `BookInspected`, which the state ignores. Its handler calls `Update` with a change that does nothing, which only moves the revision of the item on:

```go
func newCatalogProjection(catalog *architecturekit.InMemoryView[string, BookItem]) *architecturekit.TypedProjection {
  return architecturekit.NewTypedProjection().
    // ...
    On(func(ctx context.Context, event architecturekit.Envelope[BookInspected]) error {
      _, err := catalog.Update(ctx, bookIDOf(event.Subject), event.ID, func(*BookItem) {})
      return err
    })
}
```

To have the projection see the same events as the state, hand over the same set of upcasters with the `UpcastWith` function (see [Versioning Events](#versioning-events)):

```go
catalogProjection.UpcastWith(libraryUpcasters)
```

*Note that calling `On` twice for the same event type panics, and so does calling it with `nil` as the function, or with a pointer or an interface as the event type, as with `Evolve`.*

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

To run a projection, call the `StartProjection` function with a context, the store, the subjects to read, and the projection. Say which subjects with the `SubjectTree` function, which stands for the given subject together with every subject below it, or with the `ExactSubject` function, which stands for the given subject alone. A projection usually reads a tree, such as every book below `/books`. There is no default, so that every projection says which one it means, since the client SDK reads a single subject unless told otherwise.

*Note that a subject that does not start with a slash makes `SubjectTree` and `ExactSubject` panic, and that the zero value of `Subjects`, which names no subject, makes `StartProjection`, the other functions that run a projection, and `Read` panic, as does a `nil` projection. A `nil` pointer or a `nil` `ProjectionFunc` counts as a `nil` projection.*

The function runs the projection in the background and returns a `*ProjectionRun` at once. The run first applies all events that are already stored, then observes new events until the context is canceled. An application usually answers queries only once its views have caught up, since a half-built view answers wrongly rather than slowly, so wait for that with the `WaitCaughtUp` function, which takes a context and the runs:

```go
ctx, cancel := context.WithCancel(context.TODO())
defer cancel()

run := architecturekit.StartProjection(ctx, store, architecturekit.SubjectTree("/books"), catalogProjection,
  architecturekit.Named("catalog"),
)

if err := architecturekit.WaitCaughtUp(ctx, run); err != nil {
  return err
}

// Start to answer queries.
```

The options after the projection are optional. `Named` gives the projection a name, by which the observer of reconnects and the health checks report it (see [Checking Health over HTTP](#checking-health-over-http)). The `Name` function of the run returns it.

*Note that an empty name makes `Named` panic, and that giving `Named` twice makes `StartProjection` and the other functions that run a projection panic, and so does a `nil` option.*

`WaitCaughtUp` waits until every run has applied the events that were stored when it started, and returns `nil` then, and only then, so that `nil` always means that the views are complete. To wait for several projections, hand over all of their runs, as in `architecturekit.WaitCaughtUp(ctx, catalogRun, readersRun)`. It waits for all of them at once, not one after the other, and returns as soon as one of them ends before it has caught up:

- If a run ends before it catches up, `WaitCaughtUp` returns the error the run ended with. If the run has a name, the error names it, as in `projection "catalog": …`, and wraps the error of the run, so that `errors.Is` still finds what that wraps.
- If a run ends without an error before it catches up, because its context ended, `WaitCaughtUp` returns an error that wraps `context.Canceled` and says that the projection stopped before it caught up, naming it if it has a name.
- If the context handed to `WaitCaughtUp` ends first, it returns the error of that context.

A run that has caught up counts as caught up, even if it has ended since, or the context handed to `WaitCaughtUp` has ended. If several of the other cases have happened by the time `WaitCaughtUp` looks, the error a run ended with comes first, since it tells why, that of the first such run among the ones handed over, and the error of the context handed to `WaitCaughtUp` comes next. So if the run and `WaitCaughtUp` share a context, as in the example above, and that context times out, `WaitCaughtUp` returns its error, `context.DeadlineExceeded`, which tells more than `context.Canceled`.

*Note that `WaitCaughtUp` returns `nil` at once without any run, since none of them is behind, and that a `nil` run makes it panic.*

For finer control, the run offers what `WaitCaughtUp` waits for. `CaughtUp` returns a channel that is closed once the run has applied the events that were stored when it started. It is closed only once, and stays closed while the run reconnects later on. If the run ends before it catches up, the channel is never closed, so wait for `Done` as well. `Done` returns a channel that is closed once the run has ended, which happens when the context ends, or on a failure that trying again will not fix. `Err` returns why the run has ended. It returns `nil` as long as the run has not ended, and if it ended because its context did, since canceling the context is how a projection is stopped. If `Apply` returns an error that trying again will not fix, the run ends, and `Err` returns it.

If the projection panics, for example because `Apply` writes into a map that was never made, the run ends as well, rather than the whole process. `Err` then returns an error of the category `ErrPermanent`, since a panic is a mistake in the code that trying again will not fix. Its message holds the value and the stack of the panic, so log it to find out where the panic happened. This holds for every function of the projection that the run calls, and for the observer of reconnects (see below). Once the run has ended, `Liveness` answers `503`, so that the orchestrator restarts the application (see [Checking Health over HTTP](#checking-health-over-http)).

*Note that canceling the context stops the projection, so `defer cancel()` stops it as soon as the surrounding function returns. That is too early for a function that only sets up the application (see [Putting It Together](#putting-it-together)).*

To wait until the run ends, as a process does that runs nothing else, wait for `Done`:

```go
<-run.Done()
return run.Err()
```

*Note that if the database can not be reached at the start, the run keeps trying, and `CaughtUp` stays open. To wait for a limited time only, hand `WaitCaughtUp` a context with a deadline, such as one from `context.WithTimeout`. Its end stops the waiting, but not the run (see [Putting It Together](#putting-it-together)).*

If reading fails with an error of the category `ErrTransient`, or if the database ends the stream, for example because it restarts, the run waits and continues after the last event it has applied, until the context is canceled. The delay starts at one second, doubles with every attempt in a row, and never exceeds one minute. It starts over once the projection has applied an event again, or has caught up and followed the stream for longer than the initial delay, even if no event arrived. That way, a load balancer that ends long-lived connections regularly does not hold back a quiet projection, while a database that fails before the projection has caught up, or within the initial delay after, is given ever more time.

*Note that how long the projection followed the stream is compared with the initial delay, not with the delay it has grown to. Otherwise, once an outage had let the delay grow to one minute, a load balancer that ends connections every 30 seconds would keep a quiet projection waiting for a minute after each of them, and a caller who wants to read their own write in the meantime would not see it (see [Reading Your Own Writes](#reading-your-own-writes)). In return, a database that ends every stream a few seconds after the projection has caught up is tried again every few seconds rather than once a minute. That is not a loop without a pause, and catching up, the expensive part, succeeds each time.*

A connection can also stall without being closed, for example behind a proxy that keeps it open but no longer passes anything on. While there is nothing else to send, the database sends a heartbeat every second, so the client ends a stream on which neither an event nor a heartbeat has arrived for 30 seconds, and the run reconnects as after any other failure of the category `ErrTransient`. Without that, the run would wait for the next event forever, while `Status` kept reporting `PhaseLive`.

To use other delays, or to learn about every attempt, for example to log it, hand over the `WithReconnectDelays` and `WithReconnectObserver` options when creating the store:

```go
store := architecturekit.NewStore(client, "https://library.eventsourcingdb.io",
  architecturekit.WithReconnectDelays(500*time.Millisecond, 30*time.Second),
  architecturekit.WithReconnectObserver(func(reconnect architecturekit.Reconnect) {
    log.Println("observing again", reconnect.Projection, reconnect.Attempt, reconnect.Err, reconnect.Delay)
  }),
)
```

*Note that an initial delay of zero or less panics, since the projections would then read again without any pause, and so does a maximum delay below the initial one. Calling `WithReconnectObserver` with `nil` panics as well.*

The observer receives a `Reconnect` with these fields:

- `Projection` is the name the projection was given with `Named`, or empty if it has none.
- `Subject` is the subject the projection reads.
- `Err` is the reason, which is `nil` if the database ended the stream.
- `Delay` is how long the projection waits before the next attempt.
- `Attempt` counts the attempts in a row, starting at one. It starts over together with the delay once the projection has applied an event, or has followed the stream for longer than the initial delay. So it stays at `1` while a load balancer ends long-lived connections regularly.

*Note that a database that can not be reached is retried as well, since that is usually transient. The observer is how to notice a database that stays unreachable. A failure that trying again will not fix, for example a rejected API token, ends the run (see [Handling Errors](#handling-errors)).*

To find out where a run stands, call the `Status` function. It returns a `ProjectionStatus` with these fields:

- `Phase` is `PhaseCatchingUp`, `PhaseLive`, `PhaseReconnecting`, or `PhaseStopped`.
- `Since` is when the phase began. For `PhaseReconnecting`, that is when the disruption began, not when the latest attempt did.
- `Err` is why the run is reconnecting or has stopped. It is `nil` if the database ended the stream, or if the run stopped because its context ended.
- `Attempts` counts the attempts to read again within the current disruption. Unlike `Attempt` of `Reconnect`, it starts over whenever the run has caught up, even if the stream ends again within the initial delay.
- `Revision` is the ID of the last event the run has applied and committed.
- `HasCaughtUp` tells whether the run has caught up at least once. Like `CaughtUp`, it stays `true` while the run reconnects later on.

```go
status := run.Status()

if status.Phase == architecturekit.PhaseReconnecting && time.Since(status.Since) > 5*time.Minute {
  // The view has not been up to date for more than five minutes.
}
```

To only apply the events that are already stored, for example for a batch job or in a test, call the `CatchUpProjection` function instead. It takes the same arguments and returns once all stored events have been applied. If the context ends before that, it returns the error of the context, so that a read model that is only partly built does not look complete. If the projection panics, it returns the same error a run ends with, rather than panicking:

```go
err := architecturekit.CatchUpProjection(context.TODO(), store, architecturekit.SubjectTree("/books"), catalogProjection)
if err != nil {
  // ...
}
```

For a transactional projection, call the `StartTransactionalProjection` and the `CatchUpTransactionalProjection` function instead (see [Resuming Projections](#resuming-projections)).

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

To make a projection created with `NewTypedProjection` resumable, embed it in a type of your own, and add the two functions there:

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

Instead of implementing `Apply` on the `Tx` yourself, you can use handlers created with `NewTypedProjection`. Build them in `Begin`, so that they write into the transaction that has just been started, and embed them in the `Tx`, which then only needs `Commit` and `Rollback`:

```go
func (p *TransactionalBookTableProjection) Begin(ctx context.Context) (architecturekit.Tx, error) {
  tx, err := p.db.BeginTx(ctx, nil)
  if err != nil {
    // The database can not be reached, which may pass.
    return nil, fmt.Errorf("%w: beginning a transaction: %v", architecturekit.ErrTransient, err)
  }

  return &bookTableTx{
    tx: tx,
    TypedProjection: architecturekit.NewTypedProjection().
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

func (tx *bookTableTx) Commit(ctx context.Context, lastEventID string) error {
  _, err := tx.tx.ExecContext(ctx, "UPDATE checkpoint ...", lastEventID)
  if err != nil {
    return errors.Join(err, tx.tx.Rollback())
  }

  return tx.tx.Commit()
}

func (tx *bookTableTx) Rollback(ctx context.Context) error {
  return tx.tx.Rollback()
}
```

*Note that `Rollback` is not called after `Commit` fails, or panics, so `Commit` has to roll back itself if it can not finish, as above.*

To run a transactional projection, call the `StartTransactionalProjection` or the `CatchUpTransactionalProjection` function instead of `StartProjection` or `CatchUpProjection`. They take the same arguments:

```go
run := architecturekit.StartTransactionalProjection(ctx, store, architecturekit.SubjectTree("/books"), &TransactionalBookTableProjection{},
  architecturekit.Named("book-table"),
)
```

*Note that `StartProjection`, `CatchUpProjection`, and `Tracking` panic for a projection that implements `Transactional` in addition to `Apply`, since calling `Apply` would bypass the transactions.*

The place where a view keeps its data can fail as well, and such a failure counts like an error of `Apply`, whether it comes from `Checkpoint`, `SaveCheckpoint`, `Begin`, `Commit`, or the `Apply` function of a `Tx`. An error that does not belong to `ErrTransient` ends the run, so `Liveness` answers `503`, and the orchestrator restarts the application (see [Checking Health over HTTP](#checking-health-over-http)). That fits a mistake, such as a statement that the database refuses, but not a failure that may pass, such as a lost connection or a deadlock. Report such a failure as an error of the category `ErrTransient`, as `Begin` does above. The run then waits and tries again from where it stopped, with the same growing delay as when reading from EventSourcingDB fails (see [Running Projections](#running-projections)). A panic in any of these functions, on the other hand, always ends the run, since it is a mistake in the code, even if it panics with an error of the category `ErrTransient`. A panic in the `Apply` function of a `Tx` rolls the transaction back first, as an error does.

*Note that which failures may pass depends on the database and its driver, so mark only those. A failure that is marked as transient but never passes keeps the run trying forever, while `Liveness` keeps answering `200`.*

### Batching Events

By default, the checkpoint is saved, or the transaction is committed, after every event. While a projection catches up, for example when it starts for the first time, that takes a lot longer than needed. To apply several events at once while catching up, implement the `Batched` interface on a resumable or transactional projection, and return how many:

```go
func (p *BookTableProjection) CatchUpBatchSize() int {
  return 1000
}
```

Values below `1` count as `1`. For a transactional projection, a batch is one transaction, so a failure rolls back all of its events, and the next attempt starts after the last commit.

Once the projection has caught up, it saves the checkpoint, or commits, after every event again. Events then arrive one at a time, and a batch that waited to fill up would hold back the ones that have already arrived.

*Note that a resumable projection may apply up to that many events a second time after a crash.*

### Publishing Events

Other systems often need to learn about events, for example to send an email once a book has been borrowed, to call a webhook, to fill a search index, or to tell another team. How they learn about them depends on whether they can read the database themselves.

If the other system can read the EventSourcingDB, let it observe the events itself. It then gets every event in the order it was stored, and after a restart, it resumes from the last event it has seen. Nothing needs to be forwarded (see [Observing Events](https://www.eventfoundation.io/docs/eventsourcingdb/sdks/go#observing-events)).

If it can not, because it is a mail server, a webhook, or a message broker, or if it should not depend on how the events of your application look, forward the events with a projection that publishes them instead of writing a view. Do not publish from where a command is executed: if the application stops right after `Execute`, the event is stored, but never published, and nothing tells.

A projection that publishes needs to be resumable (see [Resuming Projections](#resuming-projections)), and it reports a failure of the other system as an error of the category `ErrTransient`:

```go
type LoanMailer struct {
  *architecturekit.TypedProjection
  checkpoints CheckpointStore
}

func NewLoanMailer(mailer Mailer, checkpoints CheckpointStore) *LoanMailer {
  return &LoanMailer{
    TypedProjection: architecturekit.NewTypedProjection().
      On(func(ctx context.Context, event architecturekit.Envelope[BookBorrowed]) error {
        // The ID of the event lets the receiver recognize a mail it got before.
        err := mailer.Send(ctx, event.ID, event.Data.BorrowedBy, "You have borrowed a book.")
        if err != nil {
          return fmt.Errorf("%w: sending a mail: %v", architecturekit.ErrTransient, err)
        }
        return nil
      }),
    checkpoints: checkpoints,
  }
}

func (m *LoanMailer) Checkpoint(ctx context.Context) (string, error) {
  return m.checkpoints.Load(ctx, "loan-mailer")
}

func (m *LoanMailer) SaveCheckpoint(ctx context.Context, eventID string) error {
  return m.checkpoints.Save(ctx, "loan-mailer", eventID)
}
```

Here, `Mailer` and `CheckpointStore` stand for whatever your application uses to send mails and to keep a value. `Checkpoint` and `SaveCheckpoint` hand on the errors of the checkpoint store as they are, so it reports a failure that may pass, such as a lost connection, as an error of the category `ErrTransient` itself. Otherwise, such a failure ends the run (see [Resuming Projections](#resuming-projections)). Start the projection as any other (see [Running Projections](#running-projections)):

```go
run := architecturekit.StartProjection(ctx, store, architecturekit.SubjectTree("/books"), NewLoanMailer(mailer, checkpoints),
  architecturekit.Named("loan-mailer"),
)
```

Since publishing rides on a projection, it behaves like one:

- **It resumes only with a checkpoint.** A projection that is not resumable starts from the first event whenever the application starts, which suits a view held in memory. A publisher would send every event again on every start. Within a run, the position is kept either way, so reconnecting repeats nothing.
- **An event may be published twice.** The checkpoint is saved after an event has been applied, so an event published right before the application stopped is published again after the restart. Hand over the ID of the event, so that the receiver can recognize an event it got before.
- **A transient failure is tried again.** For an error of the category `ErrTransient`, the run tries the failed event again, with a growing delay. Any other error ends the run.
- **Events arrive in order,** one at a time, as they were stored.

On its first start, the checkpoint is empty, so the projection reads every event that has ever been stored, and publishes all of them, for example by sending a mail for every book that has ever been borrowed. To publish only the events from now on, save the ID of the latest event as the checkpoint before the first start. To find it, read the subjects of the projection newest first, and stop after the first event:

```go
checkpoint, err := checkpoints.Load(ctx, "loan-mailer")
if err != nil {
  // ...
}

if checkpoint == "" {
  for event, err := range architecturekit.Read(ctx, store, architecturekit.SubjectTree("/books"), architecturekit.NewestFirst()) {
    if err != nil {
      // ...
    }

    if err := checkpoints.Save(ctx, "loan-mailer", event.ID); err != nil {
      // ...
    }

    break
  }
}
```

If there are no events yet, the checkpoint stays empty, since there is nothing to skip.

A transient failure is tried again without a limit. So if the mail server stays down, the run keeps trying, and since it does not end, `Liveness` keeps answering `200`, and so does `Readiness` once the run has caught up. To notice it, check the `Status` of the run, whose `Phase` stays `PhaseReconnecting`, with `Since` telling since when and `Err` telling why, or log every attempt with the observer of reconnects, whose `Attempt` keeps growing (see [Running Projections](#running-projections)).

*Note that the mode of a projection depends on the functions it implements. To make sure that a publisher is resumable, check its mode in a test (see [Testing Projections](#testing-projections)).*

### Defining Queries

A query describes what someone wants to know. Define it as a struct, and answer it with a function that reads a view. To filter, order, page, and transform the items, use the `query` package:

```go
import "github.com/thenativeweb/architecturekit-golang/architecturekit/query"
```

To turn the items into a slice, call the `Collect` function. If the view fails while it is read, `Collect` returns its error instead:

```go
type ListBooks struct {
  OnlyAvailable bool
  Limit         int
}

func listBooks(catalog architecturekit.View[BookItem]) func(context.Context, ListBooks) ([]BookItem, error) {
  return func(ctx context.Context, q ListBooks) ([]BookItem, error) {
    items := catalog.All(ctx)

    // ...

    return query.Collect(items)
  }
}
```

All functions of the `query` package take an iterator over items and errors, as `All` returns it, and those that return items return such an iterator again, so they can be combined without collecting anything in between. An error of the view ends the iterator. Every function hands it on unchanged, without calling the function you hand over for it, up to the function that returns the result, such as `Collect`, which returns it.

*Note that `Collect` returns no items along with an error, not even the ones it read before, so that a part of the result is never taken for all of it.*

*Note that the `query` package is for reading a view. Once the items are in a slice, for example because an answer groups the same snapshot of a view in several ways, work on the slice with the `slices` package and a loop, such as `slices.SortStableFunc` to order it. A slice can not fail, so handing it to the `query` package would only add errors that never occur.*

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

*Note that ordering reads all items before it hands out the first one, so if the view fails, it hands out the error alone. Ordering is stable, so items that compare as equal keep their order.*

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
first, ok, err := query.First(items)
```

To get the only item, call the `Single` function. It returns `query.ErrNoItems` if there is none, and `query.ErrTooManyItems` if there are several:

```go
type GetBook struct {
  BookID string
}

func getBook(catalog architecturekit.View[BookItem]) func(context.Context, GetBook) (BookItem, error) {
  return func(ctx context.Context, q GetBook) (BookItem, error) {
    return query.Single(query.Where(catalog.All(ctx), func(item BookItem) bool {
      return item.ID == q.BookID
    }))
  }
}
```

Both stop reading as soon as they know the answer, and return an error of the view only if it comes before that. `First` reads no further than the first item, so it never sees an error that comes later. `Single` reads up to the second item, and only knows that an item is the only one once the view has ended, so it also returns an error that comes after the first item. With an error, the item is empty.

*Note that `Where` asks about every item it is handed. To get an item by its key, call the `Get` function of a `KeyedView` instead, and to get items by the value of a secondary index, call the `Lookup` function of the index (see [Defining Views](#defining-views)).*

#### Counting Items

To count items, call the `Count` function. To check whether at least one item matches, call the `Any` function, which stops at the first match:

```go
count, err := query.Count(items)

hasBorrowedBooks, err := query.Any(items, func(item BookItem) bool {
  return item.IsBorrowed
})
```

`Count` reads all items, and if the view fails, it returns the error with `0`. `Any` returns an error only if the view fails before the first match.

### Reading Your Own Writes

A view lags behind the events that have been written, by however long its projection takes. To read your own writes, wait until the view has seen the events you have written.

The ID of the last event a view has seen is its revision. Since the database assigns event IDs in ascending order across all subjects, revisions can be compared.

A view sees only the events of the subjects its projection reads. So wait only for a write to one of those subjects: if the write went to another subject, the view does not reach its revision until a later event lands in one of its own subjects, and waiting for it lasts the full time, with `WaitFor`, with `Await`, and with the `Wait-For-Revision` header alike. `WaitForWritten` even fails then, with an error of the category `ErrTransient`, so hand it only the events written to the subjects the view reads.

#### Tracking Revisions

To track the revision of a view, wrap the projection with the `Tracking` function and hand over the view. It records every event that reaches the projection, including the ones the projection ignores:

```go
trackedProjection := architecturekit.Tracking(catalogProjection, catalog)
```

If the projection writes to several views, hand over all of them, so that each one knows how far it has come:

```go
trackedProjection := architecturekit.Tracking(libraryProjection, catalog, readers, loans)
```

Then run `trackedProjection` instead of `catalogProjection` (see [Running Projections](#running-projections)).

`Tracking` accepts every view that implements the `RevisionSink` interface, which consists of the `Seen` function. `InMemoryView` implements it.

*Note that calling `Tracking` with `nil` as the projection, without any view, or with `nil` as one of the views panics. A `nil` pointer or a `nil` `ProjectionFunc` counts as `nil`, too, such as a view that was declared but never created with `NewInMemoryView`.*

The tracked projection keeps the mode and the batch size of the projection it wraps. A transactional projection can not be tracked, since it has no `Apply` function. Record its revision within the transaction instead.

To get the revision your own write has produced, call the `RevisionOf` function with the written events. It returns the highest event ID, or an empty string if no events were written:

```go
revision := architecturekit.RevisionOf(writtenEvents)
```

#### Waiting for Revisions

To wait until a view has reached a revision, call the `WaitFor` function with a context and the revision. It returns `nil` once the view has reached the revision, at once if it has already. If the context ends first, it returns the error of the context, such as `context.DeadlineExceeded`, and for a value that is not a revision, it returns an error that wraps `ErrNotARevision` (see [Comparing Revisions](#comparing-revisions)):

```go
ctx, cancel := context.WithTimeout(context.TODO(), 5*time.Second)
defer cancel()

err := catalog.WaitFor(ctx, revision)
if err != nil {
  // ...
}
```

*Note that running out of time is an error here. The `Await` function of the `httpapi` package, which waits for the revision an HTTP request asks for, takes it for none instead, and returns `nil`, so that the handler answers with what the view holds (see [Waiting for a Revision in a Handler of Your Own](#waiting-for-a-revision-in-a-handler-of-your-own)).*

To get the current revision of a view, call the `Revision` function with a context. It returns an empty string as long as the view has not seen any event, and an error if it can not read the revision, which may happen to a view of your own that keeps its revision in a database. `InMemoryView` keeps its revision in memory, so it never fails:

```go
current, err := catalog.Revision(ctx)
if err != nil {
  // ...
}
```

Both functions form the `Revisioned` interface, which `InMemoryView` implements. To wait for revisions of a view of your own, implement it as well.

#### Comparing Revisions

To compare two revisions, call the `CompareRevisions` function. Like `cmp.Compare`, it returns `-1`, `0`, or `1`. An empty revision comes before every other one. If a value is not a revision, it returns an error that wraps `ErrNotARevision` and names the value:

```go
result, err := architecturekit.CompareRevisions("9", "10")
// result == -1
```

A revision is a decimal number from `0` to `2^63-1`, the range of the event IDs of the database. To check a revision that comes from outside, such as the one a caller hands over, call the `ParseRevision` function. It reads the revision by the same rules, and returns the number it stands for. Unlike `CompareRevisions`, it refuses an empty revision as well, since that is the ID of no event, and so it is the function to check a revision with before it becomes part of a command (see [Handling Commands over HTTP](#handling-commands-over-http)):

```go
number, err := architecturekit.ParseRevision("42")
// number == 42

_, err = architecturekit.ParseRevision("")
// errors.Is(err, architecturekit.ErrNotARevision) == true
```

For a value that is not a revision, it returns an error that wraps `ErrNotARevision` and names the value, as `CompareRevisions` does.

#### Waiting for Written Events

Sometimes a step on the server builds on what was just written, for example a handler that executes a command and then answers with what a view holds after it. To wait until a view has seen the events that `Execute` or `Write` returned, call the `WaitForWritten` function with a context, the view, the written events, and how long to wait at most:

```go
writtenEvents, err := architecturekit.Execute(ctx, store, borrowBook, cmd)
if err != nil {
  // ...
}

err = architecturekit.WaitForWritten(ctx, catalog, writtenEvents, 5*time.Second)
if err != nil {
  // ...
}
```

It waits with `WaitFor` for the revision that `RevisionOf` returns for the events, and returns `nil` once the view has reached it. If no events were written, it returns `nil` at once, since there is nothing to wait for. If the time runs out first, it returns an error of the category `ErrTransient` that says so, as in `the view did not catch up within 5s`, since the view may still catch up. If the context ends first, it returns the error of the context, and any other error of the view as it is.

*Note that the view sees only the events of the subjects its projection reads, so hand over only the events written to those subjects. For a write to several subjects, such as one with `Write`, the highest ID of all events may lie in a subject the view does not read, and then the view does not reach it until a later event lands in one of its own subjects (see [Waiting for Revisions](#waiting-for-revisions)).*

*Note that running out of time is an error here, since the step needs what was written. The `Await` function of the `httpapi` package, on the other hand, waits for the revision a caller asks for, and takes running out of time for none, so that the handler answers with what the view holds (see [Waiting for a Revision in a Handler of Your Own](#waiting-for-a-revision-in-a-handler-of-your-own)).*

*Note that `WaitForWritten` panics for a `nil` view, a `nil` pointer included, and for a timeout that is not positive, since a view that catches up with a write that has just happened needs some time, and waiting no time at all would fail almost always.*

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

If the function returns an error, neither a command nor a query is run, and the request is answered with `401 Unauthorized`. An error that has a status code of its own keeps it, though (see [Mapping Errors to Status Codes](#mapping-errors-to-status-codes)), and so does an error of the category `ErrPermanent`, which is answered with `500 Internal Server Error`. So if the function can not determine the user because the session store is down, for example, it returns an error of the category `ErrTransient`. The request is then answered with `503 Service Unavailable`, and the failure is logged, rather than sending the caller off to sign in again.

*Note that to answer an error that has a status code of its own with `401 Unauthorized` all the same, the function wraps it with `httpapi.ErrUnauthorized` itself, for example with `fmt.Errorf("%w: %w", httpapi.ErrUnauthorized, err)`.*

*Note that calling `NewAPI` with `nil` as the function panics. The store may be `nil`, though, to test the queries alone (see [Testing Queries](#testing-queries)).*

For an application without authentication, call the `NewPublicAPI` function instead. Commands and queries then receive `httpapi.NoUser` as user:

```go
api := httpapi.NewPublicAPI(store)
```

Everything that answers through an API logs every error it does not explain to the caller in full, once, with the method and the route of the request: the routes it wires up, and the functions that answer in a handler of your own. A failure of the server is logged at level `Error`, and a refusal whose details the caller is not told, such as one with `401`, `409`, or a `404` for a query that found no item, at level `Info` (see [Handling Commands over HTTP](#handling-commands-over-http)). Only a request that was canceled is not logged, since nothing failed. A panic is logged with its value and its stack. By default, they use the default logger of `log/slog`. To use the logger of your application instead, hand over the `WithLogger` option, which `NewPublicAPI` accepts as well:

```go
api := httpapi.NewAPI(store, userFrom, httpapi.WithLogger(logger))
```

*Note that calling `WithLogger` with `nil` panics, and so does calling `NewAPI` or `NewPublicAPI` with `nil` as one of the options.*

#### Determining the User

To determine the user in a handler of your own, call the `UserOf` function. If the user can not be determined, it returns an error that wraps `httpapi.ErrUnauthorized` as well as the error of the function, so that `errors.Is` and `errors.As` find either. If the error of the function has a status code of its own, though, it returns that error as it is (see [Setting Up an HTTP API](#setting-up-an-http-api)):

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

To accept a command over HTTP, define a request type with JSON annotations for the body, and a function that returns the command. It receives the request, the body, decoded into the request type, and the user, as the function that returns a query does (see [Handling Queries over HTTP](#handling-queries-over-http)). So it takes from the request what the body does not hold, such as a value of the path, a header, or the context. For example, for the `BorrowBook` that checks the revision of the caller (see [Checking the Revision of the Caller](#checking-the-revision-of-the-caller)), with the ID of the book in the path:

```go
type borrowBookRequest struct {
  BorrowedUntil   string `json:"borrowedUntil"`
  ExpectedEventID string `json:"expectedEventId"`
}

func toBorrowBook(r *http.Request, request borrowBookRequest, user User) (BorrowBook, error) {
  bookID := r.PathValue("id")
  if err := bookSubject.Check(bookID); err != nil {
    return BorrowBook{}, err
  }
  if _, err := time.Parse(time.DateOnly, request.BorrowedUntil); err != nil {
    return BorrowBook{}, errors.New("borrowedUntil must be a date")
  }
  if _, err := architecturekit.ParseRevision(request.ExpectedEventID); err != nil {
    return BorrowBook{}, errors.New("expectedEventId must be an event ID")
  }

  return BorrowBook{
    BookID:          bookID,
    ReaderID:        user.ID,
    BorrowedUntil:   request.BorrowedUntil,
    ExpectedEventID: request.ExpectedEventID,
  }, nil
}
```

The function is the place to validate a request, since an error it returns is answered with `400 Bad Request`, unless it has a status code of its own (see [Authorizing Commands](#authorizing-commands)). Check at least what would otherwise fail later: the ID of the book becomes part of a subject, and `Build` panics on an empty ID or one with a character that a subject may not contain, such as a slash or a dot (see [Composing Subjects](#composing-subjects)), which is answered with `500 Internal Server Error`. A value of the path is no exception, since it may hold a slash, sent as `%2F`. And a value that does not match the schema of its event is refused by the database, which is a permanent failure answered with `500 Internal Server Error` – although it is the caller's mistake. An expected event ID that is empty or not an event ID at all is answered with `400 Bad Request` without a check of your own, since `OnEventID` refuses it before anything is read (see [Checking the Revision of the Caller](#checking-the-revision-of-the-caller)). Checking it here lets the message name the field, though. `ParseRevision` refuses both, unlike `CompareRevisions`, which takes an empty revision for the one of a view that has seen nothing (see [Comparing Revisions](#comparing-revisions)).

Then call the `Route` function with the API, the mux, a pattern, the function that returns the command, and the decider:

```go
httpapi.Route(api, mux, "POST /api/books/{id}/borrow", toBorrowBook, borrowBook)
```

A command changes something, so the pattern names a method that may do so, usually `POST`. A pattern without a method, such as `/api/books/{id}/borrow`, accepts every method, `GET` included, and `GET`, `HEAD`, `OPTIONS`, and `QUERY` must not change anything. A browser sends `GET` and `HEAD` from another site without asking, for example for a link that the user follows, along with the cookies of the user, and a route that takes no body lets `GET`, `HEAD`, and `OPTIONS` pass from another origin (see [Handling Commands Without a Body](#handling-commands-without-a-body)). `QUERY` may change nothing either, since HTTP defines it as safe and repeatable, so a client or a cache may send it again. So `Route` panics for a pattern without a method, or with one of these, rather than executing the command for any site that links to it, or whenever a query is sent again.

The route decodes the request body, builds the command, and executes it:

```shell
curl -X POST http://localhost:8080/api/books/42/borrow \
  -H "Content-Type: application/json" \
  -d '{"borrowedUntil":"2026-10-24","expectedEventId":"0"}'
```

If this succeeds, it answers with `200 OK` and the revision it has written, which is the ID of the last written event:

```json
{ "revision": "1" }
```

The revision is empty if the command did not write anything. A caller hands it to a query to read its own writes (see [Reading Your Own Writes over HTTP](#reading-your-own-writes-over-http)).

Otherwise, it answers with the status code that matches the error (see [Mapping Errors to Status Codes](#mapping-errors-to-status-codes)) and a message:

```json
{ "message": "book 42 is already borrowed" }
```

The message is the error message if the error is written for the caller, such as a broken business rule or what is wrong with the request. Other errors may name internals, such as the key a token failed to verify with, or the subject a precondition guarded, so their message is fixed:

| Status code | Message |
|---|---|
| `401 Unauthorized` | `unauthorized` |
| `404 Not Found`, for a query that found no item | `not found` |
| `409 Conflict` | `conflict: the data has changed since it was read` |
| `499 Client Closed Request` | `request canceled` |
| `500` and above | `internal server error` |

The actual error is logged, so that it does not vanish (see [Setting Up an HTTP API](#setting-up-an-http-api)): at level `Info` for `401`, `404`, and `409`, since the server did not fail, and at level `Error` for `500` and above. For `404`, that holds only for a query that found no item (see [Reporting Missing Items](#reporting-missing-items)). A request that was canceled, usually because the caller went away, is not logged, since nothing failed.

If handling a request panics, for example because `Build` received an ID that was not checked, the route answers with `500 Internal Server Error` and the message `internal server error`, like any other internal failure, and logs the panic at level `Error`, with its value and its stack. Otherwise, `net/http` would close the connection, and the caller would get no answer at all.

*Note that a panic with `http.ErrAbortHandler` is passed on, since `net/http` expects it to abort the response.*

To answer this way in a handler of your own, call the `Respond` function with the response writer, the request, the API, the written events, and the error. Like the route, it logs through the logger of the API, with the route of the request. Unlike the route, it does not know where an error comes from, so an error without a status code of its own is answered with `500 Internal Server Error`, also if the handler has found a mistake in the request itself. Wrap such an error with `httpapi.ErrMalformed`, for example with `fmt.Errorf("%w: %w", httpapi.ErrMalformed, err)`, to answer it with `400 Bad Request`.

*Note that the function has the type `httpapi.ToCommand`. The request type only describes the body, so it may come from another package, for example one that the application shares with its clients.*

*Note that calling `Route` with `nil` as the API, as the function, or as one of the options, or with a `Decider` that was not created with `NewDecider`, panics, rather than failing every request.*

#### Handling Commands Without a Body

Some commands take everything they need from the path, such as `ReturnBook`, which only needs the ID of the book. Such a command takes no body, so its function has the request type `httpapi.NoBody`:

```go
func toReturnBook(r *http.Request, _ httpapi.NoBody, user User) (ReturnBook, error) {
  bookID := r.PathValue("id")
  if err := bookSubject.Check(bookID); err != nil {
    return ReturnBook{}, err
  }

  return ReturnBook{BookID: bookID}, nil
}

httpapi.Route(api, mux, "POST /api/books/{id}/return", toReturnBook, returnBook)
```

The route then expects no body, so the caller sends neither a `Content-Type` header nor a body:

```shell
curl -X POST http://localhost:8080/api/books/42/return
```

A body of `{}` is accepted as well, whatever the `Content-Type` header says, so that a caller that sends one out of habit keeps working. Any other body is answered with `400 Bad Request`, and the message says that the route takes no body.

Requiring `application/json` is what keeps a browser from sending a command from another site without asking the server first, since a form can not send JSON. A route without a body can not rely on that, so before it looks at the body, it checks where the request comes from, with the `CrossOriginProtection` of `net/http`. A request that a browser sends from another origin, which the `Sec-Fetch-Site` header says, or, without it, an `Origin` header whose host differs from the `Host` header, is answered with `403 Forbidden`, and the error is `httpapi.ErrForbidden`. A request from the same origin passes, and so does one without these headers, such as one of `curl` or of another server.

*Note that this also refuses a browser frontend that runs on another origin than the API, even on another subdomain, so such a frontend can not call a route without a body for now.*

The check lets a request with `GET`, `HEAD`, or `OPTIONS` pass from any origin, since these methods must not change anything. That is why `Route` refuses a pattern with one of them, or without a method, which accepts every method (see [Handling Commands over HTTP](#handling-commands-over-http)). Wire a handler of your own that executes a command to a method that may change something as well, such as `POST`.

#### Adding to the Answer

To answer with more than the revision, for example with the ID that `toAcquireBook` below makes up for a new book, hand over the `Adding` option. It takes a function that receives a `Handled` value with the command and the written events, and returns the fields to add, usually as a struct with JSON annotations, and an error:

```go
type acquireBookRequest struct {
  Title  string `json:"title"`
  Author string `json:"author"`
  ISBN   string `json:"isbn"`
}

func toAcquireBook(r *http.Request, request acquireBookRequest, user User) (AcquireBook, error) {
  return AcquireBook{
    BookID: rand.Text(),
    Title:  request.Title,
    Author: request.Author,
    ISBN:   request.ISBN,
  }, nil
}

httpapi.Route(api, mux, "POST /api/acquire-book", toAcquireBook, acquireBook,
  httpapi.Adding(func(handled httpapi.Handled[AcquireBook]) (any, error) {
    return struct {
      ID string `json:"id"`
    }{handled.Command.BookID}, nil
  }))
```

The route then answers with both:

```json
{ "id": "…", "revision": "1" }
```

The function is only called if the command has succeeded. A value that encodes to `null` adds no fields. That is `nil`, and also a `nil` pointer of a concrete type, which an interface does not count as `nil`, such as the one that `return findShelf(handled)` hands back if `findShelf` returns a `*Shelf` and an error, and fails. The fields are encoded as the result of a query is, so a `nil` slice or map in them is answered as `[]` or `{}` (see [Handling Queries over HTTP](#handling-queries-over-http)).

If the function returns an error, the events are written all the same, so the route still answers with `200 OK` and the revision, which the caller needs to read its own writes, and must not take for a reason to send the command again, whatever is wrong with the fields the function returned along with the error. The answer then holds these fields if they can be used, and none otherwise. The error is logged through the logger of the API, with the route, and so is why the fields were dropped, if they were.

The kit adds the revision itself, so the fields must not contain one, and they must encode to a JSON object, so they must not hold `NaN`, for example, which JSON has no number for. Otherwise, unless the function has returned an error, the route answers with `500 Internal Server Error` and logs why, although the events have been written, since that is a mistake in the code rather than something that happens at runtime.

*Note that the written events are available in `Handled` as well. Add them only deliberately: they are the inner model of the application, every caller that reads them depends on their shape, and they may contain data that is not meant for the caller.*

*Note that calling `Adding` with `nil`, or giving it twice, panics.*

#### Answering Commands in Your Own Format

To answer in a format of your own, for example with another status code, call the `Handle` function in a handler of your own. It does the same as a route, but writes nothing to the response. Instead, it returns a `Handled` value with the command it has built and the written events:

```go
mux.HandleFunc("POST /api/acquire-book", func(w http.ResponseWriter, r *http.Request) {
  handled, err := httpapi.Handle(r, api, toAcquireBook, acquireBook)
  if err != nil {
    httpapi.Respond(w, r, api, nil, err)
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

*Note that `Handle` returns a panic as an error, which `StatusFor` maps to `500`, and which `Respond` logs with the value and the stack of the panic.*

*Note that calling `Handle` with `nil` as the API or as the function, or with a `Decider` that was not created with `NewDecider`, panics, as with `Route`, but on every request, even one whose caller is unknown. That panic, too, comes back as an error, which names the mistake.*

#### Authorizing Commands

To refuse a command, return `httpapi.ErrForbidden` from the function that returns it. The request is then answered with `403 Forbidden`, and the command is not executed:

```go
func toAcquireBook(r *http.Request, request acquireBookRequest, user User) (AcquireBook, error) {
  if !user.IsLibrarian {
    return AcquireBook{}, httpapi.ErrForbidden
  }

  return AcquireBook{
    BookID: rand.Text(),
    Title:  request.Title,
    Author: request.Author,
    ISBN:   request.ISBN,
  }, nil
}
```

The same applies to every error that has a status code of its own (see [Mapping Errors to Status Codes](#mapping-errors-to-status-codes)), such as `httpapi.ErrNotFound` or an error of the category `ErrDomain`, and to an error of the category `ErrPermanent`, which is answered with `500 Internal Server Error`. So if the function looks something up in another service, with the context of the request, and that service is down, it returns an error of the category `ErrTransient`. The request is then answered with `503 Service Unavailable`, and the failure is logged, rather than blaming the request.

Any other error returned from the function is answered with `400 Bad Request`, with the error as the message, after the text of `httpapi.ErrMalformed`, as in `malformed request: borrowedUntil must be a date`. In `Handle`, it wraps `httpapi.ErrMalformed` as well as the original error, so that `errors.Is` and `errors.As` find either.

#### Validating Requests

Before the function that returns the command receives the body, the request is validated, as it is for a query (see [Handling Queries over HTTP](#handling-queries-over-http)):

- The `Content-Type` header must be `application/json`, otherwise the request is answered with `415 Unsupported Media Type`, and the error is `httpapi.ErrUnsupportedMediaType`.
- The body must not be larger than `httpapi.MaxRequestBody`, which is one mebibyte, otherwise the request is answered with `413 Request Entity Too Large`, and the error is `httpapi.ErrTooLarge`. The same goes for a body that a middleware cuts off at a lower limit with `http.MaxBytesReader`.
- The body must be a single valid JSON value without unknown fields, otherwise the request is answered with `400 Bad Request`, and the error is `httpapi.ErrMalformed`, which wraps the error of decoding, so that `errors.As` finds it. Nothing but whitespace may follow the value, and no name may occur twice in an object. Names match fields regardless of case, as with `encoding/json`, so two names that match the same field count as the same name, even if they differ in case. The body must not be `null`, which `encoding/json` would turn into the zero value of the request type, unless the request type decodes itself, and so decides itself what `null` means, or is an interface, which takes `null` for `nil`, while `null` for a field still leaves the field as it is. And an array must have as many elements as the Go array it is decoded into, such as a `[2]int`, which `encoding/json` would otherwise fill up with zeros, or cut off.

*Note that parsers disagree on what a name that occurs twice means, and on data after the value: one takes the first value, another the last, and one stops after the value, while another reads on. A filter or a proxy in front of the application might then check another value than the one the application uses, which is why both are refused.*

If the body can not be decoded, the message says what is wrong in words of its own, rather than in those of the decoder, which name the types of Go. After `malformed request: `, it says, for example, `empty body`, `invalid JSON`, `unknown field "borowedUntil"`, `duplicate field "expectedEventId"`, `"borrowedUntil" must be a string`, `the body must be an object`, for a body of `null` as well, or `"pair" must have 2 elements`. A nested value is named by its path, such as `"items.0.bookId"`. For a failure that it has no words of its own for, such as a value of a type that JSON has no kind for, it says that the value can not be decoded, as in `"rating" can not be decoded`, or `the body can not be decoded`. A body that can not even be read, for example because a middleware fails to decompress it, gives `the body could not be read`, without the words of the middleware. The error of a type that decodes itself, on the other hand, keeps its own text, as it is, also for a type of a library, such as `netip.Addr`, as long as it can tell its text. If its `Error` function panics, as that of a `*json.UnmarshalTypeError` without a type does, the message says that the value can not be decoded instead. For the full list, see the documentation of `BodyOf`.

If the request type is `httpapi.NoBody`, the request is validated differently (see [Handling Commands Without a Body](#handling-commands-without-a-body)): a request that a browser sends from another origin is answered with `403 Forbidden` first, and the error is `httpapi.ErrForbidden`. The `Content-Type` header is not required, and the body must be empty or `{}`, otherwise the request is answered with `400 Bad Request`, and the error is `httpapi.ErrMalformed`. A body larger than `httpapi.MaxRequestBody` is still answered with `413 Request Entity Too Large`.

To read a body by the same rules in a handler of your own, call the `BodyOf` function with the request type as its type parameter, as in `httpapi.BodyOf[borrowBookRequest](r)`. With `httpapi.NoBody`, it checks that a request has no body, as the route does.

### Handling Queries over HTTP

A query is asked with the method `QUERY`, which HTTP defines in RFC 10008. Like `GET`, it is safe, so it changes nothing, but like `POST`, it carries a body, so a query takes its input from the body, as a command does. To answer a query over HTTP, define a request type with JSON annotations for the body, and a function that returns the query. Like the function that returns a command, it receives the request, the body, decoded into the request type, and the user:

```go
type listBooksRequest struct {
  OnlyAvailable bool `json:"onlyAvailable"`
  Limit         int  `json:"limit"`
}

func toListBooks(r *http.Request, request listBooksRequest, user User) (ListBooks, error) {
  return ListBooks{
    OnlyAvailable: request.OnlyAvailable,
    Limit:         request.Limit,
  }, nil
}
```

As for a command, the request type describes the body, and the query is what the application works with, so the query needs no JSON annotations. The function builds the query from the request type, so it may check the input, and add what the body does not hold, such as the user or a value of the path, as `toBorrowBook` adds the reader to the command (see [Handling Commands over HTTP](#handling-commands-over-http)).

Items carry no JSON annotations (see [Defining Views](#defining-views)). So to answer with JSON, define a response type with JSON annotations, as the counterpart of the request types that commands use (see [Handling Commands over HTTP](#handling-commands-over-http)), and map the items to it. Here, the revision of a book goes along as `eventId`, so that a caller can send it back as `expectedEventId`:

```go
type bookBody struct {
  ID            string `json:"id"`
  Title         string `json:"title"`
  Author        string `json:"author"`
  IsBorrowed    bool   `json:"isBorrowed"`
  BorrowedUntil string `json:"borrowedUntil"`
  EventID       string `json:"eventId"`
}

func bookBodyOf(book BookItem) bookBody {
  return bookBody{
    ID:            book.ID,
    Title:         book.Title,
    Author:        book.Author,
    IsBorrowed:    book.IsBorrowed,
    BorrowedUntil: book.BorrowedUntil,
    EventID:       book.EventID,
  }
}
```

To answer with such bodies, wrap the function that answers the query with items, such as `listBooks` (see [Defining Queries](#defining-queries)), and map what it returns. Since every query that lists books returns items, one wrapper serves all of them:

```go
func answerBooks[TQuery any](ask httpapi.Answer[TQuery, []BookItem]) httpapi.Answer[TQuery, []bookBody] {
  return func(ctx context.Context, q TQuery) ([]bookBody, error) {
    books, err := ask(ctx, q)
    if err != nil {
      return nil, err
    }

    bodies := make([]bookBody, 0, len(books))
    for _, book := range books {
      bodies = append(bodies, bookBodyOf(book))
    }

    return bodies, nil
  }
}
```

Then call the `Query` function with the API, the mux, a pattern, the function that returns the query, and the function that answers it:

```go
httpapi.Query(api, mux, "QUERY /api/books", toListBooks, answerBooks(listBooks(catalog)))
```

The pattern names the method `QUERY`. A pattern without a method, which accepts every method, or with another method, makes `Query` panic. `net/http` does not name the method yet, so the constant `httpapi.MethodQuery` does, as `http.MethodGet` names `GET`, for example for a request in a test. A handler of your own that answers with `Ask` may use another method, though, for example `GET` for a download or a CSV export (see [Answering Queries in Your Own Format](#answering-queries-in-your-own-format)).

The caller sends the input as JSON, as for a command:

```shell
curl -X QUERY http://localhost:8080/api/books \
  -H "Content-Type: application/json" \
  -d '{"onlyAvailable":true}'
```

The route determines the user, and then decodes the body by the same rules as for a command (see [Validating Requests](#validating-requests)), before it calls the function that returns the query. So a body that does not claim to be JSON, that is too large, or that is not valid JSON or does not fit the request type is answered with `415`, `413`, or `400`, and the same message as for a command.

A query without input, such as one that takes everything it needs from the path, has the request type `httpapi.NoBody`, so that the caller sends no body, or `{}`. As for a command without a body, a request that a browser sends from another origin is then answered with `403 Forbidden` (see [Handling Commands Without a Body](#handling-commands-without-a-body)):

```go
func toGetBook(r *http.Request, _ httpapi.NoBody, user User) (GetBook, error) {
  return GetBook{BookID: r.PathValue("id")}, nil
}
```

The route answers with `200 OK` and the result as JSON, with `Cache-Control: no-store`, so that no cache keeps it (see [Reading Your Own Writes over HTTP](#reading-your-own-writes-over-http)). The result is encoded as `encoding/json` encodes it, except that a `nil` slice is answered as an empty list, `[]`, and a `nil` map as an empty object, `{}`, at every depth, such as the `nil` slice that `query.Collect` returns when there are no items, or a field of a response type that holds a `nil` slice. So a caller gets a list or an object, whether it holds anything or not. A `nil` pointer, on the other hand, is answered as `null`. A result that can not be encoded, for example because it holds `NaN`, is a mistake in the code, and is answered with `500 Internal Server Error` and logged, like any other internal failure. Errors and panics are answered as for commands, and errors returned from the first function are treated as they are from the function that returns a command (see [Authorizing Commands](#authorizing-commands)).

To answer this way in a handler of your own, call the `RespondResult` function with the response writer, the request, the API, the result, and the error. As with `Respond`, an error without a status code of its own is answered with `500 Internal Server Error`, so wrap a mistake in the request that the handler finds itself with `httpapi.ErrMalformed` (see [Handling Commands over HTTP](#handling-commands-over-http)). To answer an error without a result, call the `RespondError` function (see [Answering Queries in Your Own Format](#answering-queries-in-your-own-format)).

*Note that a `nil` slice of bytes is answered as an empty string, `""`, since a slice of bytes is encoded as a string in base64, and that a `nil` `json.RawMessage` is answered as `null`, since it holds JSON text.*

*Note that the functions have the types `httpapi.ToQuery` and `httpapi.Answer`. The answering function receives neither the request nor the user.*

*Note that calling `Query` with `nil` as the API, for either function, or as one of the options, panics, rather than failing every request.*

#### Answering Queries in Your Own Format

To answer in a format of your own, call the `Ask` function in a handler of your own. It does the same as a route, but writes nothing to the response. Instead, it returns the result. Such a handler may use another method than `QUERY`, for example `GET` for a CSV export that a browser can download from a link. A `GET` request carries no body, so its function has the request type `httpapi.NoBody`, and takes the input from the URL instead:

```go
toExportBooks := func(r *http.Request, _ httpapi.NoBody, user User) (ListBooks, error) {
  return ListBooks{
    OnlyAvailable: r.URL.Query().Get("available") == "true",
  }, nil
}

mux.HandleFunc("GET /api/books.csv", func(w http.ResponseWriter, r *http.Request) {
  books, err := httpapi.Ask(r, api, toExportBooks, listBooks(catalog))
  if err != nil {
    httpapi.RespondError(w, r, api, err)
    return
  }

  w.Header().Set("Content-Type", "text/csv")
  // ...
})
```

To answer an error the way a route does, call the `RespondError` function with the response writer, the request, the API, and the error, as above. It answers with the same status codes and messages as `RespondResult`, says `Cache-Control: no-store`, and logs the same way, but needs no result. As with `RespondResult`, an error without a status code of its own is answered with `500 Internal Server Error`.

*Note that a `GET` request without a body passes from any origin, unlike a `QUERY` request without one, since `GET` must not change anything (see [Handling Commands Without a Body](#handling-commands-without-a-body)).*

*Note that calling `RespondError` with `nil` as the API, or with a `nil` error, panics.*

*Note that `Ask` returns a panic as an error, as `Handle` does (see [Answering Commands in Your Own Format](#answering-commands-in-your-own-format)).*

*Note that calling `Ask` with `nil` as the API, or for either function, panics, as with `Query`, but on every request, even one whose caller is unknown. That panic, too, comes back as an error, which names the mistake.*

#### Asking for Several Items at Once

Since a `QUERY` request carries a body, the input of a query may hold more than a URL could, for example a list of books to check at once. Here, the answer tells for every book whether it is available:

```go
type CheckAvailability struct {
  BookIDs []string
}

func checkAvailability(catalog architecturekit.KeyedView[string, BookItem]) func(context.Context, CheckAvailability) (map[string]bool, error) {
  return func(ctx context.Context, q CheckAvailability) (map[string]bool, error) {
    isAvailable := make(map[string]bool, len(q.BookIDs))
    for _, bookID := range q.BookIDs {
      book, isFound, err := catalog.Get(ctx, bookID)
      if err != nil {
        return nil, err
      }

      isAvailable[bookID] = isFound && !book.IsBorrowed
    }

    return isAvailable, nil
  }
}

type checkAvailabilityRequest struct {
  BookIDs []string `json:"bookIds"`
}

func toCheckAvailability(r *http.Request, request checkAvailabilityRequest, user User) (CheckAvailability, error) {
  return CheckAvailability{BookIDs: request.BookIDs}, nil
}

httpapi.Query(api, mux, "QUERY /api/check-availability", toCheckAvailability, checkAvailability(catalog))
```

#### Reporting Missing Items

If the answering function returns `query.ErrNoItems`, as `query.Single` does if no item matches, the request is answered with `404 Not Found`, and the message `not found`, since the text of `query.ErrNoItems`, `no items`, does not say what was not found. The error is logged at level `Info`, as for `401` and `409`. So a wrapper that maps a single item hands on the error as it is:

```go
func answerBook[TQuery any](ask httpapi.Answer[TQuery, BookItem]) httpapi.Answer[TQuery, bookBody] {
  return func(ctx context.Context, q TQuery) (bookBody, error) {
    book, err := ask(ctx, q)
    if err != nil {
      return bookBody{}, err
    }

    return bookBodyOf(book), nil
  }
}

httpapi.Query(api, mux, "QUERY /api/books/{id}", toGetBook, answerBook(getBook(catalog)))
```

Here, `toGetBook` is the function of a query without input, whose request type is `httpapi.NoBody` (see [Handling Queries over HTTP](#handling-queries-over-http)).

To report a missing item yourself, return `httpapi.ErrNotFound`, or an error that wraps it, such as `fmt.Errorf("%w: book %s", httpapi.ErrNotFound, q.BookID)`. Such an error is written for the caller, so it is the message, here `not found: book 42`.

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
| `context.Canceled` | `499 Client Closed Request` |
| `context.DeadlineExceeded` | `503 Service Unavailable` |
| `architecturekit.ErrPermanent` | `500 Internal Server Error` |
| `architecturekit.ErrNotARevision`, `architecturekit.ErrEmptyRange` | `400 Bad Request` |
| any other error | `500 Internal Server Error` |

*Note that `context.Canceled` means that the caller went away before it got an answer. HTTP has no status code for that, so `499` is the one that nginx introduced, and which logs and metrics commonly know. Since nothing failed, it is not logged. Its message is `request canceled`, because the error may name internals, such as the subject that was read.*

*Note that `ErrNotARevision` means that a value that was handed over is not a revision, such as a bound of `Read`, the event ID of `OnEventID`, a value for `CompareRevisions` or `ParseRevision`, or the revision a view is to wait for, which usually comes from the request. So it is answered with `400 Bad Request` and the error as the message, like any other mistake in the request, also if it is the function that answers a query that finds it. An ID that the server stored or made itself and that is broken is a failure of the server, though, so an error of the category `ErrPermanent` is answered with `500 Internal Server Error`, even if it wraps `ErrNotARevision` as well. Both come last, so that an error that belongs to another category as well keeps its status code.*

*Note that `ErrEmptyRange` means that the bounds of `Read` leave no room for any event, which usually come from the request as well. So it is answered the same way, with `400 Bad Request` and the error as the message, such as `empty range: no event can lie before "0"`, unless the error belongs to the category `ErrPermanent` as well (see [Reading Events](#reading-events)).*

*Note that an error of the function that returns a command, of the one that returns a query, or of the one that determines the user keeps its status code only if it has one of its own, or belongs to the category `ErrPermanent`. Any other error is answered with `400 Bad Request` for the first two, and with `401 Unauthorized` for the last (see [Authorizing Commands](#authorizing-commands) and [Setting Up an HTTP API](#setting-up-an-http-api)).*

*Note that a panic while a route handles a request is answered with `500 Internal Server Error` as well (see [Handling Commands over HTTP](#handling-commands-over-http)).*

*Note that the status code says nothing about what to tell the caller. If you answer in a format of your own, leave out the error for `401`, `409`, `499`, and `500` and above, as `Respond`, `RespondResult`, and `RespondError` do, since it may name internals, and say `not found` for `query.ErrNoItems` (see [Handling Commands over HTTP](#handling-commands-over-http)).*

### Reading Your Own Writes over HTTP

To let a caller read its own writes over HTTP, hand over the `Revisioned` option to `Query`, with a view that implements `Revisioned`, whose projection is tracked (see [Tracking Revisions](#tracking-revisions)), and how long to wait at most:

```go
httpapi.Query(api, mux, "QUERY /api/books", toListBooks, answerBooks(listBooks(catalog)),
  httpapi.Revisioned(catalog, httpapi.DefaultWait),
)
```

After sending a command, the caller takes the revision from the answer and sends it in the `Wait-For-Revision` header of the query:

```shell
curl -X QUERY http://localhost:8080/api/books \
  -H "Content-Type: application/json" \
  -H "Wait-For-Revision: 1" \
  -d '{}'
```

The route waits until the view has reached this revision, but at most for the given duration, which is five seconds for `httpapi.DefaultWait`. Then it answers with what the view holds, even if the time has run out. Without the header, it does not wait at all. If the header holds something that is not a revision, the request is answered with `400 Bad Request`. If the view can not read its revision once it has waited, the request is answered with the error, as with any other error, for example with `503 Service Unavailable` for an error of the category `ErrTransient` (see [Mapping Errors to Status Codes](#mapping-errors-to-status-codes)).

Once the view has seen at least one event, the response contains the revision it shows in the `Revision` header, as well as an `ETag` header and `Cache-Control: private, no-cache`. `no-cache` makes a cache ask again before it hands out the answer, and `private` keeps shared caches, such as proxies, from keeping it at all. If the caller sends the `ETag` in the `If-None-Match` header, asks the same, and the view has not changed since, the request is answered with `304 Not Modified`, which carries the same headers, and no body.

The header is read as HTTP has it: it may hold a list of tags, separated by commas, or `*`, which stands for any tag. A tag also counts if it is marked as weak, as `W/"…"`, which a proxy does when it compresses the answer.

HTTP has `304 Not Modified` for `GET` and `HEAD` requests, and for `QUERY`, which it treats like `GET`, also when the query asks with a body. Since `Query` accepts only `QUERY`, a query whose answer has not changed is always answered with `304 Not Modified`.

Every other answer that the kit writes carries no revision, and says `Cache-Control: no-store`, so that no cache keeps it. That holds for the answer to a command, for every failure, and for the answer of a query without `Revisioned`, or of one whose view has not seen any event yet. Without a word on caching, HTTP would let a cache keep such an answer for a while it picks itself, and hand it out again.

The `ETag` holds the query that was asked, with every field. So two callers get the same `ETag` only if their queries are equal: a query that holds the user, or anything else that tells callers apart, gets an `ETag` of its own for each of them. That matters as soon as callers share a browser one after the other, since the browser asks with the `ETag` it kept for the one before. The query is built before the route waits or tells the caller that nothing has changed, so a caller who may not ask is refused first.

This holds as long as the answer depends on nothing but the query and the view, which is why the answer sees neither the request nor the user. Three things get past it:

- The clock, or anything else outside the events. See [Depending on More Than the Read Model](#depending-on-more-than-the-read-model).
- Another view. The revision is that of the view handed over, so an answer that also reads from another view does not notice when that one changes.
- The context. A value that a middleware puts into the context, such as the user, never shows up in the `ETag`. Put it into the query instead.

For an answer that depends on more than the query and `Varying` can capture, wait without a tag (see [Waiting for a Revision Without a Tag](#waiting-for-a-revision-without-a-tag)).

A query that holds a function or a channel can not be written into an `ETag`, and its answer goes without one.

*Note that the constants `httpapi.HeaderWaitFor` and `httpapi.HeaderRevision` contain the names of the two headers.*

*Note that `Revisioned` panics for a `nil` view or a negative wait, and so does giving it twice, or along with `Awaiting`. A `nil` pointer counts as a `nil` view, such as a view that was declared but never created with `NewInMemoryView`.*

#### Depending on More Than the Read Model

If an answer depends on more than the view, for example on the current date, the simplest way is to put that value into the query, as `Today` below. Since the query is part of the `ETag`, the `ETag` changes with it, and `Revisioned` is all it takes:

```go
type ListOverdueBooks struct {
  Today string
}

func listOverdueBooks(catalog architecturekit.View[BookItem]) func(context.Context, ListOverdueBooks) ([]BookItem, error) {
  return func(ctx context.Context, q ListOverdueBooks) ([]BookItem, error) {
    return query.Collect(query.Where(catalog.All(ctx), func(item BookItem) bool {
      return item.IsBorrowed && item.BorrowedUntil < q.Today
    }))
  }
}

httpapi.Query(api, mux, "QUERY /api/overdue-books",
  func(r *http.Request, _ httpapi.NoBody, user User) (ListOverdueBooks, error) {
    return ListOverdueBooks{Today: time.Now().Format(time.DateOnly)}, nil
  },
  answerBooks(listOverdueBooks(catalog)),
  httpapi.Revisioned(catalog, httpapi.DefaultWait),
)
```

If the answer takes such a value from elsewhere instead, for example because it reads the clock itself, hand over the `Varying` option as well, with a function of the type `httpapi.Volatile`. It receives the request and returns a value that changes whenever the answer would, and that becomes part of the `ETag` as well:

```go
func today(*http.Request) string {
  return time.Now().Format(time.DateOnly)
}

httpapi.Query(api, mux, "QUERY /api/books-due-today", toListBooksDueToday, answerBooks(listBooksDueToday(catalog)),
  httpapi.Revisioned(catalog, httpapi.DefaultWait),
  httpapi.Varying(today),
)
```

Here, `listBooksDueToday` answers like `listOverdueBooks`, but takes the current day from `time.Now` itself, rather than from its query, and `toListBooksDueToday` has the request type `httpapi.NoBody` and returns that query.

*Note that such a value, whether it is part of the query or comes from `Varying`, holds the time only as precisely as the answer depends on it, such as the day or the hour, never as an instant. An instant, such as `time.Now()` itself, differs on every request, and so does every `ETag`, so the answer is never `304 Not Modified`.*

*Note that `Varying` panics for `nil`, and so does giving it twice, or without `Revisioned`.*

#### Waiting for a Revision Without a Tag

Some answers depend on more than the query and `Varying` can capture, for example on the instant, on the configuration, or on another view. A tag would then tell a caller that nothing has changed when it has. To let the caller read its own writes all the same, hand over the `Awaiting` option instead of `Revisioned`. It waits exactly as `Revisioned` does, but tags nothing: the answer carries neither a `Revision` header nor an `ETag`, says `Cache-Control: no-store`, as every answer without a revision does, and is never `304 Not Modified`. As with `Revisioned`, the query is built before the route waits, so a body that does not fit is refused at once.

For example, a preview of borrowing a book tells whether the book is available, which the caller wants to see right after returning it, and until when the book would be borrowed, which depends on the current time and on the loan period, which comes from the configuration:

```go
type PreviewLoan struct {
  BookID string
}

type loanPreviewBody struct {
  IsAvailable   bool   `json:"isAvailable"`
  BorrowedUntil string `json:"borrowedUntil"`
}

func previewLoan(catalog architecturekit.KeyedView[string, BookItem], loanPeriod time.Duration) func(context.Context, PreviewLoan) (loanPreviewBody, error) {
  return func(ctx context.Context, q PreviewLoan) (loanPreviewBody, error) {
    book, isFound, err := catalog.Get(ctx, q.BookID)
    if err != nil {
      return loanPreviewBody{}, err
    }
    if !isFound {
      return loanPreviewBody{}, fmt.Errorf("%w: book %s", httpapi.ErrNotFound, q.BookID)
    }

    return loanPreviewBody{
      IsAvailable:   !book.IsBorrowed,
      BorrowedUntil: time.Now().Add(loanPeriod).Format(time.DateOnly),
    }, nil
  }
}

httpapi.Query(api, mux, "QUERY /api/books/{id}/loan-preview", toPreviewLoan, previewLoan(catalog, loanPeriod),
  httpapi.Awaiting(catalog, httpapi.DefaultWait),
)
```

Here, `toPreviewLoan` has the request type `httpapi.NoBody`, and takes the ID of the book from the path.

So use `Revisioned` while the answer depends on nothing but the query and the view, plus what `Varying` adds, since the `ETag` then saves sending an answer that has not changed. Use `Awaiting` for an answer that depends on more, and neither for a query whose caller never needs to read its own writes.

*Note that `Awaiting` panics for a `nil` view or a negative wait, and so does giving it twice, or along with `Revisioned`. A `nil` pointer counts as a `nil` view, such as a view that was declared but never created with `NewInMemoryView`. Since only a query with `Revisioned` has a tag, `Varying` panics along with `Awaiting` as well.*

#### Waiting for a Revision in a Handler of Your Own

To read its own writes in a handler of your own, for example one that answers in another format than JSON, call the `Await` function with the request, the view, and how long to wait at most. It waits for the revision the request asks for, within the context of the request. Running out of time is not an error, and neither is the end of the context of the request, for example because the caller went away: in both cases, `Await` stops waiting and returns `nil`, so the handler answers with what the view holds. It returns an error if the header holds something that is not a revision, which wraps `httpapi.ErrMalformed` as well as `architecturekit.ErrNotARevision`, or if waiting fails for another reason. Determine the caller first, so that nobody can make the server wait without being allowed to ask:

```go
mux.HandleFunc("GET /api/books.csv", func(w http.ResponseWriter, r *http.Request) {
  if _, err := httpapi.UserOf(r, api); err != nil {
    httpapi.RespondError(w, r, api, err)
    return
  }

  if err := httpapi.Await(r, catalog, httpapi.DefaultWait); err != nil {
    httpapi.RespondError(w, r, api, err)
    return
  }

  // ...
})
```

*Note that such a handler answers with neither an `ETag` nor `304 Not Modified`. Those come with the `Revisioned` option of `Query`, which ties the `ETag` to the query, so that callers never share one by accident. For a query that answers in the default format, the `Awaiting` option of `Query` waits the same way (see [Waiting for a Revision Without a Tag](#waiting-for-a-revision-without-a-tag)).*

### Checking Health over HTTP

An orchestrator such as Kubernetes regularly asks an application whether it can serve requests, and whether it is alive. To answer both by the state of the projections, hand the runs started with `StartProjection` over to the `Readiness` and `Liveness` functions, and serve the handlers they return on paths of your choice. They list each projection by the name it was given with `Named`:

```go
run := architecturekit.StartProjection(ctx, store, architecturekit.SubjectTree("/books"), catalogProjection,
  architecturekit.Named("catalog"),
)

mux.Handle("GET /ready", httpapi.Readiness(run))
mux.Handle("GET /live", httpapi.Liveness(run))
```

*Note that a `nil` run, a run without a name, or two runs of the same name, make `Readiness` and `Liveness` panic.*

Both answer with `200 OK` or `503 Service Unavailable`, depending on where the projections stand:

| Projection | `Readiness` | `Liveness` |
| --- | --- | --- |
| catches up for the first time | `503` | `200` |
| can not reach the database at the start | `503` | `200` |
| is live | `200` | `200` |
| reconnects after it has caught up | `200` | `200` |
| has stopped | `503` | `503` |

The application is ready once every projection has caught up, since a half-built view answers wrongly. A projection that reconnects later on, for example because the database restarts, keeps it ready: its view is behind, but consistent, and every instance shares the database, so taking them all out would answer nothing instead of something that is behind. A projection that has stopped makes the application neither ready nor alive, since its view never changes again. The orchestrator then restarts the application, which builds the view anew, with a configuration that may have been fixed in the meantime. There is no time limit for reconnecting, since a restart does not bring the database back.

The body of `Readiness` tells whether the application is ready, and where each projection stands:

```json
{
  "isReady": true,
  "projections": {
    "catalog": {
      "phase": "live",
      "since": "2026-09-30T12:00:00Z",
      "hasCaughtUp": true,
      "attempts": 0,
      "revision": "42"
    }
  }
}
```

The body of `Liveness` has the same shape, but tells whether the application is alive, with `isAlive` instead of `isReady`.

*Note that the body does not tell why a projection reconnects or has stopped, since health checks are usually reachable without signing in, and the reason may name internal addresses. Log it instead, for example by waiting for `Done` and calling `Err`.*

### Putting It Together

So far, the pieces have been shown one at a time. A `main` function wires them together: it creates the store, registers the schemas, starts the projection and waits until it has caught up, serves the routes and the health checks, and shuts down cleanly once it is asked to stop:

```go
func main() {
  ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
  defer stop()

  baseURL, err := url.Parse("http://localhost:3000")
  if err != nil {
    log.Fatal(err)
  }

  client, err := eventsourcingdb.NewClient(baseURL, "secret")
  if err != nil {
    log.Fatal(err)
  }

  store := architecturekit.NewStore(client, "https://library.eventsourcingdb.io")

  if err := architecturekit.RegisterSchemas(ctx, store, bookState.Schemas()); err != nil {
    log.Fatal(err)
  }

  // The projection stops only after the server, since the requests that the
  // server finishes may still wait for the view.
  projectionCtx, stopProjection := context.WithCancel(context.Background())
  defer stopProjection()

  catalog := newCatalog()
  run := architecturekit.StartProjection(projectionCtx, store, architecturekit.SubjectTree("/books"),
    architecturekit.Tracking(newCatalogProjection(catalog), catalog),
    architecturekit.Named("catalog"),
  )

  waitCtx, cancelWait := context.WithTimeout(ctx, time.Minute)
  defer cancelWait()

  if err := architecturekit.WaitCaughtUp(waitCtx, run); err != nil {
    if ctx.Err() != nil {
      // The process was asked to stop while waiting.
      return
    }

    log.Fatalf("the catalog has not caught up: %v", err)
  }

  api := httpapi.NewAPI(store, userFrom)
  mux := http.NewServeMux()

  httpapi.Route(api, mux, "POST /api/books/{id}/borrow", toBorrowBook, borrowBook)
  httpapi.Query(api, mux, "QUERY /api/books", toListBooks, answerBooks(listBooks(catalog)),
    httpapi.Revisioned(catalog, httpapi.DefaultWait),
  )
  mux.Handle("GET /ready", httpapi.Readiness(run))
  mux.Handle("GET /live", httpapi.Liveness(run))

  server := &http.Server{Addr: ":8080", Handler: mux}
  go func() {
    if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
      log.Fatal(err)
    }
  }()

  <-ctx.Done()

  shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
  defer cancel()

  if err := server.Shutdown(shutdownCtx); err != nil {
    log.Println(err)
  }

  stopProjection()
  <-run.Done()

  if err := run.Err(); err != nil {
    log.Println(err)
  }
}
```

`signal.NotifyContext` ends `ctx` once the process is asked to stop, as an orchestrator does with `SIGTERM`. `Shutdown` then lets the server finish the requests it has begun, which may still wait for the view (see [Reading Your Own Writes over HTTP](#reading-your-own-writes-over-http)). That is why the projection runs with a context of its own, which `main` cancels only after the server has stopped. Then it waits for `Done`, and logs the error of the run, which is `nil` unless the run had ended on a failure before.

A database that can not be reached at the start makes `RegisterSchemas` fail right away. The server starts only once the view has caught up, so that no query sees a half-built view. `main` waits for that with a context that ends after a minute, or once the process is asked to stop, which ends the waiting, but not the projection, since that runs with a context of its own. If catching up takes longer than a minute, for example because the database has stopped answering in the meantime, `main` gives up rather than wait without end, so that the orchestrator restarts the application. Choose the limit with room for the history to grow, since a view that never catches up within it keeps the application from ever starting. Until the server starts, the health checks do not answer either, so give the application that long to start, for example with a startup probe in Kubernetes.

Set up this way, everything lives as long as `main` does. If you move the setup of the projection into a function of its own, that function returns long before the application ends, so it must not cancel the context of the projection with `defer cancel()`, which would stop the projection right away. Have it return the `cancel` function along with the run instead, and call it on shutdown, as `main` calls `stopProjection`.

### Testing Deciders

To test deciders without a database, use the `architecturekittest` package:

```go
import "github.com/thenativeweb/architecturekit-golang/architecturekit/architecturekittest"
```

Call the `Given` function with a `*testing.T`, the decider, and the events that have happened so far. Then call the `When` function with the command, and check the decision:

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
      BookID:        "42",
      ReaderID:      "23",
      BorrowedUntil: "2026-10-24",
    }).
    ThenEvents(BookBorrowed{
      BorrowedBy:    "23",
      BorrowedUntil: "2026-10-24",
    })
}
```

`Given` returns a `*Fixture`, and `When` returns a `*Decision`, which is the decider's decision on the command: events, nothing, or a refusal. The functions that check the decision return the decision again, so they can be chained.

Like `Execute`, `When` refuses an event that is `nil`, a `nil` pointer included, an event that the state of the decider has no rule for, and an event whose data can not be encoded as JSON, for example because it holds a float `NaN` (see [Executing Commands](#executing-commands)). As `Execute` does, it checks all events for `nil` first, then all of them for a rule, and encodes them last. It also checks the preconditions of the command before the decider decides, so that a command `Execute` refuses, such as one that combines `Unconditionally` with others, is refused here as well (see [Using Preconditions](#using-preconditions)). The decision then holds the same error of the category `ErrPermanent` that `Execute` returns, so `ThenEvents` and the other functions that expect events, or nothing, fail and name the cause, and `ThenFailed(architecturekit.ErrPermanent)` matches. Only an event ID of `OnEventID` that is not a revision is refused with the error that wraps `ErrNotARevision` instead, as `Execute` does, which `ThenFailed(architecturekit.ErrNotARevision)` matches.

*Note that `Given` takes a `testing.TB`, as does every function of the package that can fail a test, so it works with the `*testing.B` of a benchmark as well.*

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

*Note that `ThenSomeEvent`, `ThenEveryEvent`, and `ThenNoEvent` fail the test for `nil` as the function, whatever the decision is, and say so. Otherwise, `ThenNoEvent` would pass for a decision without events, although it checks nothing.*

#### Expecting Rejections

To expect that a command is rejected, call the `ThenFailed` function with the error you expect. It matches with `errors.Is`, so it takes the error the decider returns, as well as every error that this error wraps. For example, `acquireBook` wraps `ErrBookAlreadyAcquired` to add the ID of the book (see [Making Decisions](#making-decisions)), and `ThenFailed` still finds it:

```go
architecturekittest.Given(t, acquireBook, BookAcquired{}).
  When(AcquireBook{BookID: "42"}).
  ThenFailed(ErrBookAlreadyAcquired)
```

To expect any error of a category instead, hand over the category, such as `architecturekit.ErrDomain` or `architecturekit.ErrPermanent`. For example, `borrowBook` rejects a book that does not exist with an error that it creates in place with `NewDomainError`, which belongs to the category `ErrDomain`:

```go
architecturekittest.Given(t, borrowBook).
  When(BorrowBook{BookID: "42"}).
  ThenFailed(architecturekit.ErrDomain)
```

*Note that `ThenFailed` does not compare messages, so rewording a rejection breaks no test. To tell a rejection apart from the others of the same category, create it once, as a variable, as `ErrBookAlreadyAcquired` is.*

#### Expecting Preconditions

To expect exactly the given preconditions, in the given order, call the `ThenPreconditions` function. Describe the preconditions with the functions of the same names, `OnStateRead`, `Unconditionally`, `OnPristineSubject`, `OnPopulatedSubject`, and `OnEventID`, and one made with `Require` from an EventQL query with the `OnQuery` function. For example, the `BorrowBook` that checks the revision of the caller requires its subject to be on the event ID the command carries (see [Checking the Revision of the Caller](#checking-the-revision-of-the-caller)):

```go
architecturekittest.Given(t, borrowBook, BookAcquired{}).
  When(BorrowBook{BookID: "42", ReaderID: "23", ExpectedEventID: "0"}).
  ThenPreconditions(architecturekittest.OnEventID("/books/42", "0"))
```

`ThenPreconditions` describes a precondition by what the database checks, so `OnPristineSubject`, `OnPopulatedSubject`, and `OnEventID` also describe a precondition made with `Require` from the function of the client SDK that does the same, such as `NewIsSubjectPristinePrecondition`. A test that expects a pristine subject fails for a command that declares a populated one, and the failure names both. For example, `AcquireBook` requires a pristine subject (see [Preventing Duplicates](#preventing-duplicates)):

```go
architecturekittest.Given(t, acquireBook).
  When(AcquireBook{BookID: "42"}).
  ThenPreconditions(architecturekittest.OnPristineSubject("/books/42"))
```

For a pristine or a populated subject, the two are alike. For a revision, they are not: only `OnEventID` checks the event ID before anything is read, and refuses one that is not an event ID with an error that wraps `ErrNotARevision`, while `Require` with `NewIsSubjectOnEventIDPrecondition` leaves it to the database, which refuses it as a malformed request, so that `Execute` fails with an error of the category `ErrPermanent` (see [Checking the Revision of the Caller](#checking-the-revision-of-the-caller)). `ThenPreconditions` matches both, so a test tells them apart by what they do. For example, the following test passes for the `BorrowBook` that checks the revision of the caller with `OnEventID`, and fails for one that uses `Require` instead, since the test fixture then leaves the ID to the database, as `Execute` does, and the decider decides on the command:

```go
architecturekittest.Given(t, borrowBook, BookAcquired{}).
  When(BorrowBook{BookID: "42", ReaderID: "23", ExpectedEventID: "abc"}).
  ThenFailed(architecturekit.ErrNotARevision)
```

To get the preconditions of a command directly, call the `PreconditionsOf` function. It returns a slice of `Precondition`, with the fields `Subject`, `Pristine`, `Populated`, `EventID`, `Query`, `OnStateRead`, and `Unconditional`:

```go
preconditions := architecturekittest.PreconditionsOf(ReturnBook{BookID: "42"})
```

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

*Note that `ThenState` fails the test for `nil` as the function, and says so.*

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
  ThenFailed(architecturekit.ErrDomain)
```

The upcaster turns the older event into a `BookBorrowed` event, so the book is borrowed already, and `borrowBook` rejects the command.

#### Replaying Events Directly

To evolve a state from events without a decider, call the `Replay` function with the state and typed events, or the `ReplayStored` function with the state and stored events. To check the state after each event, advance it one event at a time with the `Step` function (see [Stepping Through States](#stepping-through-states)):

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

The `ExpectItems` function expects the view to hold exactly the given items, in the given order. It compares the items by value, so that items with slices or maps work too.

*Note that `ExpectItems` compares with `reflect.DeepEqual`, so a `nil` slice or map is not equal to an empty one.*

To get the items as a slice instead, call the `ItemsOf` function:

```go
items := architecturekittest.ItemsOf(t, catalog)
```

Both fail the test if the view fails while it is read, also after some of the items.

If a projection reads the time of an event, or the events of several subjects have to follow one another, call the `StoredEventsAt` function instead of `StoredEvents`. It numbers the events from the given ID on, and times them one minute apart from the given time on:

```go
noon := time.Date(2026, time.March, 2, 12, 0, 0, 0, time.UTC)

events := append(
  architecturekittest.StoredEventsAt("/books/42", 0, noon, BookAcquired{ /* ... */ }),
  architecturekittest.StoredEventsAt("/books/23", 1, noon.Add(time.Minute), BookAcquired{ /* ... */ })...,
)
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

To test the queries of an HTTP API without a database, create the API without a store, with `nil`. It answers queries, and answers a command with `500 Internal Server Error`, and logs that the API has no store:

```go
api := httpapi.NewAPI(nil, userFrom)
```

### Testing with a Database

Some tests need a real database, for example to run commands from end to end. To get one, use the `dbtest` package:

```go
import "github.com/thenativeweb/architecturekit-golang/architecturekit/architecturekittest/dbtest"
```

Call the `Store` function with a `*testing.T`, the source, and the schemas to register. It returns a store on a database that all tests of the package share, which the first test that asks for it starts in a container. Options for the store, such as `WithStateCache`, follow the schemas, so that a test runs with the same store as the application:

```go
func TestAcquireBook(t *testing.T) {
  store := dbtest.Store(t, "https://library.eventsourcingdb.io", bookState.Schemas())

  _, err := architecturekit.Execute(context.TODO(), store, acquireBook, AcquireBook{
    BookID: rand.Text(),
    // ...
  })
  if err != nil {
    t.Fatal(err)
  }
}
```

To stop the database once all tests have run, call the `Main` function from `TestMain`:

```go
func TestMain(m *testing.M) {
  dbtest.Main(m)
}
```

If `TestMain` has more to do once the tests have run, such as closing a browser, run the tests yourself, and call the `StopSharedDatabase` function afterwards. It does nothing if no test has started the database. Here, `closeBrowser` stands for whatever else is left to do:

```go
func TestMain(m *testing.M) {
  code := m.Run()
  closeBrowser()

  if err := dbtest.StopSharedDatabase(); err != nil {
    fmt.Fprintln(os.Stderr, err)
    code = 1
  }

  os.Exit(code)
}
```

*Note that the schemas come as a single slice, so that the options can follow. To hand over the schemas of several states, join them with `slices.Concat`, for example `slices.Concat(bookState.Schemas(), readerState.Schemas())`.*

The tests share the events as well, so a test writes to subjects of its own, for example with a random ID in them, and reads only from those. A test that reads more than that, such as a projection from `/`, needs a database of its own. Call the `IsolatedStore` function to start one for the test alone, which is stopped once the test is over. It takes the same arguments as `Store`, and a few seconds to start, so use it only where the shared one would not do.

For a test that connects by itself, such as one that starts a whole server, call the `SharedDatabase` or the `IsolatedDatabase` function. Each returns a `*Database`, whose `URL` and `APIToken` fields are what a client needs. Its `Client` function returns a client, for example to write an event that no command would, and its `Store` function returns a store, as above. Here, `server.Config` stands for the configuration of your application:

```go
database := dbtest.SharedDatabase(t)

config := server.Config{
  DatabaseURL: database.URL.String(),
  APIToken:    database.APIToken,
}
```

*Note that with `-short` every test that asks for a database is skipped, so that the other tests run without Docker.*

*Note that a `nil` option makes `Store`, `IsolatedStore`, and the `Store` function of a `Database` panic before they ask for a database, so with `-short` as well.*

*Note that the database runs the image `thenativeweb/eventsourcingdb:latest`, as the client SDK starts it, rather than a version of your choice. Docker pulls the image only if it is missing, so which release that is depends on what the machine has pulled before. The database runs without a signing key, so its events carry no signature, and a store with `WithSignatureVerification` fails to read them with an error of the category `ErrUnverified`. Signature verification can therefore not be tested with it (see [Verifying Events](#verifying-events)).*

*Note that `dbtest` is a package of its own because it starts the database with Testcontainers, which brings along the Docker client. A package whose tests only import `architecturekittest`, for example to test deciders and projections, builds without either.*
