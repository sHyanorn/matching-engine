package book

// Level is one aggregated (L2) price level of the book: a price, the total
// quantity resting there and how many orders make it up.
type Level struct {
	Price  Price
	Qty    Qty
	Orders uint32
}

// Depth appends up to n of side s's best levels to dst, best price first,
// and returns it: highest bids first on the Buy side, lowest asks first on
// the Sell side. It costs O(n + log P) and, once dst has room, allocates
// nothing. It appends nothing for n <= 0 or an invalid side.
func (b *Book) Depth(s Side, n int, dst []Level) []Level {
	if !s.Valid() {
		return dst
	}
	for l := b.side(s).best; l != nil && n > 0; l = l.next() {
		dst = append(dst, Level{Price: l.price, Qty: l.totalQty, Orders: l.count})
		n--
	}
	return dst
}
