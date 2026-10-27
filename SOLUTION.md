# Yodelr — Solution Notes (Go)

Go implementation of the Yodelr service interface, ported from a Python
solution of identical design. Standard library only, single-threaded,
fully in memory, designed and measured for 1–50+ million posts. The
bottom of this document holds the measured Python-vs-Go comparison on
the same machine and workload.

## Layout

```
yodelr/yodelr.go       the service interface
yodelr/service.go      the implementation: Service
yodelr/service_test.go unit + randomized reference tests
cli/cli.go             interactive command-line front end
cli/cli_test.go        tests for the CLI adapter
cmd/yodelr/main.go     CLI entry point
cmd/benchmark/main.go  scale/memory measurement harness
```

## Installation and running

1. Have Go 1.22 or newer available (developed on 1.26).
2. Clone the repository and `cd` into its root.

That is the entire installation: no third-party dependencies. From the
repository root:

```
go test ./...                            # run the test suite
go run ./cmd/yodelr                      # explore interactively
go run ./cmd/benchmark                   # measure at 1,000,000 posts
go run ./cmd/benchmark -posts 10000000 -users 1000000 -vocab 500000
```

The CLI behaves exactly like the Python one (commands, output format,
`auto` timestamps, `stats`, `demo`); see the README for a sample
session. The Python solution's `loadgen.py` (a Faker-based bulk-load
dev tool, explicitly not part of that solution) was not ported; the
benchmark harness covers bulk loading.

## Design

The binding constraint at tens of millions of posts is memory. The two
decisions carried over from the Python solution follow from it:

**1. Columnar storage — a post is an integer id, not an object.**
Everything about post *p* lives at index *p* in parallel slices. Even
the texts are not kept as individual `string` values (16-byte header
per post, one heap allocation each rounded up to the allocator's size
class, and a pointer the GC must scan forever): they are packed
back-to-back into shared 1 MiB byte buffers and addressed by
offset/length slices — ~10 B/post plus the raw bytes, and zero pointers
for the collector to trace — then copied out to a `string` only when a
query returns them.

**2. Post-id order == timestamp order.** The spec guarantees `AddPost`
timestamps are unique and monotonically increasing. Ids are assigned
densely in insertion order, so every index is already time-sorted — no
sorted containers, no comparator, binary search for any time window.
The implementation enforces the invariant with one integer comparison,
since every index silently depends on it.

| structure | type | purpose |
|---|---|---|
| `textChunks` + `textLoc` + `textLen` | `[][]byte` chunks + `[]uint64`/`[]uint16` | packed UTF-8 texts; loc/len address each post's bytes |
| `timestamps` | `[]int64` | post id → timestamp, sorted by construction |
| `mentionFlat` + `mentionOff` | `[]int32` + `[]int64` | per-post topic ids in CSR form: post *p*'s topics are `flat[off[p]:off[p+1]]` |
| `topicIDs` / `topicNames` | `map[string]int32` / `[]string` | topic string interning (topic id = int) |
| `topicPosts` | `[]int32` per topic | ascending **live** post ids mentioning the topic (inverted index) |
| `userPosts` | `map[string][]int32` | ascending post ids written by each user |

Hashmaps appear exactly where string lookup is required — one entry per
user and per distinct topic. Nothing per post or per mention lives in a
hashmap: `[]int32`/`[]int64` elements cost their raw 4/8 bytes.

### How each call works

* **AddPost** — validate (user exists, ≤140 code points, timestamp
  increases; no mutation before validation passes), append the text
  bytes to the blob, parse topics with a byte scanner equivalent to the
  regex `#([0-9a-zA-Z_]+)`, dedupe within the post, intern each topic
  (cloning the name so the interning table never pins a whole post
  text), append the post id to the topic's posting list and the topic
  ids to the mention log. Amortized O(text length + topics).
