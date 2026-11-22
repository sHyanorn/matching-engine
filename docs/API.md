# API

All requests and responses are JSON. Prices are integers in ticks and quantities integers in lots; a number with a fraction is rejected. Order IDs are assigned by the engine and are unique across symbols.

## Errors

Every error has the same shape:

```json
{ "error": { "code": "invalid_qty", "message": "engine: invalid command: invalid_qty" } }
```

| Status | Codes | Meaning |
|---|---|---|
| 400 | `bad_request`, `invalid_order_id`, `invalid_client_id`, `invalid_depth`, and every validation reason (`invalid_side`, `invalid_type`, `invalid_tif`, `market_gtc`, `invalid_price`, `invalid_qty`) | The request is malformed or invalid; nothing was applied |
| 404 | `unknown_symbol`, `unknown_order`, `not_found` | No such symbol, order or route |
| 405 | `method_not_allowed` | The route does not take this method; see `Allow` |
| 409 | `order_finished` | The order exists but is filled, canceled or rejected |
| 413 | `body_too_large` | The request body is over 4 KB |
| 500 | `log_failed`, `internal` | `log_failed`: the command log failed, and the symbol accepts no more commands. `internal`: an unexpected error; nothing was applied |
| 503 | `busy` (with `Retry-After: 1`), `shutting_down` | Try again later; nothing was applied |

A `log_failed` reply to `POST` or `DELETE` that also holds `order` (when there is one) and `events` means the command took effect, but its log record may not be durable.

## Orders

### `POST /v1/orders`

```json
{ "symbol": "BTC-USD", "side": "buy", "type": "limit", "tif": "gtc",
  "price": 6500000, "qty": 150, "client_id": "c-001" }
```

- `side`: `buy` or `sell`. `type`: `limit` or `market`.
- `tif`: `gtc`, `ioc` or `fok`; optional, defaulting to `gtc` for limit orders and `ioc` for market orders.
- `price`: required for limit orders, omitted or 0 for market orders.
- `client_id`: optional, at most 64 bytes, echoed back in order status. It is not checked for uniqueness: a retried request places a second order.
- The body is one JSON object. Field names are case-sensitive; unknown and repeated fields are rejected.

`200 OK` once the order has been matched, with the order's status and the events it produced:

```json
{ "order": { ... order status ... }, "events": [ ... events ... ] }
```

A FOK order that cannot fill in full also returns `200`, with `"status": "rejected"` and `"reason": "fok_unfillable"`.

### `DELETE /v1/orders/{id}`

`200 OK` with the order's final status (`"status": "canceled"`) and the events. `404 unknown_order` if the order was never issued or its status has been forgotten, `409 order_finished` if it is no longer resting.

### `GET /v1/orders/{id}`

`200 OK` with the order status. Resting orders are always found; each symbol remembers its 100,000 most recently finished orders.

### Order status

```json
{ "order_id": 2305, "symbol": "BTC-USD", "client_id": "c-001",
  "side": "buy", "type": "limit", "tif": "gtc", "price": 6500000,
  "qty": 150, "filled_qty": 40, "remaining_qty": 110,
  "status": "open", "reason": "", "ts": 1730000000000000000 }
```

- `status`: `open` (resting, possibly partly filled), `filled`, `canceled` (by the user, or the unfilled part of an IOC or market order) or `rejected`.
- `reason`: why it was canceled or rejected (`user_cancel`, `ioc_remainder`, `fok_unfillable`), otherwise empty.
- `remaining_qty`: what is still resting; 0 once the order has finished.
- `ts`: the order's timestamp in Unix nanoseconds, set by the engine.

### Events

The events of one command, in order, as defined in `internal/book/doc.go`:

```json
{ "seq": 7, "ts": 1730000000000000000, "type": "trade", "order_id": 2305,
  "maker_id": 1281, "side": "buy", "price": 6500000, "qty": 40,
  "level_side": "sell", "level_qty": 0, "level_orders": 0 }
```

