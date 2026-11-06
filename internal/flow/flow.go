// Package flow generates the reproducible order flow of SPEC §9: GTC limit
// orders whose prices are normally distributed around a mid price, and
// about 30% cancels of earlier orders, some of them already filled. The
// matching core benchmarks and cmd/loadgen both use it, so they measure
// the same flow.
package flow

import "math/rand/v2"

// Mid is the price the flow centers on.
const Mid = 10_000

// Op is one step of the flow: a new order, or a cancel of an earlier one.
type Op struct {
	Cancel bool
	// Order numbers the order this op places or cancels: new orders are
	// numbered from 1 in flow order.
	Order int
	// For a new order only.
	Buy   bool
	Price int64
	Qty   int64
}

// Generator yields the flow one op at a time. The ops depend only on the
// seed and the spread.
type Generator struct {
	r      *rand.Rand
	sd     float64
	orders int // new orders so far
}

// New returns a generator whose prices have standard deviation sd ticks.
func New(seed uint64, sd float64) *Generator {
	return &Generator{r: rand.New(rand.NewPCG(seed, 0xf10)), sd: sd}
}

// Next returns the next op.
func (g *Generator) Next() Op {
	if g.orders > 0 && g.r.IntN(100) < 30 {
		return Op{Cancel: true, Order: 1 + g.r.IntN(g.orders)}
	}
	g.orders++
	off := g.r.NormFloat64() * g.sd
	buy := g.r.IntN(2) != 0
	// Buyers center slightly below mid and sellers slightly above, so a
	// fraction of orders cross and trade, and the rest rest.
	p := Mid + int64(off)
	if buy {
		p -= 2
	} else {
		p += 2
	}
	return Op{Order: g.orders, Buy: buy, Price: p, Qty: int64(1 + g.r.IntN(100))}
}
