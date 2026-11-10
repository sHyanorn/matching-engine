package engine

import "github.com/sHyanorn/matching-engine/internal/book"

// An order ID is one engine-wide counter value shifted left by symbolBits,
// with the index of the order's symbol in the low bits:
//
//	id = counter<<symbolBits | symbolIndex
//
// Every ID is unique across symbols because the counter is shared (D007),
// and DELETE or GET /v1/orders/{id}, which name no symbol, are routed to
// the right book from the ID alone, with no lookup table and no lock.
// IDs stay below 2^53, exact in JavaScript clients, for the first
// 2^45 - 1 orders (about 35 trillion).
const (
	symbolBits = 8
	// MaxSymbols is how many symbols one engine can serve.
	MaxSymbols = 1 << symbolBits
)

// makeOrderID returns the ID for a counter value and the index sym of the
// order's symbol. The counter starts at 1, so no ID is 0.
func makeOrderID(counter uint64, sym int) book.OrderID {
	return book.OrderID(counter<<symbolBits | uint64(sym))
}

// symbolIndexOf returns the index of the symbol an ID was issued for.
func symbolIndexOf(id book.OrderID) int { return int(id & (MaxSymbols - 1)) }

// counterOf returns the counter value an ID was issued from.
func counterOf(id book.OrderID) uint64 { return uint64(id) >> symbolBits }
