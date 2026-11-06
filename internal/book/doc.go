// Package book implements the limit order book for one symbol: price levels,
// FIFO queues and the price-time priority matching algorithm. See
// docs/SPEC.md.
//
// The book is a deterministic state machine. It consumes [Command] values and
// emits [Event] values; it never reads a clock, never draws random numbers
// and never does I/O. CI guards against accidental dependencies by forbidding
// imports of time, math/rand, crypto/rand, net and os (depguard, see D005).
// Everything that is not deterministic happens in internal/engine before a
// Command reaches the book, so replaying the command log into a fresh book
// yields exactly the same events, Seq for Seq.
//
// # What lives where
//
//   - book: the domain types and their wire names, stateless validation
//     ([Command.Validate]), matching, and the meaning, order and numbering of
//     events.
//   - engine: OrderID allocation (one counter shared by all symbols, called
//     by the API before it validates a new order), timestamps, the command
//     log with its format version and replay, client_id, order history and
//     status after an order leaves the book, and the sequence numbers of
//     WebSocket channels.
//   - api: JSON shapes, parsing wire names with [ParseSide], [ParseOrderType]
//     and [ParseTIF], and mapping [Reason] codes to HTTP status codes.
//
// # Event grammar
//
// Every command produces exactly one of these event sequences:
//
//	new order X:  Rejected(X)
//	           |  Accepted(X) Trade(taker=X)* [ Rested(X) | Canceled(X, ioc_remainder) ]
//	cancel X:     Rejected(X)
//	           |  Canceled(X, user_cancel)
//	any other X:  Rejected(X, invalid_command)
//
// Rejected means the command changed nothing. After Accepted, trades come
// best price first and, within one price, in time priority (arrival order).
// What is left then rests (a GTC limit order) or is canceled (an IOC limit
// order or a market order); a fully filled order ends with its last Trade. A
// FOK order that cannot fill in full is Rejected(fok_unfillable) with no
// Accepted, so it never trades partially (SPEC §6.4).
//
// # Quantity conservation
//
// Every quantity movement is an explicit event field, so for every accepted
// order X, after the last event of every command (inside a command, a
// taker's Remaining is not visible in the stream):
//
//	Accepted.Qty = Σ Trade.Qty with X as taker or maker
//	             + Σ Canceled.Qty of X
//	             + Remaining(X)
//
// where Remaining(X) is the order's Remaining while it rests and 0 once it
// has left the book. Events that change the resting book also carry the new
// total of the level they changed ([Event.LevelUpdate]), so L2 depth can be
// followed from the event stream alone.
//
// # Sequence numbers
//
// Event.Seq is per book: 1, 2, 3, ... with no gaps, rejections included. A
// trade's Seq is its trade number (SPEC §6.5), and a snapshot records the
// last Seq it reflects. Seq is reproducible because the engine logs exactly
// the commands it applies, in order, and never logs or applies a command
// that fails [Command.Validate]; it answers such a command with 400. WebSocket
// channels carry their own gap-free counters and quote Seq only for snapshot
// alignment, because a channel sees only some of the book's events.
//
// # Output buffer
//
// Book.Apply (M1) appends a command's events to a slice the caller owns, in
// the style of strconv.AppendInt:
//
//	buf = b.Apply(cmd, buf[:0])
//
// Once buf has grown, emitting an event allocates nothing. The next call
// overwrites buf, so a caller that hands events to another goroutine must
// copy them first.
//
// # Wire values
//
// Every enum's zero value is invalid (Reason's zero is [ReasonNone]), and
// every numeric value is written out and pinned by tests: CommandKind, Side,
// OrderType and TIF are stored in the command log, and EventKind and Reason
// are part of the output contract. Append new values; never renumber.
package book