`type` is `accepted`, `rejected`, `trade`, `rested` or `canceled`. Fields a type does not use are omitted. An event that changed a price level carries `level_side`, `level_qty` and `level_orders`, the level's total quantity and order count after the event at `price`; the three are always present together, even when 0.

## Market data

### `GET /v1/books/{symbol}?depth=20`

`depth` is 1 to 1000, default 20.

```json
{ "symbol": "BTC-USD", "seq": 42,
  "bids": [ { "price": 6499900, "qty": 300, "orders": 2 } ],
  "asks": [ { "price": 6500100, "qty": 150, "orders": 1 } ] }
```

`seq` is the book's last event sequence number reflected in the snapshot.

### WebSocket `/v1/stream`

Client messages:

```json
{ "op": "subscribe", "channels": ["trades:BTC-USD", "book:BTC-USD"] }
{ "op": "unsubscribe", "channels": ["book:BTC-USD"] }
```

Data messages (`book_snapshot`, `book_delta`, `trade`) carry the `channel`, a `seq` that counts the subscription's data messages from 1 with no gaps (subscribing again starts a new count), and `book_seq`, the book event sequence number they reflect. `subscribed`, `unsubscribed` and `error` carry no `seq`; a `subscribed` acknowledgement comes before the channel's first data message.

- `book_snapshot`: the first message of a `book:` channel, with every level in `bids` and `asks`.
- `book_delta`: the levels one command changed, each with its new total quantity and order count (0 removes the level): `"changes": [ { "side": "sell", "price": 6500100, "qty": 0, "orders": 0 } ]`.
- `trade`: one execution, with `price`, `qty`, `taker_side`, `taker_order_id`, `maker_order_id` and `ts`.
- `subscribed` and `unsubscribed`: acknowledge a channel.
- `error`: a bad message, with `code` and `message`; the connection stays open. A client message over 4 KB gets no `error`: the connection is closed with code 1009 and counted as `client` in `matching_engine_ws_disconnects_total`.

A snapshot followed by its deltas reproduces the book's levels, order counts included. A client that falls behind (1024 updates behind on a channel, over 1 MB of unsent messages apart from the largest, or over 10 s spent writing one message) is disconnected with close code 1008 and reason `slow consumer`; it reconnects and subscribes again for a fresh snapshot. On shutdown connections are closed with code 1001.

## Binary order entry (OE1)

OE1 is a binary protocol for sending orders and cancels over a persistent TCP session in fixed-length frames ([D041](DECISIONS.md)). The engine serves it on a listener of its own when started with `-oe-addr`, for example `-oe-addr 127.0.0.1:9000`; without the flag there is none. Like the HTTP API it has no TLS and no authentication. Depth, order status and market data stay on REST and WebSocket.

**What this version serves:** logging in to a new session, `NewOrder`, `Cancel`, `Ping`, `Heartbeat` and `Logout`. Resuming a session is not served yet: a session ends with its connection, so a `Login` that names an earlier session gets `LoginRejected` with `unknown_session`.

### Frames

Every frame is `length u16 | type u8 | body`, all integers little-endian. `length` counts the type byte and the body, and each type has one body size. The server closes the connection on a frame whose length is not the one its type has, or whose type it does not take. Padding is sent as zeros and ignored on receipt.

Client to server:

| Type | Frame | Body | Bytes |
|---|---|---|---|
| `L` | Login | version u16, session u64, next_server_seq u64 | 21 |
| `N` | NewOrder | client_seq u64, symbol u8, side u8, type u8, tif u8, price i64, qty i64 | 31 |
| `C` | Cancel | client_seq u64, order_id u64 | 19 |
| `P` | Ping | client_seq u64, 20 bytes of padding | 31 |
| `H` | Heartbeat | none | 3 |
| `O` | Logout | none | 3 |

Server to client:

| Type | Frame | Body | Bytes |
|---|---|---|---|
| `A` | LoginAccepted | session u64, next_server_seq u64, last_client_seq u64 | 27 |
| `J` | LoginRejected | reason u8 | 4 |
| `Y` | Symbol | index u8, name in 32 bytes padded with zeros | 36 |
| `E` | Event | server_seq u64, client_seq u64, event (64 bytes) | 83 |
| `D` | Done | server_seq u64, client_seq u64, status u8, reason u8, order_id u64, order_state u8 | 30 |
| `Q` | Pong | client_seq u64, 19 bytes of padding | 30 |
| `H` | Heartbeat | none | 3 |
| `Z` | EndOfSession | none | 3 |

