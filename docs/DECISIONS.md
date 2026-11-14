# Technical Decision Log

Format: ID, date, decision, rationale, alternatives. Append new decisions at the end.

## D001 Use Go
- Date: 2026-11-04
- Decision: Implement in Go
- Rationale: Goroutines and channels fit the event-loop model; built-in benchmarking, fuzzing, and pprof
- Alternatives: Rust (faster, but a steeper learning curve); TypeScript (already familiar, but poorly suited to low-level performance work)

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
- Update: "faster" was not measured; no locked version was built or benchmarked. In the CPU profile after the optimizations, `Book.Apply` is 1.4% to 2% of the engine's CPU (docs/BENCHMARKS.md).

## D004 Red-black tree for ordered price levels
- Date: 2026-11-04
- Decision: Each side's price levels use a hand-written classic red-black tree (CLRS chapter 13, with parent pointers, iterative). The tree is ordered by price priority and caches its leftmost node as the best price, so Best is O(1); the top N levels are walked from the best price via successors in O(N + log P). Both sides share one implementation; direction appears only in the comparison function. Deletion relinks nodes and never copies levels, so a returned level pointer stays valid until that level is deleted
- Rationale: Both structures were compared with the same model tests and benchmarks (Intel Xeon 2.1GHz cloud host, Go 1.24.7, median of 10 runs each). For P = 100 / 1000 / 10000 price levels: removing one level and adding one near the top of the book takes 127 / 183 / 250 ns for the red-black tree vs 159 / 241 / 405 ns for the skip list; consuming the best level 86 / 119 / 144 ns vs 172 / 190 / 227 ns; lookup 6 / 27 / 48 ns vs 27 / 67 / 101 ns. The skip list is faster only when walking the top 20 levels, by 13% to 16% (about 15 ns), and the gap was not significant in interleaved reruns (6 runs each). Per-level memory in the prototypes is 48 bytes for the red-black tree vs 58.7 bytes for the skip list; with the FIFO queue fields added, the production version uses 80 bytes per level. The red-black tree has a guaranteed worst case, with height at most 2·log2(P+1); the skip list is only O(log P) in expectation, and anyone who knows the seed can choose prices that slow it down: in testing, the average lookup steps near the top of the book rose from 18 to 517. The red-black tree uses no randomness, so the same command sequence always produces the same tree, as the replayable matching core requires. The cost is more than twice the code (271 lines vs 114); the symmetric branches are written out in full and keep the CLRS case numbering so they can be checked against the textbook. Buy-side ordering uses a single comparison, `a^flip < b^flip`. An if-based version made lookup 93% slower at 100 levels and 11% to 15% slower at 1000 and 10000 levels, and made consuming the best level and adding/removing near the top of the book 6% to 16% slower or within noise (`bench-results/d004/results/flip-vs-if.txt`), so the comparison is kept and covered by boundary-value tests. Data and the three prototypes are in `bench-results/d004/`
- Alternatives: skip list (shorter code, but writes are 26% to 101% slower, no worst-case guarantee, and a seed to manage); left-leaning red-black tree (deletion copies the successor's key and value, invalidating returned level pointers); sorted slice (14 to 25 times slower to add or remove levels at P = 10000; kept only as a baseline)

## D005 golangci-lint for all static analysis
- Date: 2026-11-04
- Decision: Use golangci-lint v2 (pinned to v2.15.0; v2.5.0 until 2026-11-05) for all static analysis, with staticcheck enabled as one of its linters instead of installed separately; the configuration is in `.golangci.yml`
- Rationale: One tool, one configuration, the same version locally and in CI, with no staticcheck rules lost. depguard also forbids non-test code in `internal/book` from importing `time`, `math/rand`, `crypto/rand`, `net`, and `os`, so CI enforces the rule that the matching core does not depend on time, randomness, or networking
- Alternatives: installing staticcheck separately (one more tool version to maintain, and no import restrictions)

## D006 Core types hold only fixed-size integers; events are appended to a caller-owned buffer
- Date: 2026-11-04
- Decision: Command (40 bytes), Event (64 bytes), Order (64 bytes) and LevelUpdate consist only of fixed-size integer fields and can be compared with `==`. All event kinds share one Event struct, distinguished by Kind; field usage is documented in the Event comments. The M1 interface is `Book.Apply(cmd Command, out []Event) []Event`, called as `buf = b.Apply(cmd, buf[:0])`; the next call overwrites buf, so copy it before handing it to another goroutine. `contract_test.go` pins the sizes of these types and checks that their fields are integers only
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
- Decision: `Validate()` returns a Reason (ReasonNone when valid) and runs its checks in a fixed order. Market orders must be IOC or FOK with Price 0; limit orders must have Price in [1, MaxPrice]; cancels may carry only OrderID and Timestamp. The API uses it to return 400 directly, and commands that fail it are neither logged nor applied; the book checks every command again. The engine fills in Kind and a new order's OrderID itself, and the API checks a cancel's ID before the engine sees it, so invalid_command or invalid_order_id from the engine indicates a server bug and returns 500
- Rationale: Both layers share one function, so their conclusions cannot diverge; the fixed order makes the reason deterministic when several checks fail, keeping replay results stable; each intent has exactly one valid representation and unused fields must be 0, so future features such as order amendment can use those fields without affecting old logs
- Alternatives: returning error (allocates, and callers need type assertions); ignoring TIF and Price on market orders (non-canonical log)

## D010 FOK orders that cannot fill completely are rejected outright
- Date: 2026-11-04
- Decision: FOK first sums the fillable quantity across the opposite side's levels; if it is not enough, it emits only `Rejected(fok_unfillable)`, with no Accepted and no Trade, and the book is unchanged. The remainder of IOC and market orders is Accepted ... `Canceled(ioc_remainder)`
- Rationale: SPEC section 6, rule 4 says "if it is not enough, rejects the whole order without producing any trades", which is more specific than section 3's "fill completely or cancel"; this keeps "Rejected means the command changed nothing" always true, and the quantity conservation check needs no special case
- Alternatives: Accepted plus `Canceled(fok_unfillable)` (contradicts the wording of section 6)

## D011 Event sequences, level totals, and Seq
- Date: 2026-11-04
- Decision: A new order's event sequence is Rejected, or Accepted, zero or more Trades, then Rested or Canceled; a cancel produces Rejected or Canceled; a command of unknown kind produces `Rejected(invalid_command)`. Quantity conservation holds after the last event of each command. Events that change resting orders (Trade, Rested, and a Canceled that removes a resting order) carry the side of the affected level and its absolute total quantity after the event, and its order count since D035. Seq starts at 1 in each book and increases by 1 per event, rejections included; a Trade's Seq is its trade sequence number. Each WebSocket channel uses its own contiguous counter, kept by the channel's pump in internal/api
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
- Decision: `Book.CheckInvariants()` returns the first problem found, or nil if there is none; it runs in O(N + P) and is called only from tests and fuzzing. CI has a fuzz job that runs `FuzzBook`, `FuzzCommandValidate` and `FuzzCheckKeys` for 30 seconds each on every push; on failure, the failing input is uploaded as a build artifact. Failing inputs found by fuzzing are committed to the package's `testdata/fuzz/`, after which a plain `go test` runs them as regression tests
- Rationale: Returning error lets the caller decide whether to fail a test, log, or halt, and the error can name the exact rule that was broken; a panic can only stop the process. Fuzzing on every push catches problems introduced by new changes before they are merged; keeping failing inputs in the repository means fixed bugs cannot quietly return
- Alternatives: CheckInvariants panics directly (the caller has no choice); fuzzing only manually on a local machine (easy to forget, and problems are found late)

## D016 One command log per symbol
- Date: 2026-11-05
- Decision: Each symbol's goroutine owns one append-only file, `<DataDir>/<symbol>.log`. The header holds a format version, the symbol and its index; each record holds the command's fixed fields and client_id, prefixed by its length and followed by a CRC-32C. On startup a torn last record, or a zero-filled tail, is truncated; damage anywhere else stops the engine. A new log is created by writing its header to a temporary file, syncing it and renaming it into place. An empty log file therefore counts as damage and stops the engine: a log appears only through that rename, with its header already synced, appends only grow it, and a torn tail is never cut shorter than the header. D027 covers Windows, and D028 keeps a second engine out of the directory
- Rationale: No lock or shared writer between symbols, and each book replays independently. The checksum tells a crash-truncated tail, which is safe to drop, from real corruption, which is not
- Alternatives: one shared log (needs a lock or a writer goroutine); no checksum (silent corruption)
- Update 2026-11-13: a record whose length ran to or past the end of the file used to count as torn without a check, so a damaged length near the end dropped that record and every whole record after it. It is now damage when the bytes after its length field begin with a whole record, checksum included, under the length its client_id length gives: a crash cuts a record short but leaves its length field as written. Still truncated as a torn tail, since the checksum cannot tell them from a crash: damage to the last record's payload or checksum, a length damaged together with the payload or checksum under it, and damage that zeroes the file from some point to its end

## D017 Log durability: write before apply, fsync every 10 ms
- Date: 2026-11-05
- Decision: Each batch of commands is written to the OS in one write before any of them is applied, and the book applies exactly the commands whose records were written in full. The log is fsynced every `SyncInterval` (10 ms by default) and on shutdown; a negative interval fsyncs after every batch. After a failed write or fsync the symbol accepts no more commands (`ErrLogFailed`). D031 runs the interval fsyncs beside the matching goroutine, which adds an fsync in progress to the bound below; D029 reports a failure
- Rationale: A process crash loses no acknowledged command (`TestKillUnderLoad` in cmd/engine kills the engine under load and checks this); a machine crash loses at most the last interval, which no test covers. An fsync per batch costs milliseconds on cloud disks and would endanger the 5 ms p99 target
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
- Update 2026-11-13: the HTTP server's write timeout (5 s in cmd/engine) used to run from the end of reading the request, so a command that waited longer in its queue was applied but its reply was cut off. The order and cancel handlers now restart it once the result is ready, so the reply gets the whole timeout and a client that reads slowly is still bounded

## D021 Order IDs are reserved durably in blocks
- Date: 2026-11-05
- Decision: The ID counter is reserved 65,536 values at a time in `<DataDir>/ids`, written to a temporary file, synced and renamed, before any ID of the block is issued. On startup the counter continues from the larger of the reservation and the highest ID in the logs. D027 covers Windows
- Rationale: A machine crash can lose the last unsynced log records, and with them the highest IDs the logs show. Without a reservation those IDs could be issued again, and a client holding an old ID could cancel someone else's order. A restart wastes at most one block
- Alternatives: fsync before acknowledging every ID (slow); a random epoch per restart (shrinks the ID space and still needs persistence)

## D022 Finished orders are kept in a bounded history
- Date: 2026-11-05
- Decision: Each symbol's goroutine tracks every order's status from the events. Resting orders are always known; the 100,000 most recently finished orders per symbol (`Config.History`) are kept in a ring buffer. Replay rebuilds both, client_id included
- Rationale: `GET /v1/orders/{id}` can report how an order ended, and a cancel of a finished order can be told apart from an unknown one, with bounded memory
- Alternatives: resting orders only (an order disappears the moment it fills); unbounded history (memory grows forever)

## D023 Two dependencies for the network layer
- Date: 2026-11-05
- Decision: `github.com/coder/websocket` v1.8.15 for WebSocket and `github.com/prometheus/client_golang` v1.23.2 for metrics, pinned in go.mod. v1.23.2 is the newest client_golang release that supports Go 1.24; D037 moves the project to Go 1.27.3 and keeps these pins. `internal/book` stays standard library only. D028 later uses `golang.org/x/sys`, already in the module graph through client_golang
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
- Decision: `book_snapshot` holds every level, so snapshot plus deltas always reproduces the book; deltas carry order counts since D035. Each connection's unsent messages, apart from the largest, are limited to 1 MB, and taking over 10 s to write one message also marks a slow consumer (close 1008)
- Rationale: With a capped snapshot, deltas for levels below the cap left the client's book incomplete. A byte bound keeps the memory a non-reading client can pin small even when snapshots are large
- Alternatives: a capped snapshot with deltas filtered to the visible depth (the server tracks each client's window); a message-count bound only (a backlog of snapshots costs megabytes)
- Update 2026-11-13: the bound used to exempt only a message queued when nothing else was: a client that did not read held at most 1 MB, or that one message, in the queue, plus the message being written. A client that read in time was disconnected when a message near or over 1 MB waited behind others and any message followed it, for example a large `book_snapshot` or `book_delta` beside a busy trades channel. The bound now leaves out the largest queued message, so a client that does not read holds at most 1 MB plus its largest queued message, and one more message in the writer, which the write timeout bounds. Two messages over 1 MB queued at once still close the connection. The 10 s timeout runs from the start of writing a message; time spent in the 256-message queue is not timed
- Update 2026-11-13: a client that reads in time can still be disconnected as `slow_consumer_updates` just after subscribing to a deep book under a high command rate. The snapshot is taken on the symbol's goroutine; while the pump converts, encodes and queues it, updates collect in the subscription, which holds 1024, and it is dropped if they fill it first. Checked once by hand in a 4-CPU container, with commands sent through `Engine.PlaceOrder` at a fixed rate, one update each: with 200,000 levels at 50,000/s the 7.09 MB snapshot arrived 118 ms after subscribing and the client was disconnected at 120 ms; with 50,000 levels at 50,000/s, and with 200,000 levels at 5,000/s, it stayed connected for 30 s

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
- Update 2026-11-13: a failed extension of the order-ID reservation (D021) is reported alike: logged at error level when it starts failing, `matching_engine_order_ids_failed` set to 1, and `/healthz` answering `200 {"status":"degraded","order_ids_failed":true}`. Meanwhile every new order fails with 500 on every symbol, while cancels and queries are served. Each new order tries the reservation again; the first that succeeds clears the state, logged at info level, with no restart

## D030 End-to-end latency is measured open-loop from each request's due time
- Date: 2026-11-05
- Decision: `cmd/loadgen` sends the SPEC §9 flow (`internal/flow`, shared with the core benchmarks) on a fixed schedule, request i due at t0 + i/rate, over a fixed pool of keep-alive connections (64 by default), whatever the replies. Latency counts from the due time: to the first trade push on the WebSocket for an order that traded, and to the REST reply for every request; service time and send lag are kept apart, and a 1 ms sleep loop records loadgen's own pauses (after jHiccup). Percentiles are exact, from sorted samples. A run starts from an empty data directory, discards a 10 s warmup and measures 30 s; it is valid only if 99% of the target rate was sent in the window, no request failed and every trade was pushed once and in order, as D036 checks. The sweep starts at 10k/s, runs each rate three times and stops at the first invalid run
- Rationale: A closed-loop generator waits for replies, so a stall delays only the requests in flight, and the generator quietly sends fewer: p99 looks met while the rate falls short (wrk2; exchange-core's latency tests; Schroeder et al., NSDI 2006). The engine stamps `ts` after a command leaves its queue, so client time minus `ts` would leave out the queue wait. 409 and 404 replies to cancels are expected outcomes of the flow, not errors. More connections than the load needs only add work on both sides: at 15k/s, 512 connections gave a p99 of 11.2 ms against 7.26 ms with 64 (bench-results/m5-e2e/conns-512, conns-64)
- Alternatives: closed-loop workers (understate latency); HdrHistogram (a dependency; a run's samples sort in well under a second); RecordCorrectedValue (suits recording at a fixed interval only)
- Update 2026-11-13: a request that failed during the warmup was not counted, so such a run could pass as valid; it now makes the run invalid. The published runs are not affected: none of the 92 engine metrics files in bench-results, each read at the end of a whole run, counts a busy, failed or invalid order or a busy or failed cancel

## D031 Interval fsyncs run beside the matching goroutine
- Date: 2026-11-05
- Decision: With `SyncInterval` above 0, each symbol's fsyncs run on a helper goroutine woken through a one-slot channel, so the symbol's goroutine goes on with later batches meanwhile; it takes up a failed fsync before its next batch and then accepts no more commands, as in D017. Close waits for the helper. With a negative interval every batch is still fsynced before its replies. This amends D017's bound: a machine crash may lose up to one interval of commands plus those taken while an fsync was in progress
- Rationale: An inline fsync held up every command of the symbol for its whole duration: here about 0.8 ms every 10 ms, and tens of milliseconds at times. Redis (`appendfsync everysec`), Kafka (its log flusher) and PostgreSQL (its WAL writer) also flush on a background thread. Results in docs/BENCHMARKS.md
- Alternatives: fsync inline (D017); wait for an fsync still running at the next interval, as Redis does for up to 2 s (bounds the window, brings the stall back)

## D032 A distroless image with a built-in health check
- Date: 2026-11-05
- Decision: The image builds the engine and loadgen with `CGO_ENABLED=0` in `golang:1.24.7` (`golang:1.27.4`, see D037) and runs them on `gcr.io/distroless/static-debian13:nonroot`, both pinned by digest. The engine has a `healthcheck` subcommand that GETs `/healthz`, which compose uses. Compose keeps the data in a named volume, publishes the port on 127.0.0.1 only, and allows 35 s to stop, more than the engine's worst case: 10 s for HTTP requests, then up to 20 s to close stalled WebSocket connections. loadgen runs from the same image under the `load` profile
- Rationale: The image holds two static binaries and no shell, about 30 MB. With no curl in it, the binary checks itself, as Traefik's `healthcheck` command does. A named volume is owned by the image's user and avoids slow bind mounts from a Windows file system. The API has no authentication, so it is not published beyond the host
- Alternatives: an Alpine base with curl (a shell and more packages to patch); a bind-mounted data directory (permissions and speed depend on the host)

## D033 The engine serves an order book page
- Date: 2026-11-05
- Decision: `GET /` serves one static page, embedded in the binary, with no framework or build step. It keeps a local book from `book:SYMBOL` and the tape from `trades:SYMBOL`, applies messages as they arrive and repaints at most 10 times a second, writing only cells whose text changed, into rows made once. A sequence gap or a closed connection leads to a reconnect and a fresh snapshot, with backoff up to 10 s
- Rationale: The book holds absolute totals, so any number of deltas between paints collapse into one state, and a paint touches a bounded number of cells at any message rate. One recovery path covers gaps, slow-consumer closes and restarts. dYdX, Serum and OpenDAX lay out their books the same way: asks above, bids below, cumulative depth bars
- Alternatives: canvas rendering (scaling, theming and accessibility to handle by hand); a framework and a build step (tooling a single page does not need)

## D034 CI actions are pinned by commit SHA
- Date: 2026-11-05
- Decision: Every action in `.github/workflows/ci.yml` is referenced by the full commit SHA of a release, with the release tag in a comment. Pins move to a new release only by hand. The pinned releases run on Node.js 24, which GitHub's runners now require
- Rationale: A tag can be moved to other code; a SHA cannot. GitHub's security hardening guide and the OpenSSF Scorecard recommend full SHAs, and the Docker images are already pinned by digest (D032)
- Alternatives: major tags such as `@v4` (follow whatever the tag points to)

## D035 Level updates carry the level's order count
- Date: 2026-11-07
- Decision: `book.Event` has `LevelOrders uint32`, the order count of the level it changed, after the event, next to `LevelQty`. REST events carry it as `level_orders` and `book_delta` changes as `orders`, so deltas reproduce the `orders` of snapshots and REST depth. `LevelUpdate` gains `Orders`, and the level's own count becomes a `uint32`
- Rationale: Snapshots and depth had order counts that deltas could not keep up, so snapshot plus deltas did not reproduce the book (D026). Bitfinex's book channel likewise sends the count in snapshots and updates. The field fills the 4 bytes of padding after Event's one-byte fields, so Event stays 64 bytes; BenchmarkApply and BenchmarkApplyDeep show no slowdown (bench-results/level-orders). 2^32 orders on one level would need over 500 GB
- Alternatives: dropping `orders` from snapshots and depth (loses information clients use); a client counting orders from the trade and order events (needs every event of the book, not the L2 channel)

## D036 loadgen checks every trade push
- Date: 2026-11-07
- Decision: loadgen checks the trade stream of the whole run, warmup included. The channel's `seq` must run 1, 2, 3, ... and `book_seq` must rise from push to push. The trade pushes, the trade events in the REST replies and the rise of `matching_engine_trades_total` must agree in number, and the sum of a 64-bit hash of each trade's (book_seq, taker, maker, price, qty) must agree between pushes and replies. Any difference makes the run invalid. The first-push latency is measured as before. The run needs the symbol to itself
- Rationale: Before, loadgen recorded only the first push of each order, so later pushes of the same order could go missing, repeat or come out of order unnoticed. A sum of hashes (an additive multiset hash, Clarke et al., 2003) does not depend on the order in which replies arrive, so each worker keeps its own without a lock, and memory stays constant. The workers read trade events in place, as they read the order ID; loadgen's CPU time at 20k requests/s rose 1.2% and the latency did not change (bench-results/loadgen-check)
- Alternatives: a map from book_seq to trade (hundreds of thousands of entries a run, and a lock between workers and the reader); decoding each reply as JSON (costs loadgen more CPU than the engine spends on the request)

## D037 Build with Go 1.27.3
- Date: 2026-11-07
- Decision: go.mod says `go 1.27.3`, the latest release on go.dev, and the image builds in `golang:1.27.3`, pinned by digest. CI follows go.mod. The loadgen test proxy uses `ReverseProxy.Rewrite`, as `Director` is deprecated since Go 1.26
- Rationale: Go 1.24 leaves support once 1.26 is out, and 1.27.2 and 1.27.3 carry security fixes to net/http and crypto/x509. Raising the go line drops eleven GODEBUG compatibility settings; the ones that apply here turn on container-aware GOMAXPROCS (it follows a cgroup CPU limit; compose sets none, and the benchmark VM has none), stricter colons in URL hosts and pprof labels in tracebacks, and the rest concern TLS and x509, which the engine does not serve. Go 1.26 made the Green Tea GC the default and Go 1.27 runs encoding/json on its v2 implementation; tests, vet, golangci-lint v2.15.0 and the three fuzz targets pass unchanged. BenchmarkApply, BenchmarkApplyDeep and BenchmarkRestingOrders, ten runs each alternating on one idle VM, show no slowdown: one case 7.4% faster (p = 0.029), the rest within noise, and the same allocations and live heap (bench-results/go1.27). The published figures stay those measured with Go 1.24.7
- Alternatives: stay on Go 1.24 (out of support); move only the image (local builds and CI would test another compiler than the one shipped)
- Update: raised to Go 1.27.4 for GO-2026-6859 (net/http MaxBytesReader race that can crash the HTTP/1 server); govulncheck reports it as reachable from internal/api/rest.go.
- Update 2026-11-14: the matching core figures and the comparison with other engines in docs/BENCHMARKS.md, and the README lines drawn from them, were measured with Go 1.27.4 on a second VM (D038, D039, D040); the other published figures stay those measured with Go 1.24.7

## D038 Resting orders indexed by an open-addressing table
- Date: 2026-11-14
- Decision: The book finds a resting order by its ID in a hand-written hash table instead of `map[OrderID]*orderNode`: open addressing with linear probing, Fibonacci hashing, backward-shift deletion, and growth when half full. Growth is gradual: a table twice the size takes new entries, and each insert moves 8 slots of the old one, so no command copies the table. The table does not shrink. ID 0 marks an empty slot; a lookup or delete of 0 finds nothing, and indexing 0 panics (D014). `CheckInvariants` checks the index's own structure
- Rationale: Measured on one VM in two sets of 10 alternating runs (bench-results/core-index). Against the Go map, the index takes 9% to 14% less time per command in `BenchmarkApply` and 12% to 28% less in `BenchmarkApplyDeep`, and holds 129.7 instead of 133.9 bytes per resting order. Gradual growth costs no time per command against copying the whole table (every case within noise). The largest single Apply while 1M orders come to rest, median of 10 runs: Go map 1.02 and 1.01 ms (single runs 0.26 to 18.5 ms), gradual growth 8.59 and 4.53 ms (3.40 to 20.0 ms), whole-table growth 92.0 and 95.8 ms (55.5 to 133.2 ms). The target for this pause was at most 2 ms; it is not met. The pause left is the allocation of the new table in one piece: the insert that starts a growth to 2,097,152 slots takes 0.13 ms on fresh memory and 3.6 to 4.0 ms when the runtime reuses heap memory and must clear 32 MiB first. It happens only when the number of resting orders reaches a new high and the table doubles, and grows with the table. Order IDs are assigned by the engine (D007), so clients cannot choose IDs that collide
- Alternatives: Go's map (slower; it grows in tables of at most 1024 slots, so its pauses stay small); whole-table growth (median pause 92 to 96 ms); extendible hashing with fixed 1024-slot tables under a directory, as Go's map grows (median pause 4.15 ms, single runs 0.75 to 21.4 ms, and 244.8 and 259.6 ns per command against 189.3 and 209.7 ns for whole-table growth with 1M resting orders); allocating the new table in pages on first write (not built: it would add an indirection to every lookup to remove a pause that comes once per doubling)

## D039 Nodes of orders that leave the book are reused
- Date: 2026-11-14
- Decision: The book keeps the node of an order that leaves it, by a fill or a cancel, on a free list linked through the node's own next field, and `rest` takes a node from the list before it allocates one. A node joins the list only after the order's last event has been emitted, and is reset there: zeroed but for the link, so a read after release finds ID 0 and the node keeps no order or level alive. `rest` sets every field again. The list never holds more nodes than the book has held resting orders at once, and that memory stays with the book, as the order index's table does (D038). `CheckInvariants` checks that every free node is reset and that the list ends
- Rationale: Measured on one VM in 10 alternating rounds against the open-addressing index alone (bench-results/core-reuse, benchstat-reuse.txt): time per command falls 17% to 39% in `BenchmarkApply` and `BenchmarkApplyDeep`, and allocations per command from 0.59 and 0.68 to 0.30 and 0.35 on an empty book, and to 0.07 and 0.24 on a deep one, whose passes start with the last pass's nodes free. p99 of a single Apply falls 16% to 25% in the same flows (benchstat-reuse-latency.txt). Resting 1M orders on an empty book frees nothing and costs the same, and the heap per resting order stays 129.7 bytes. On the Go map, reuse alone saves 10% to 30% where the difference is significant (benchstat.txt)
- Alternatives: `sync.Pool` (the runtime may drop pooled nodes at any garbage collection, so the book would allocate them again, and every Get and Put costs a per-P lookup); nodes allocated in blocks (fewer and larger allocations, but a block stays alive while any of its nodes rests, and the free list is still needed); a bound on the list (memory returns after a peak, and the next peak allocates again)

## D040 The flash1-dev benchmark drives the book through a cgo adapter
- Date: 2026-11-14
- Decision: `cmd/harness-adapter` (build tag `harness`) builds the matching core as the shared library the flash1-dev matching engine benchmark loads, pinned at its commit 424e4e5. `internal/harness` maps the benchmark's messages onto `Book.Apply` and the book's events onto its reports; CI tests it, on the benchmark's 34 conformance cases among others. A modify is a cancel and a new GTC order under the same ID, as the benchmark defines it; the book has no amendment of its own (SPEC §3 lists it as optional). Order IDs are resolved by the book's own index (D038), not by a flat array in the adapter as the benchmark's Liquibook adapter does. Messages come in runs (`engine_on_batch`), and reports collect in a buffer that goes to the benchmark's transport in one cgo call once it holds 1,024 and at the end of every call into the library. The comparison runs Liquibook, CppTrader with CppCommon, and geseq/orderbook, each built by the benchmark's own script at its pinned commit; engines without a license file are not run. CppCommon e14011974b8d follows the main branch of {fmt} without pinning a version, so CppTrader is built against Ubuntu 24.04's libfmt-dev 9.1.0+ds1-2. The state audit compares a book with Liquibook's; Liquibook's own audit needs QuantCup, which has no license file, so Liquibook runs perf only. `scripts/harness.sh` builds and runs everything
- Rationale: The benchmark checks every report of every run against a hash that independent engines agree on, so a run whose reports are wrong cannot pass. Crossing cgo dominates if done per message: on normal, ten alternating runs each, one call into Go and one push to C per message ran at a median of 2.22M messages/s, messages in runs with one push per message at 2.45M, and runs of both at 6.72M (bench-results/harness/adapter)
- Alternatives: the engine layer (`internal/engine`) behind the adapter (it adds the command log and channels, which the reference engines run here do not include); one call or one push per message (numbers above); an adapter-side flat array of IDs like the Liquibook adapter's (the book needs its index anyway, for cancels from the API)
