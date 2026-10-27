# Yodelr (Go)

A Go implementation of the **Yodelr** micro-message service coding
challenge, ported from a Python solution of the same design — columnar
in-memory storage measured for tens of millions of posts on a single
machine. See [SOLUTION.md](SOLUTION.md) for the design, the behaviour
decisions, and the measured Python-vs-Go memory comparison.

## The service

Yodelr is an in-process, single-threaded, fully in-memory service:

* Users register by name and may be deleted again; deleting a user also
  deletes all their posts.
* A registered user writes posts of up to 140 characters, carrying zero
  or more topics: `#` followed by one or more characters from
  `[0-9a-zA-Z_]` (the topic name excludes the `#`).
* Post timestamps are caller-supplied seconds since an arbitrary epoch,
  unique and strictly increasing across posts.
* Queries: all posts by a user (newest first), all posts mentioning a
  topic (newest first), and the trending topics within an inclusive
  time interval (by descending mention count, ties alphabetical, where
  a topic mentioned several times in one post counts once).

The challenge's ground rules: standard library only (testing libraries
excepted), no disk, no network, no databases, a single calling thread,
and the provided service interface may be extended but not altered.

## Layout

```
yodelr/yodelr.go       the service interface
yodelr/service.go      the implementation: Service
yodelr/service_test.go unit + randomized reference tests
cli/                   interactive command-line front end + tests
cmd/yodelr/            CLI entry point
cmd/benchmark/         scale/memory measurement harness
```

## Running

Requires Go 1.22+ (developed on 1.26). From the repository root:

```
go test ./...                             # run the test suite
go run ./cmd/yodelr                       # explore interactively
go run ./cmd/benchmark                    # measure at 1,000,000 posts
go run ./cmd/benchmark -posts 10000000
```

### Trying it by hand

`go run ./cmd/yodelr` starts an interactive session
(`go run ./cmd/yodelr < commands.txt` runs a script; `demo` replays the
challenge's worked example):

```
yodelr> add_user kevin
ok: user 'kevin' registered
yodelr> add_post kevin hello #world
ok: post stored at timestamp 1
yodelr> trending 1 1
  1. world
```

Post text runs to the end of the line (no quoting needed). The
timestamp is optional and defaults to the previous one + 1; pass an
integer (or `auto`) as the second argument to set it explicitly — which
is also how to write a post whose text begins with a bare number.
`stats` prints live totals (users, posts, distinct topics), backed by
small read-only extensions on the service (`UserCount`, `PostCount`,
`TopicCount`).