An event is `seq u64, ts i64, type u8, side u8, reason u8, level_side u8, level_orders u32, order_id u64, maker_id u64, price i64, qty i64, level_qty i64`: the fields of an event in a REST reply, 0 where the REST reply omits one. Enumerations have the numbers of `internal/book`, which never change (D008):

- `side` and `level_side`: buy 1, sell 2. `type` of an order: limit 1, market 2. `tif`: gtc 1, ioc 2, fok 3.
- `type` of an event: accepted 1, rejected 2, trade 3, rested 4, canceled 5.
- `reason`: none 0, invalid_command 1, invalid_order_id 2, invalid_side 3, invalid_type 4, invalid_tif 5, market_gtc 6, invalid_price 7, invalid_qty 8, unexpected_field 9, duplicate_order_id 10, unknown_order 11, fok_unfillable 12, user_cancel 13, ioc_remainder 14.

### Sessions

The first frame on a connection must be `Login`, with version 1 and session 0 to open a new session. The server answers `LoginAccepted` with the session's number, a random nonzero 64-bit value, so that a number from before an engine restart is not taken for a new session, next_server_seq 1 and last_client_seq 0, then one `Symbol` frame per configured symbol in index order; the index is the low byte of each of the symbol's order IDs. `LoginRejected` says why a login was refused, and the server then closes the connection: bad_version 1, unknown_session 2, replay_lost 3 (the output the client missed is no longer held), too_many_sessions 4. This version sends only bad_version and unknown_session, and has no limit on sessions.

`client_seq` starts at 1 in a session and rises by one with each `NewOrder`, `Cancel` and `Ping`. A frame whose number is not above the last one taken is a resend and is dropped; a number that skips one closes the connection. `server_seq` starts at 1 and numbers every `Event` and `Done` of the session. A `Ping` is answered with a `Pong` of the same client_seq without reaching the engine; a `Pong` has no server_seq.

Either side sends `Heartbeat` after 1 s without sending anything, and the server closes a connection on which nothing has arrived for 15 s. `Logout` ends the session: the server answers every command it has taken, sends all it holds for the client, then closes the connection; frames after the `Logout` are not read. When the server shuts down, it answers every command that arrives with `Done` shutting_down, answers the commands it has taken, sends `EndOfSession` and closes the connection, and sends `EndOfSession` only then.

### Commands and replies

A `NewOrder` is `POST /v1/orders` without `client_id`, naming its symbol by index and with every field required: a tif of 0 is invalid, where REST would take a default. A `Cancel` is `DELETE /v1/orders/{id}`. Commands of one session for one symbol are applied in the order sent. The reply to a command is one `Event` frame per event in the REST reply, in book order, then one `Done`; the frames of one command come together, and a reply that holds no events, as the 404 and 409 answers hold none, is a `Done` alone. Replies are matched to commands by client_seq: replies to commands for different symbols, and the `Done` that the server gives at once to a command it did not queue (`invalid`, `unknown_symbol`, `unknown_order`, `busy`, `shutting_down`), can come in another order than the commands were sent. `Done.status` matches the REST answer one to one:

| `status` | REST answer |
|---|---|
| ok 1 | 200 |
| invalid 2 | 400 with a validation reason, which `Done.reason` carries; nothing was applied |
| unknown_symbol 3 | 404 `unknown_symbol` |
| unknown_order 4 | 404 `unknown_order` |
| order_finished 5 | 409 `order_finished` |
| internal 6 | 500 `internal` |
| log_failed 7 | 500 `log_failed` without events: not applied |
| log_failed_applied 8 | 500 `log_failed` with events: applied, but its log record may not be durable |
| busy 9 | 503 `busy`: the symbol's queue was full, and nothing was queued |
| shutting_down 10 | 503 `shutting_down` |