* **GetPostsForUser / GetPostsForTopic** — walk the user's/topic's id
  slice backwards (ids ascend in time) and copy texts out. O(result).
* **GetTrendingTopics(from, to)** — two binary searches on `timestamps`
  give the contiguous post-id window `[lo, hi)`. Because posts enter
  the mention log in id order, the window's mentions are one contiguous
  span of `mentionFlat`, counted in a single pass over the span (no
  batching needed — unlike Python's `Counter.update`, the loop makes no
  transient copies), then sorted by (−count, name). O(log n + mentions
  in window + u·log u) for u unique topics in the window. Texts are
  never touched.
* **DeleteUser** — for each of the user's posts: zero its text bytes in
  place (nothing readable remains), mark its location dead, and
  overwrite its mention-log entries with the sentinel `-1` (trending
  skips these with one branch). Each *affected* topic's posting list is
  rebuilt once without the deleted ids and right-sized when mostly
  slack; a topic left with no live posts is retired outright — its
  interned name is freed (completing the privacy intent of deletion)
  and its id recycled for the next new topic, which is safe because an
  empty posting list proves no live mention references the id. Both
  shared logs self-compact: when dead text bytes exceed live ones, one
  pass rebuilds the text blob (releasing old chunks progressively, so
  the transient peak is live bytes + one chunk, not old + new), and
  when tombstones outnumber live mentions, one pass rebuilds the
  mention log — so reclamation is amortized O(1) per delete and a
  trending scan touches at most twice the live mentions in its window,
  however many posts have ever been deleted. Deletions are the rare
  operation; the price is paid here so reads stay (nearly) filter-free.

### Differences from the Python original

The design, algorithms, API behaviour and test suite are the same;
these are the deliberate ports-of-idiom and fixes:

1. **Errors, not exceptions.** Methods return `error` where Python
   raised `ValueError`, with the same messages. Pure queries
   (`GetPostsForTopic`, `GetTrendingTopics`) still cannot fail.
2. **No timestamp range check.** Python stored timestamps in an
   int64-wide array but took arbitrary-precision ints, so it validated
   the range; here the parameter *is* `int64`, making out-of-range
   values unrepresentable. The type boundaries themselves are storable
   (tested).
3. **No `surrogatepass` machinery.** Go strings are arbitrary bytes;
   storage is byte-exact by construction, so any string — valid UTF-8
   or not — round-trips. "140 characters" means Unicode code points
   (`utf8.RuneCountInString`); in the invalid-UTF-8 corner Python
   counted a lone surrogate as 1 character while Go counts each invalid
   byte as 1.
4. **Scanner instead of regex.** Topic parsing is a hand-rolled byte
   scanner over exactly the spec's charset — same results as the
   compiled regex, no allocation. The randomized reference model in the
   tests still uses `regexp`, so the two implementations cross-check
   each other.
5. **Chunk-boundary fix.** The Python original has a latent bug: a
   zero-length text stored while the write offset sits exactly at the
   1 MiB chunk boundary gets a location whose offset overflows into the
   chunk-index bits, and reading it panics until a later post happens
   to create the next chunk. `storeBytes` here starts a fresh chunk in
   that case; a regression test constructs the exact boundary.
