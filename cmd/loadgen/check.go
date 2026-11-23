package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
)

// The trade check of a run (D030): every trade the replies report is
// pushed once, in order. Three counts must agree: the trade events in the
// replies, the trade pushes and the rise of matching_engine_trades_total.
// Each trade is also hashed on both sides and the hashes summed, so a
// push that is missing, duplicated or altered changes the sum whatever
// the order the replies arrived in (an additive multiset hash). On the
// stream, seq must run 1, 2, 3, ... and book_seq must rise.

// trade is what both a reply and a push say about one execution.
type trade struct {
	bookSeq, taker, maker uint64
	price, qty            int64
}

// hash mixes t's fields with the splitmix64 finalizer.
func (t trade) hash() uint64 {
	h := uint64(0)
	for _, v := range [...]uint64{t.bookSeq, t.taker, t.maker, uint64(t.price), uint64(t.qty)} {
		h = mix64(h ^ v)
	}
	return h
}

func mix64(z uint64) uint64 {
	z += 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

type tradeSum struct {
	n   int
	sum uint64
}

func (s *tradeSum) add(t trade) {
	s.n++
	s.sum += t.hash()
}

func (s tradeSum) plus(o tradeSum) tradeSum { return tradeSum{s.n + o.n, s.sum + o.sum} }

// tape checks the trade pushes as they arrive. Only the stream reader
// touches it, except pushed, which the run polls while it waits.
type tape struct {
	pushed  atomic.Int64
	sum     uint64 // of the pushes' hashes
	seq     uint64 // of the last push
	bookSeq uint64 // of the last push
	problem string // the first push out of sequence
}

// push checks one trade push, numbered seq on the channel.
func (t *tape) push(seq uint64, tr trade) {
	switch {
	case t.problem != "":
	case seq != t.seq+1:
		t.problem = fmt.Sprintf("trade push seq %d after %d", seq, t.seq)
	case tr.bookSeq <= t.bookSeq:
		t.problem = fmt.Sprintf("trade push book_seq %d after %d", tr.bookSeq, t.bookSeq)
	}
	t.seq, t.bookSeq = seq, tr.bookSeq
	t.sum += tr.hash()
	t.pushed.Add(1)
}

// scanTrades adds the trade events of a reply's events to s. It reads them
// in place, relying on the field order of docs/API.md events, as classify
// does for the order ID; decoding the reply would cost loadgen more CPU
// than the engine spends on it. It returns false if a trade event lacks a
// field.
func scanTrades(events []byte, s *tradeSum) bool {
	for {
		i := bytes.Index(events, tradeEvent)
		if i < 0 {
			return true
		}
		start := bytes.LastIndex(events[:i], seqKey)
		if start < 0 {
			return false
		}
		var t trade
		var ok bool
		rest := events[start:]
		if t.bookSeq, rest, ok = uintAfter(rest, seqKey); !ok {
			return false
		}
		if t.taker, rest, ok = uintAfter(rest, orderIDKey); !ok {
			return false
		}
		if t.maker, rest, ok = uintAfter(rest, makerIDKey); !ok {
			return false
		}
		var price, qty uint64
		if price, rest, ok = uintAfter(rest, priceKey); !ok {
			return false
		}
		if qty, rest, ok = uintAfter(rest, qtyKey); !ok {
			return false
		}
		t.price, t.qty = int64(price), int64(qty)
		s.add(t)
		events = rest
	}
}

var (
	seqKey     = []byte(`{"seq":`)
	orderIDKey = []byte(`"order_id":`)
	makerIDKey = []byte(`"maker_id":`)
	priceKey   = []byte(`"price":`)
	qtyKey     = []byte(`"qty":`)
)

// uintAfter parses the unsigned integer after the first key in b and
// returns what follows it.
func uintAfter(b, key []byte) (uint64, []byte, bool) {
	i := bytes.Index(b, key)
	if i < 0 {
		return 0, nil, false
	}
	b = b[i+len(key):]
	n := 0
	var v uint64
	for n < len(b) && b[n] >= '0' && b[n] <= '9' && n < 19 {
		v = v*10 + uint64(b[n]-'0')
		n++
	}
	return v, b[n:], n > 0
}

// tradeProblems makes the trade checks of a run, which the REST mode and the
// OE1 mode share, and returns the trades each source counted and what is
// wrong with them. The checks of the stream are made only if the run read it
// (streamed): missing is the number of measured orders that traded and got no
// push, and streamErr what ended the stream. rose is the rise of the engine's
// counter, which metricsErr says could not be read.
func (l *loader) tradeProblems(replied tradeSum, streamed bool, missing int, streamErr error, rose int, metricsErr error) (tradeCounts, []string) {
	var problems []string
	counts := tradeCounts{Replies: replied.n}
	if metricsErr == nil { // else rose is not a rise
		counts.Metric = rose
	}
	if streamed {
		if missing > 0 {
			problems = append(problems, fmt.Sprintf("%d orders that traded got no trade push", missing))
		}
		if streamErr != nil {
			problems = append(problems, fmt.Sprintf("trade stream ended: %v", streamErr))
		}
		tp := &l.tape
		counts.Pushes = int(tp.pushed.Load())
		switch {
		case tp.problem != "":
			problems = append(problems, tp.problem)
		case counts.Pushes != replied.n:
			problems = append(problems, fmt.Sprintf("%d trade pushes for %d trades in the replies", counts.Pushes, replied.n))
		case tp.sum != replied.sum:
			problems = append(problems, "the trade pushes differ from the trades in the replies")
		}
	}
	switch {
	case metricsErr != nil:
		problems = append(problems, fmt.Sprintf("reading matching_engine_trades_total: %v", metricsErr))
	case rose != replied.n:
		problems = append(problems, fmt.Sprintf("matching_engine_trades_total rose by %d for %d trades in the replies", rose, replied.n))
	}
	return counts, problems
}

// tradesTotal reads the engine's matching_engine_trades_total for symbol.
// The engine exports the series from its start, at 0, so a missing line
// is an error.
func (l *loader) tradesTotal(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.base+"/metrics", nil)
	if err != nil {
		return 0, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("/metrics answered %d", res.StatusCode)
	}
	prefix := []byte(`matching_engine_trades_total{symbol=` + strconv.Quote(l.cfg.symbol) + `} `)
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		if v, ok := bytes.CutPrefix(sc.Bytes(), prefix); ok {
			f, err := strconv.ParseFloat(string(v), 64)
			return int(f), err
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("/metrics has no matching_engine_trades_total line for symbol %q", l.cfg.symbol)
}