`order_id` is the new order's ID, also when it failed validation (its ID was taken), or the ID the cancel named; 0 if the command got no ID. `order_state` is the order's status after the command, open 1, filled 2, canceled 3 or rejected 4, and 0 if the REST answer holds no order, as a 409 does. For any status but invalid, `reason` is the reason in that order status (`user_cancel`, `ioc_remainder`, `fok_unfillable`), or 0.

### Disconnects

Orders stay on the book when a connection closes: there is no cancel on disconnect. An engine restart ends every session: as for a REST client whose connection broke before the reply, the outcome of a command whose `Done` never arrived is then unknown.

**Not served yet:** a client that reconnects within 60 s can resume its session. It logs in with the session and the next server_seq it expects, gets `LoginAccepted` with the last client_seq the server took, then every `Event` and `Done` it missed, then live traffic, and sends again its own frames above that number. Each command is then applied once and answered once. The server holds the last 1 MB of a session's output for this; a client that comes back later, or has missed more, gets `LoginRejected` and can look up the order IDs it knows with `GET /v1/orders/{id}`.

### Flow control

A session whose unsent output passes 1 MB, or whose output takes over 10 s to write, is disconnected as a slow consumer. A client may put any number of frames in one write, and the server writes everything ready for a session in one write, without waiting for more.

A full symbol queue is answered with `Done` busy at once, and the server stops reading from a session that has 1,024 `NewOrder` and `Cancel` frames taken and not yet answered, so TCP holds the client back. A command answered at once does not count, nor does a `Ping`.

## Operations

- `GET /`: a page that follows one symbol's book and trades over the stream; `/?symbol=ETH-USD` picks another.

- `GET /healthz`: `200 {"status":"ok"}`, or `503 {"status":"shutting_down"}` once shutdown has begun. From then on new orders, cancels and WebSocket connections get `503 shutting_down`. Once a symbol's command log has failed, the answer is `200 {"status":"degraded","log_failed":["BTC-USD"]}`: that symbol takes no more orders or cancels until a restart, and the others serve as before. While order IDs cannot be reserved (for example on a full disk), the answer is `200 {"status":"degraded","order_ids_failed":true}`: new orders get `500 internal` on every symbol, cancels and queries are served, and the first order whose reservation succeeds brings back `ok`.
- `GET /metrics`: Prometheus metrics:

| Metric | Type | Labels |
|---|---|---|
| `matching_engine_orders_total` | counter | `symbol`, `result`: `accepted`, `rejected`, `invalid`, `busy`, `failed` |
| `matching_engine_cancels_total` | counter | `symbol`, `result`: `canceled`, `unknown`, `finished`, `busy`, `failed` |
| `matching_engine_trades_total` | counter | `symbol` |
| `matching_engine_queue_length` | gauge | `symbol` |
| `matching_engine_log_failed` | gauge | `symbol`; 1 once its command log has failed |
| `matching_engine_order_ids_failed` | gauge | 1 while reserving order IDs fails |
| `matching_engine_log_fsync_duration_seconds` | histogram | `symbol` |
| `matching_engine_http_request_duration_seconds` | histogram | `route`, `code` |
| `matching_engine_ws_connections` | gauge | |
| `matching_engine_ws_disconnects_total` | counter | `reason`: `client`, `shutdown`, `write_failed`, and for slow consumers `slow_consumer_updates`, `slow_consumer_bytes`, `slow_consumer_write` |

The Go runtime and process metrics (`go_*`, `process_*`) are served too.

The three counters of orders, cancels and trades count REST requests; OE1 commands are not counted in them. `rejected` counts orders the book refused (for example `fok_unfillable`); `failed` counts commands not applied because of a log failure or shutdown. A command that took effect counts as `accepted`, `rejected` or its cancel result even if its log then failed. Requests that name no configured symbol are not counted, nor is a cancel of an ID whose counter the engine has not issued yet ([D019](DECISIONS.md)). Any other cancel goes to the symbol the ID's low byte names, even if that symbol did not issue it: it is written to that symbol's command log, rejected, and counted there as `unknown`.
