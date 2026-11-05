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
- `client_id`: optional, at most 64 bytes, echoed back in order status.
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
  "level_side": "sell", "level_qty": 0 }
```

`type` is `accepted`, `rejected`, `trade`, `rested` or `canceled`. Fields a type does not use are omitted, except that `level_qty` is present whenever `level_side` is.

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
- `book_delta`: the levels one command changed, each with its new total quantity (0 removes the level): `"changes": [ { "side": "sell", "price": 6500100, "qty": 0 } ]`.
- `trade`: one execution, with `price`, `qty`, `taker_side`, `taker_order_id`, `maker_order_id` and `ts`.
- `subscribed` and `unsubscribed`: acknowledge a channel.
- `error`: a bad message, with `code` and `message`; the connection stays open.

A snapshot followed by its deltas reproduces the book. A client that falls behind (1024 updates behind on a channel, over 1 MB of unsent messages, or one message unsent for 10 s) is disconnected with close code 1008 and reason `slow consumer`; it reconnects and subscribes again for a fresh snapshot. On shutdown connections are closed with code 1001.

## Operations

- `GET /healthz`: `200 {"status":"ok"}`, or `503 {"status":"shutting_down"}` once shutdown has begun. From then on new orders, cancels and WebSocket connections get `503 shutting_down`.
- `GET /metrics`: Prometheus metrics:

| Metric | Type | Labels |
|---|---|---|
| `matching_engine_orders_total` | counter | `symbol`, `result`: `accepted`, `rejected`, `invalid`, `busy`, `failed` |
| `matching_engine_cancels_total` | counter | `symbol`, `result`: `canceled`, `unknown`, `finished`, `busy`, `failed` |
| `matching_engine_trades_total` | counter | `symbol` |
| `matching_engine_queue_length` | gauge | `symbol` |
| `matching_engine_http_request_duration_seconds` | histogram | `route`, `code` |
| `matching_engine_ws_connections` | gauge | |

`rejected` counts orders the book refused (for example `fok_unfillable`); `failed` counts commands not applied because of a log failure or shutdown. A command that took effect counts as `accepted`, `rejected` or its cancel result even if its log then failed. Requests that name no configured symbol, such as a cancel of an ID the engine never issued, are not counted.