6. **CLI `add_post` timestamp token** that is numeric but exceeds int64
   is reported as an error (Python parsed it as a big int and let the
   service's range check reject it) rather than silently treated as
   post text.
7. **Mention-log compaction.** The Python original scans deletion
   tombstones in the queried window forever (its notes name amortized
   compaction as the fix if deletions mattered); here the mention log
   rebuilds itself once tombstones outnumber live mentions, bounding
   every trending scan to at most 2× the live mentions in its window.
8. **Topic retirement.** Python interns topic names forever (a
   documented limit there, and a privacy wrinkle: the name of a
   deleted user's hashtag outlived the deletion); here a topic whose
   last live post dies is un-interned and its id recycled.
9. **Posting lists are right-sized after deletion.** A straight port
   (`slices.DeleteFunc`) would keep each rebuilt list's full old
   backing array — Python rebuilt fresh arrays, so this Go-specific
   trap is closed by cloning once a list is mostly slack.
10. **Text compaction releases old chunks progressively** (live
    locations ascend with post id, so finished chunks free as the pass
    advances). Python holds the whole old blob until the pass ends,
    briefly costing ~1.5× the blob at the exact moment memory is
    already tight.

### Why not …

* **A `Post` struct + maps of slices** — a per-post struct with a
  `string` text, a `[]string` of topics and map entries costs several
  hundred bytes and several pointers per post; at 50M posts that is
  both the memory budget and a GC burden, for no functional gain.
* **`[]string` for texts** (the natural Go choice) — 16 B of header
  per post, one rounded-up heap allocation each, and 1 pointer per post
  for every GC mark phase to trace. Packed chunks cost ~10 B/post
  addressing, zero pointers, and the trade is copy-on-read, only on
  APIs that return texts.
* **Re-parsing texts on demand for trending** (storing no per-post
  topics) — makes a full-history trending query re-scan every post.
  The CSR mention log costs ~12 B/post and keeps counting at slice
  speed.
* **Per-topic binary search for trending** — O(all topics ever) per
  query, so a narrow window over a large vocabulary does far more work
  than scanning the window itself. The chosen approach scales with the
  window, which matches the "what is hot right now" usage pattern.
* **A time-bucketed / segment-tree counter structure** — O(n log n)
  memory of counter maps; ruled out by the memory budget.
* **Compressing the text blob** — documented in the Python solution as
  the next lever (~2–3× on realistic short posts) if the target were
  ~200M+ posts on small-RAM hardware; same here, and the OS already
  compresses cold pages transparently under pressure.

## Behaviour decisions the spec leaves open

Identical to the Python solution (all covered by tests):

1. A descriptive `error` for: unknown user in `AddPost` / `DeleteUser`
   / `GetPostsForUser`; duplicate name in `AddUser` (existing posts are
   untouched); post text over 140 characters; a timestamp not strictly
   greater than the previous post's. A failed `AddPost` leaves no
   partial state.
2. Pure queries never fail: `GetPostsForTopic` of an unknown topic and
   `GetTrendingTopics` of an empty or inverted interval return nothing.
3. Topics and user names are case-sensitive. The topic charset is
   exactly ASCII `[0-9a-zA-Z_]` per the spec: `"#café"` contributes the
   topic `caf`.
4. "140 characters" means Unicode code points. Any byte sequence
   round-trips exactly.
5. Mention count is per post: a topic repeated inside one post counts
   once, and the post appears once in `GetPostsForTopic`.
6. A deleted user's name may register again as a fresh, empty user.
7. Internal post ids are int32: ~2.1 billion posts over a service
   lifetime, failing loudly (an explicit error, before any mutation)
   rather than wrapping.

## Testing

The Python suite is ported in full:

* **Spec tests** — the challenge's worked example verbatim, plus
  per-method behaviour: ordering, inclusive interval bounds,
  tie-breaking, 140-char limit, deletion side effects,
  re-registration, parsing edge cases (`#a#b`, `##x`, lone `#`, `#_`,
  non-ASCII boundary, case sensitivity, within-post duplicates).
* **Storage tests** — byte-exact round-trips for multibyte/astral and
  invalid-UTF-8 texts (including the 560-byte worst case), a mass-
  delete test asserting compaction runs and survivors stay byte-exact,
  and the new chunk-boundary regression test.
* **Randomized cross-checks** — two seeded runs (600 mixed ops; 800
  delete-heavy ops that trigger compactions mid-run) drive both the
  service and a deliberately naive brute-force model (which parses with
  `regexp`, independently of the service's scanner); every user, topic
  and 40+ trending windows must agree exactly.

## Measured results: Go vs Python

Same machine (Apple Silicon, 8 GB RAM), same synthetic workload
(Zipf-distributed topic popularity, ~1.8 topics/post average, 60–120
char distinct texts, users = posts/10, vocab = posts/20), measured
2026-10-27 with CPython 3.14.4 and Go 1.26.2. Reproduce with
`python3 benchmark.py` (Python repo) and `go run ./cmd/benchmark`.

### Memory

| metric | Python 1M | Go 1M | Python 10M | Go 10M |
|---|---|---|---|---|
| peak RSS delta after build | 154 MiB (162 B/post) | 202 MiB (212 B/post) | 600 MiB † (63 B/post †) | 1,463 MiB (153 B/post) |
| peak RSS delta, `GOGC=25` | — | 157 MiB (164 B/post) | — | 1,597 MiB (167 B/post) |
| **live data after build** | **162 B/post** ‡ | **125 B/post** | **~162 B/post** † | **133 B/post** |

† The Python 10M RSS figure is an artifact: macOS transparently
compresses cold pages under memory pressure on the 8 GB test machine
(the Python solution's own notes document this and put the honest
uncompressed cost at the 1M figure, ~162 B/post, texts dominating). The
Go 10M numbers are also measured under the same pressure, so if
anything they flatter Python's side of the RSS rows.

‡ CPython frees garbage eagerly by reference counting, so its RSS delta
*is* its live data. Go reports live heap after a forced GC
(`runtime.MemStats.HeapAlloc`); its RSS additionally carries collector
headroom — tunable with `GOGC`/`GOMEMLIMIT`, as the `GOGC=25` row shows
(peak RSS down from 212 to 164 B/post for a ~7% build-throughput cost;
steady-state RSS settles near live data once building stops).

**How much more data fits in the same RAM:** by live data cost per post
— the number that determines steady-state capacity — Go stores the same
workload in **125–133 B/post vs Python's ~162 B/post**, i.e. **~1.25×
more posts in the same memory** (1.30× at the 1M scale, 1.22× at 10M).
On a 16 GB machine budgeting ~12 GiB for the process, that is roughly
**~97M posts for Go vs ~80M for Python**. The gain is deliberately
modest: the Python solution already stores everything in packed
arrays/bytes precisely to avoid per-object overhead, so Go's win comes
only from the smaller residual overheads (dict/map and string headers,
list-of-chunks pointers, allocator slack) — against a naive
object-per-post design in either language the gap would be 3–5×.
Query-result materialization is also cheaper: a returned post costs
~16 B + text bytes in Go vs ~150 B in Python, so a 2M-post topic query
transiently allocates ~190 MiB instead of ~300 MiB.

### Speed (same runs, best of 3)

| metric | Python 1M | Go 1M | Python 10M | Go 10M | Go gain |
|---|---|---|---|---|---|
| build throughput (incl. text generation) | 346K posts/s | 2.20M posts/s | 280K posts/s | 1.09M posts/s | 3.9–6.3× |
| trending, last 1% of history | 2.5 ms | 0.8 ms | 27 ms | 8.3 ms | 3.3× |
| trending, full history | 88 ms | 19 ms | 1.38 s | 268 ms | 4.5–5.2× |
| get_posts_for_topic, hottest (~20% of posts) | 58 ms | 4.0 ms | 607 ms | 54 ms | 11–14× |
| delete_user (heaviest user) | 50 ms | 14 ms | 425 ms | 47 ms | 3.5–9× |

## Known limits

* Single-threaded by spec; no locking anywhere.
* A deleted post permanently keeps its ~26 bytes of
  id/timestamp/offset slots; its text bytes are zeroed immediately, and
  both the text blob and the mention log reclaim dead space by
  amortized compaction once it outweighs the live data.
* Beyond a few hundred million posts the right move is out of scope by
  the challenge rules anyway: sharding and/or a disk-backed index.
