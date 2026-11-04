package d004

// Price is a price in ticks.
type Price int64

// Level stands in for the order book's price level. Implementations embed it
// in their node, so the *Level handed out stays valid until Delete.
type Level struct {
	Price    Price
	TotalQty int64
}

// levelSet is the ordered set of price levels for one side of the book.
// "Best" is the lowest price on an ascending (ask) side and the highest on a
// descending (bid) side. Implementations must be deterministic (no math/rand,
// no time) and use only the standard library.
type levelSet interface {
	// Find returns the level at p, or nil. O(log P).
	Find(p Price) *Level
	// Insert adds a new, empty level at p and returns it. p must not exist.
	// O(log P).
	Insert(p Price) *Level
	// Delete removes the level at p, which must exist. O(log P).
	Delete(p Price)
	// Best returns the best level, or nil if empty. Must be O(1).
	Best() *Level
	// Walk calls fn on levels from the best outward until fn returns false.
	// Visiting the first n levels must be O(n) (plus at most O(log P)).
	Walk(fn func(*Level) bool)
	// Len returns the number of levels.
	Len() int
}
