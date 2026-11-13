# architecturekit

Building blocks for DDD-based applications with CQRS and event sourcing in Go, on top of [EventSourcingDB](https://www.eventfoundation.io/products/eventsourcingdb) – a purpose-built database for event sourcing.

ArchitectureKit covers both sides of an event-sourced application: commands, events, and the state to decide on for writing, and projections, views, and queries for reading. An optional package exposes commands and queries over HTTP.

The complete documentation of ArchitectureKit, including preconditions, views, projections, queries, handling errors, testing, HTTP, and upgrading, is available at **[https://www.eventfoundation.io/docs/architecturekit/go](https://www.eventfoundation.io/docs/architecturekit/go)**.

## Getting Started

ArchitectureKit needs Go 1.27.1 or later, and a running EventSourcingDB. To start one on your machine, see the [quickstart of EventSourcingDB](https://www.eventfoundation.io/docs/eventsourcingdb/quickstart).

Install the packages:

```shell
go get github.com/thenativeweb/architecturekit-golang/v2/architecturekit github.com/thenativeweb/eventsourcingdb-client-golang/v2/eventsourcingdb
```

Create an EventSourcingDB client, and a store with the client and the source to use for all events you write:

```go
import (
  "context"
  "fmt"
  "net/url"

  "github.com/thenativeweb/architecturekit-golang/v2/architecturekit"
  "github.com/thenativeweb/eventsourcingdb-client-golang/v2/eventsourcingdb"
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

Define a command, which describes what someone wants to do:

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
```

Define an event, which describes what has happened:

```go
type BookAcquired struct {
  Title  string `json:"title"`
  Author string `json:"author"`
  ISBN   string `json:"isbn"`
}

func (BookAcquired) EventType() string {
  return "io.eventsourcingdb.library.book-acquired"
}
```

Define the state that a command is decided on, and how events change it:

```go
type Book struct {
  IsAcquired bool
}

var bookState = architecturekit.NewState(Book{}).
  Evolve(func(book Book, event BookAcquired) Book {
    book.IsAcquired = true
    return book
  })
```

Connect the state with the decision made on it:

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

Execute the command:

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

For everything else, see the **[complete documentation](https://www.eventfoundation.io/docs/architecturekit/go)**.
