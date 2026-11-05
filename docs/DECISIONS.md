# Technical Decision Log

Format: ID, date, decision, rationale, alternatives. Append new decisions at the end.

## D001 Use Go
- Date: 2026-11-04
- Decision: Implement in Go
- Rationale: Widely valued for backend roles; goroutines and channels fit the event-loop model; built-in benchmarking, fuzzing, and pprof
- Alternatives: Rust (faster, but a steeper learning curve); TypeScript (already familiar, but poorly suited to showcasing low-level performance)

## D002 Integer prices and quantities
- Date: 2026-11-04
- Decision: Store prices as int64 in minimum price increments (ticks); quantities are also int64
- Rationale: Floating point has precision errors that break comparisons and accumulation; real trading systems generally use fixed-point integers
- Alternatives: a decimal library (correct but slow)

## D003 Single-threaded matching
- Date: 2026-11-04
- Decision: One goroutine per symbol processes commands serially
- Rationale: Matching is inherently sequential; a lock-free design is faster and simpler, and results can be replayed deterministically
- Alternatives: fine-grained locking (complex and error-prone)

## D004 Red-black tree for ordered price levels
- Date: 2026-11-04
- Decision: Each side's price levels use a hand-written classic red-black tree (CLRS chapter 13, with parent pointers, iterative). The tree is ordered by price priority and caches its leftmost node as the best price, so Best is O(1); the top N levels are walked from the best price via successors in O(N + log P). Both sides share one implementation; direction appears only in the comparison function. Deletion relinks nodes and never copies levels, so a returned level pointer stays valid until that level is deleted
- Rationale: Both structures were compared with the same model tests and benchmarks (Intel Xeon 2.1GHz cloud host, Go 1.24.7, median of 10 runs each). For P = 100 / 1000 / 10000 price levels: removing one level and adding one near the top of the book takes 127 / 183 / 250 ns for the red-black tree vs 159 / 241 / 405 ns for the skip list; consuming the best level 86 / 119 / 144 ns vs 172 / 190 / 227 ns; lookup 6 / 27 / 48 ns vs 27 / 67 / 101 ns. The skip list is faster only when walking the top 20 levels, by 13% to 16% (about 15 ns), and the gap was not significant in interleaved reruns (6 runs each). Per-level memory in the prototypes is 48 bytes for the red-black tree vs 58.7 bytes for the skip list; with the FIFO queue fields added, the production version uses 80 bytes per level. The red-black tree has a guaranteed worst case, with height at most 2·log2(P+1); the skip list is only O(log P) in expectation, and anyone who knows the seed can choose prices that slow it down: in testing, the average lookup steps near the top of the book rose from 18 to 517. The red-black tree uses no randomness, so the same command sequence always produces the same tree, as the replayable matching core requires. The cost is more than twice the code (271 lines vs 114); the symmetric branches are written out in full and keep the CLRS case numbering so they can be checked against the textbook. Buy-side ordering uses a single comparison, `a^flip < b^flip`. An if-based version made lookup 93% slower at 100 levels and 11% to 15% slower at 1000 and 10000 levels, and made consuming the best level and adding/removing near the top of the book 6% to 16% slower or within noise (`bench-results/d004/results/flip-vs-if.txt`), so the comparison is kept and covered by boundary-value tests. Data and the three prototypes are in `bench-results/d004/`
- Alternatives: skip list (shorter code, but writes are 26% to 101% slower, no worst-case guarantee, and a seed to manage); left-leaning red-black tree (deletion copies the successor's key and value, invalidating returned level pointers); sorted slice (14 to 25 times slower to add or remove levels at P = 10000; kept only as a baseline)

## D005 golangci-lint for all static analysis
- Date: 2026-11-04
- Decision: Use golangci-lint v2 (pinned to v2.5.0) for all static analysis, with staticcheck enabled as one of its linters instead of installed separately; the configuration is in `.golangci.yml`
- Rationale: One tool, one configuration, the same version locally and in CI, with no staticcheck rules lost. depguard also forbids non-test code in `internal/book` from importing `time`, `math/rand`, `crypto/rand`, `net`, and `os`, so CI enforces the rule that the matching core does not depend on time, randomness, or networking
- Alternatives: installing staticcheck separately (one more tool version to maintain, and no import restrictions)

## D006 Core types hold only fixed-size integers; events are appended to a caller-owned buffer
- Date: 2026-11-04
- Decision: Command (40 bytes), Event (64 bytes), Order (64 bytes), Trade, and LevelUpdate consist only of fixed-size integer fields and can be compared with `==`. All event kinds share one Event struct, distinguished by Kind; field usage is documented in the Event comments. The M1 interface is `Book.Apply(cmd Command, out []Event) []Event`, called as `buf = b.Apply(cmd, buf[:0])`; the next call overwrites buf, so copy it before handing it to another goroutine. `contract_test.go` pins the sizes of these types and checks that their fields are integers only
- Rationale: Once the buffer has grown, emitting events no longer allocates, and the GC does not scan the event slice. Preliminary benchmark (Xeon 2.1GHz): appending 3 events takes about 27 ns with 0 allocations. Table-driven and replay tests can compare whole event sequences with `slices.Equal`
- Alternatives: one struct per event kind wrapped in an interface (one allocation per event, and type switches everywhere); callbacks (no allocation, but every test needs a recorder)

## D007 The engine fills OrderID and Timestamp into the Command before logging
- Date: 2026-11-04
- Decision: Clients do not send an OrderID. When the API receives a new order, it first takes the next ID from the engine's allocator (one counter shared by all symbols), then calls Validate; IDs taken by commands rejected with 400 are simply skipped, since IDs need only be unique, not contiguous. 0 is invalid; the book can only detect IDs that duplicate a currently resting order. Timestamp is an int64 supplied by the caller (the engine uses Unix nanoseconds); the book copies it unchanged and never compares it. Time priority is the order in which commands are applied. symbol and client_id are not part of the Command; the engine and API layers handle them. Each ID also carries its symbol's index (D019)
- Rationale: Each symbol's log is self-contained, so replay does not depend on the clock or on the book's internal counters; `DELETE /v1/orders/{id}` carries no symbol, so IDs must be unique across symbols; the clock moving backwards does not affect matching
- Alternatives: book-assigned IDs (collide across symbols); ordering by timestamp (priority would depend on clock quality)

## D008 Enum zero values are invalid; numeric values are explicit and pinned by tests
- Date: 2026-11-04
- Decision: The zero values of Side, OrderType, TIF, CommandKind, and EventKind are invalid; the zero value of Reason is ReasonNone. All constants have explicit numbers (e.g. `Buy Side = 1`), pinned by `TestWireValuesArePinned`; new values are only appended, never renumbered. Each enum has one name table shared by its String and Parse functions. American spelling is used throughout (canceled)
- Rationale: An unset field or a missing JSON field is rejected rather than silently becoming buy or limit. These numbers are written to the command log, so reordering constants would break old logs; the test catches that
- Alternatives: iota starting at 0 (an unset field equals a valid value); decoding JSON directly with UnmarshalText (error codes for misspellings would need a second mapping)

## D009 Command.Validate is the single stateless validation
- Date: 2026-11-04
- Decision: `Validate()` returns a Reason (ReasonNone when valid) and runs its checks in a fixed order. Market orders must be IOC or FOK with Price 0; limit orders must have Price in [1, MaxPrice]; cancels may carry only OrderID and Timestamp. The API uses it to return 400 directly, and commands that fail it are neither logged nor applied; the book checks every command again. The API fills in Kind and OrderID for new orders itself, so invalid_command or invalid_order_id on a new order indicates an API bug and should return 500
- Rationale: Both layers share one function, so their conclusions cannot diverge; the fixed order makes the reason deterministic when several checks fail, keeping replay results stable; each intent has exactly one valid representation and unused fields must be 0, so future features such as order amendment can use those fields without affecting old logs
- Alternatives: returning error (allocates, and callers need type assertions); ignoring TIF and Price on market orders (non-canonical log)

## D010 FOK orders that cannot fill completely are rejected outright
- Date: 2026-11-04
- Decision: FOK first sums the fillable quantity across the opposite side's levels; if it is not enough, it emits only `Rejected(fok_unfillable)`, with no Accepted and no Trade, and the book is unchanged. The remainder of IOC and market orders is Accepted ... `Canceled(ioc_remainder)`
- Rationale: SPEC section 6, rule 4 says "if it is not enough, rejects the whole order without producing any trades", which is more specific than section 3's "fill completely or cancel"; this keeps "Rejected means the command changed nothing" always true, and the quantity conservation check needs no special case
- Alternatives: Accepted plus `Canceled(fok_unfillable)` (contradicts the wording of section 6)

## D011 Event sequences, level totals, and Seq
- Date: 2026-11-04
- Decision: A new order's event sequence is Rejected, or Accepted, zero or more Trades, then Rested or Canceled; a cancel produces Rejected or Canceled; a command of unknown kind produces `Rejected(invalid_command)`. Quantity conservation holds after the last event of each command. Events that change resting orders (Trade, Rested, and a Canceled that removes a resting order) carry the side of the affected level and its absolute total quantity after the event. Seq starts at 1 in each book and increases by 1 per event, rejections included; a Trade's Seq is its trade sequence number. Each WebSocket channel uses its own contiguous counter kept by the engine
- Rationale: Quantity conservation can be checked from the event stream alone; only the book can obtain level totals cheaply, and with absolute values the engine can produce book_delta by keeping the last value per (side, price), without maintaining a second order book; a channel sees only some events, so using Seq directly would leave clients unable to tell a real gap from filtered events
- Alternatives: a separate BookDelta event (more events, and one Qty field name with two meanings); events without level information (the engine would have to maintain a mirror of the book)

## D012 Price and Qty are named int64-based types, both capped at 1e9
- Date: 2026-11-04
- Decision: `type Price int64`, `type Qty int64`, `type OrderID uint64`. Validate requires prices of at most 1e9 ticks and quantities of at most 1e9 lots
- Rationale: These are still the int64 integers of D002, and the compiler can also prevent a price from being used as a quantity. A single trade's notional value is at most 1e18, below the int64 maximum of about 9.2e18; the total quantity of 9 million maximum-size resting orders is still below 2^53, an exact integer in JavaScript clients. `TestBoundsCannotOverflow` turns these arguments into a test. Raising the caps later does not affect old logs; lowering them would
- Alternatives: bare int64 (swapping price and quantity variables still compiles); no caps (fuzzing would quickly overflow level totals)

## D013 The book stores only resting orders; Order records RestedSeq
- Date: 2026-11-04
- Decision: The book stores only resting orders (currently only limit GTC); filled or canceled orders are removed immediately, and history and status are maintained by the engine from events. Order records RestedSeq, the Seq of the Rested event that gave it its current queue position
- Rationale: Within each price level, RestedSeq strictly increases from head to tail, so CheckInvariants can use it to check time priority; a future amendment that reduces quantity keeps it while a price change assigns a new one, which makes "keeps priority" testable; snapshot recovery can also preserve queue order
- Alternatives: the book keeps finished orders (unbounded memory growth)

## D014 Impossible internal states are reported with panic
- Date: 2026-11-04
- Decision: `internal/book` panics in only one case: a state that no valid or invalid command can reach and that only a bug in the book itself can cause, such as inserting a price that already exists into the price-level tree. Panic messages start with `book:`. Every problem that input can trigger (including arbitrary commands constructed by fuzzing) results in a `Rejected` event and never a panic
- Rationale: Once such a state occurs, the order book can no longer be trusted, and continuing would only carry the error into later trades and logs; stopping immediately makes it easiest to locate. Fuzz and property tests report a panic as a failure, so bugs are not silently swallowed
- Alternatives: returning error (callers cannot handle it sensibly and would only pass it up); ignoring it silently (the error spreads into later state and is harder to track down)

## D015 Correctness checks: CheckInvariants returns error, CI fuzzes on every push
- Date: 2026-11-05
- Decision: `Book.CheckInvariants()` returns the first problem found, or nil if there is none; it runs in O(N + P) and is called only from tests, fuzzing, and debug mode. CI has a fuzz job that runs `FuzzBook` and `FuzzCommandValidate` for 30 seconds each on every push; on failure, the failing input is uploaded as a build artifact. Failing inputs found by fuzzing are committed to `internal/book/testdata/fuzz/`, after which a plain `go test` runs them as regression tests
- Rationale: Returning error lets the caller decide whether to fail a test, log, or halt, and the error can name the exact rule that was broken; a panic can only stop the process. Fuzzing on every push catches problems introduced by new changes before they are merged; keeping failing inputs in the repository means fixed bugs cannot quietly return
- Alternatives: CheckInvariants panics directly (the caller has no choice); fuzzing only manually on a local machine (easy to forget, and problems are found late)

## D016 One command log per symbol
- Date: 2026-11-05
- Decision: Each symbol's goroutine owns one append-only file, `<DataDir>/<symbol>.log`. The header holds a format version, the symbol and its index; each record holds the command's fixed fields and client_id, prefixed by its length and followed by a CRC-32C. On startup a torn last record, or a zero-filled tail, is truncated; damage anywhere else stops the engine. A new log is created by writing its header to a temporary file, syncing it and renaming it into place. D027 covers Windows, and D028 keeps a second engine out of the directory
- Rationale: No lock or shared writer between symbols, and each book replays independently. The checksum tells a crash-truncated tail, which is safe to drop, from real corruption, which is not
- Alternatives: one shared log (needs a lock or a writer goroutine); no checksum (silent corruption)

## D017 Log durability: write before apply, fsync every 10 ms
- Date: 2026-11-05
- Decision: Each batch of commands is written to the OS in one write before any of them is applied, and the book applies exactly the commands whose records were written in full. The log is fsynced every `SyncInterval` (10 ms by default) and on shutdown; a negative interval fsyncs after every batch. After a failed write or fsync the symbol accepts no more commands (`ErrLogFailed`). D031 runs the interval fsyncs beside the matching goroutine, which adds an fsync in progress to the bound below; D029 reports a failure
- Rationale: A process crash loses nothing; a machine crash loses at most the last interval. An fsync per batch costs milliseconds on cloud disks and would endanger the 5 ms p99 target
- Alternatives: fsync every batch (slow); no fsync (unbounded loss on power failure)

## D018 Slow subscribers are disconnected
- Date: 2026-11-05
- Decision: Each subscriber has a bounded channel. Matching never waits on it: a full channel ends the subscription with `ErrSlowConsumer`. A subscription records the Seq it starts from, so a client can resynchronize from a snapshot. D026 adds bounds on a connection's unsent bytes and on the time to write one message
- Rationale: One slow consumer must not stall a book or other consumers; a gap the client can detect is better than silently dropped events
- Alternatives: blocking (stalls matching); dropping updates but keeping the subscriber (gaps go unnoticed)

## D019 Order IDs carry their symbol's index
- Date: 2026-11-05
- Decision: `id = counter<<8 | symbolIndex`, from one engine-wide counter. An engine serves at most 256 symbols, and IDs stay exact in JavaScript for 2^45 - 1 orders. The symbol order is fixed by configuration and checked against each log header
- Rationale: `DELETE` and `GET /v1/orders/{id}` are routed from the ID alone, with no lookup table and no lock on the hot path
- Alternatives: an ID-to-symbol map (shared state updated on every fill and cancel); requiring the symbol in every request (not in SPEC §8)

## D020 Commands wait for their result and fail fast when a queue is full
- Date: 2026-11-05
- Decision: `PlaceOrder` and `CancelOrder` return the command's events once it is applied. If the symbol's queue is full they return `ErrBusy` at once. The context bounds only the submission: a queued command is always applied, so its result is always returned. Queries and subscriptions wait for room, bounded by their context
- Rationale: The REST API can report fills in its response, and backpressure reaches the client immediately instead of building latency (SPEC §4)
- Alternatives: fire-and-forget commands (the client learns the outcome only from the stream); blocking until there is room (hides overload)

## D021 Order IDs are reserved durably in blocks
- Date: 2026-11-05
- Decision: The ID counter is reserved 65,536 values at a time in `<DataDir>/ids`, written to a temporary file, synced and renamed, before any ID of the block is issued. On startup the counter continues from the larger of the reservation and the highest ID in the logs
- Rationale: A machine crash can lose the last unsynced log records, and with them the highest IDs the logs show. Without a reservation those IDs could be issued again, and a client holding an old ID could cancel someone else's order. A restart wastes at most one block
- Alternatives: fsync before acknowledging every ID (slow); a random epoch per restart (shrinks the ID space and still needs persistence)

## D022 Finished orders are kept in a bounded history
- Date: 2026-11-05
- Decision: Each symbol's goroutine tracks every order's status from the events. Resting orders are always known; the 100,000 most recently finished orders per symbol (`Config.History`) are kept in a ring buffer. Replay rebuilds both, client_id included
- Rationale: `GET /v1/orders/{id}` can report how an order ended, and a cancel of a finished order can be told apart from an unknown one, with bounded memory
- Alternatives: resting orders only (an order disappears the moment it fills); unbounded history (memory grows forever)

## D023 Two dependencies for the network layer
- Date: 2026-11-05
- Decision: `github.com/coder/websocket` v1.8.15 for WebSocket and `github.com/prometheus/client_golang` v1.23.2 for metrics, pinned in go.mod. v1.23.2 is the newest client_golang release that supports Go 1.24. `internal/book` stays standard library only. D028 later uses `golang.org/x/sys`, already in the module graph through client_golang
- Rationale: The standard library has no WebSocket server, and the exposition format and histogram semantics are easy to get subtly wrong by hand
- Alternatives: hand-written WebSocket framing and metrics (more code, more protocol risk)

## D024 A full queue is answered with 503
- Date: 2026-11-05
- Decision: `ErrBusy` maps to `503 Service Unavailable` with `Retry-After: 1`
- Rationale: The server is overloaded; the client did nothing wrong, which 429 would imply
- Alternatives: 429 Too Many Requests

## D025 A command that took effect is reported even if its log failed
- Date: 2026-11-05
- Decision: When writing or syncing a batch's log records fails after the batch was applied (`-sync-interval` below 0), the REST reply is `500 log_failed` with the order and events. Metrics count the command by what it did
- Rationale: The order is live and may trade, and replay restores it if the write reached the disk. Without its ID the client could neither query nor cancel it
- Alternatives: a plain 500 (the client wrongly concludes nothing happened); a 200 (hides that durability is unknown)

## D026 WebSocket snapshots hold the whole book; the send backlog is bounded in bytes
- Date: 2026-11-05
- Decision: `book_snapshot` holds every level, so snapshot plus deltas always reproduces the book. Each connection's unsent messages are limited to 1 MB beyond the one being queued, and one message unsent for 10 s also marks a slow consumer (close 1008)
- Rationale: With a capped snapshot, deltas for levels below the cap left the client's book incomplete. A byte bound keeps the memory a non-reading client can pin small even when snapshots are large
- Alternatives: a capped snapshot with deltas filtered to the visible depth (the server tracks each client's window); a message-count bound only (a backlog of snapshots costs megabytes)

## D027 The engine runs on Windows without directory fsync
- Date: 2026-11-05
- Decision: On Windows, syncing a directory is skipped, and the command log is opened without O_APPEND (on every platform) so a torn tail can be truncated. CI runs the tests on Windows
- Rationale: Windows opens a directory read-only and cannot flush it, so the engine could not start. The log has one writer, which writes at the end after replay. A machine crash on Windows may lose a log or ID reservation just created or renamed; the guarantees of D016 and D021 hold fully on Linux and macOS. Pebble and RocksDB make the same trade
- Alternatives: support only Linux and macOS (the engine would not run on a Windows laptop)

## D028 One engine per data directory
- Date: 2026-11-05
- Decision: `New` takes an exclusive lock on `<DataDir>/LOCK` before reading or writing anything there, and `Close` releases it after the logs are closed; a second engine gets `ErrDataDirInUse`. The lock is flock on Linux, macOS, the BSDs and illumos, and LockFileEx on Windows (via `golang.org/x/sys`, already a dependency of client_golang); other platforms run unlocked. The file is never removed, and a closed engine no longer reserves IDs
- Rationale: Two engines on one directory interleave their writes and reserve the same IDs, so a replay builds a book neither had, and a second start can truncate records the first had applied as a torn tail. flock conflicts between two opens in one process too, so a test can check it. Kafka (a lock file per log directory), etcd (its WAL files) and bbolt (its database file) keep a second instance out the same way
- Alternatives: fcntl locks (do not conflict within one process); no lock (relies on the operator)

## D029 A failed command log is reported; the process keeps running
- Date: 2026-11-05
- Decision: The first failed write or fsync of a symbol's log is logged at error level, sets `matching_engine_log_failed{symbol}` to 1, and makes `/healthz` answer `200 {"status":"degraded","log_failed":[...]}`. The symbol refuses commands until a restart (D017); the process does not exit
- Rationale: Before, the failure showed only in client replies. Kafka likewise takes one failed log directory offline, keeps serving the others and exports the count. A 503 would make a load balancer drop the healthy symbols too, Docker does not restart an unhealthy container, and a restart would not fix a full or failing disk
- Alternatives: exit the process, as etcd and PostgreSQL do (stops every symbol); 503 from /healthz

## D030 End-to-end latency is measured open-loop from each request's due time
- Date: 2026-11-05
- Decision: `cmd/loadgen` sends the SPEC §9 flow (`internal/flow`, shared with the core benchmarks) on a fixed schedule, request i due at t0 + i/rate, over a fixed pool of keep-alive connections (64 by default), whatever the replies. Latency counts from the due time: to the first trade push on the WebSocket for an order that traded, and to the REST reply for every request; service time and send lag are kept apart, and a 1 ms sleep loop records loadgen's own pauses (after jHiccup). Percentiles are exact, from sorted samples. A run starts from an empty data directory, discards a 10 s warmup and measures 30 s; it is valid only if 99% of the target rate was sent in the window, no request failed and every trade was pushed. The sweep starts at 10k/s, runs each rate three times and stops at the first invalid run
- Rationale: A closed-loop generator waits for replies, so a stall delays only the requests in flight, and the generator quietly sends fewer: p99 looks met while the rate falls short (wrk2; exchange-core's latency tests; Schroeder et al., NSDI 2006). The engine stamps `ts` after a command leaves its queue, so client time minus `ts` would leave out the queue wait. 409 and 404 replies to cancels are expected outcomes of the flow, not errors. More connections than the load needs only add work on both sides: at 15k/s, 512 connections gave a p99 of 11.2 ms against 7.26 ms with 64 (bench-results/m5-e2e/conns-512, conns-64)
- Alternatives: closed-loop workers (understate latency); HdrHistogram (a dependency; a run's samples sort in well under a second); RecordCorrectedValue (suits recording at a fixed interval only)

## D031 Interval fsyncs run beside the matching goroutine
- Date: 2026-11-05
- Decision: With `SyncInterval` above 0, each symbol's fsyncs run on a helper goroutine woken through a one-slot channel, so the symbol's goroutine goes on with later batches meanwhile; it takes up a failed fsync before its next batch and then accepts no more commands, as in D017. Close waits for the helper. With a negative interval every batch is still fsynced before its replies. This amends D017's bound: a machine crash may lose up to one interval of commands plus those taken while an fsync was in progress
- Rationale: An inline fsync held up every command of the symbol for its whole duration: here about 0.8 ms every 10 ms, and tens of milliseconds at times. Redis (`appendfsync everysec`), Kafka (its log flusher) and PostgreSQL (its WAL writer) also flush on a background thread. Results in docs/BENCHMARKS.md
- Alternatives: fsync inline (D017); wait for an fsync still running at the next interval, as Redis does for up to 2 s (bounds the window, brings the stall back)

## D032 A distroless image with a built-in health check
- Date: 2026-11-05
- Decision: The image builds the engine and loadgen with `CGO_ENABLED=0` in `golang:1.24.7` and runs them on `gcr.io/distroless/static-debian13:nonroot`, both pinned by digest. The engine has a `healthcheck` subcommand that GETs `/healthz`, which compose uses. Compose keeps the data in a named volume, publishes the port on 127.0.0.1 only, and allows 20 s to stop, more than the engine's 10 s shutdown timeout. loadgen runs from the same image under the `load` profile
- Rationale: The image holds two static binaries and no shell, about 30 MB. With no curl in it, the binary checks itself, as Traefik's `healthcheck` command does. A named volume is owned by the image's user and avoids slow bind mounts from a Windows file system. The API has no authentication, so it is not published beyond the host
- Alternatives: an Alpine base with curl (a shell and more packages to patch); a bind-mounted data directory (permissions and speed depend on the host)

## D033 The engine serves an order book page
- Date: 2026-11-05
- Decision: `GET /` serves one static page, embedded in the binary, with no framework or build step. It keeps a local book from `book:SYMBOL` and the tape from `trades:SYMBOL`, applies messages as they arrive and repaints at most 10 times a second, writing only cells whose text changed, into rows made once. A sequence gap or a closed connection leads to a reconnect and a fresh snapshot, with backoff up to 10 s
- Rationale: The book holds absolute totals, so any number of deltas between paints collapse into one state, and a paint touches a bounded number of cells at any message rate. One recovery path covers gaps, slow-consumer closes and restarts. dYdX, Serum and OpenDAX lay out their books the same way: asks above, bids below, cumulative depth bars
- Alternatives: canvas rendering (scaling, theming and accessibility to handle by hand); a framework and a build step (tooling a single page does not need)
