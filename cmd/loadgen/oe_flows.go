package main

import (
	"fmt"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
	"github.com/sHyanorn/matching-engine/internal/oe"
)

// The OE1 flows that need no random flow, and in which nothing trades.
//
// R1 (-ioc) sends an IOC buy for one lot at a price no ask meets: the engine
// accepts it and cancels it at once, so each request is one round trip
// through the engine that leaves the book as it was. R2 (-ack-cancel) sends
// a GTC buy for one lot, which rests, and, when its Done is in, a Cancel:
// the closed loop of an exchange's own test, closed in the pacer so that the
// orders stay open loop. Both want an engine whose ask side is empty: a
// trade Event in a reply fails the run.
const (
	iocPrice = 6_500_000 // ticks
	ackPrice = 6_400_000
)

// fixedOrder appends request j as a buy of one lot and notes it as the next
// command. Orders are numbered as their requests are, from 1.
func (r *oeRun) fixedOrder(j int, tif book.TIF, price book.Price) {
	r.out = oe.NewOrder{ClientSeq: uint64(r.nsent + 1), Symbol: r.symbol, Side: book.Buy, Type: book.Limit, TIF: tif,
		Price: price, Qty: 1}.Append(r.out)
	r.cmds[r.nsent] = command{request: int32(j), order: int32(j + 1)}
	r.nsent++
}

// queue hands the pacer an order that rests, to cancel. Only the reader
// calls it.
func (r *oeRun) queue(order int32) {
	n := r.queued.Load()
	r.cq[n] = order
	r.queued.Store(n + 1)
}

// cancels appends a Cancel for each order that is queued and not yet taken,
// up to room of them, and notes them as commands. A Cancel has no request
// of its own, so it names its order's.
func (r *oeRun) cancels(room int) {
	end := min(int(r.queued.Load()), r.taken+room)
	for ; r.taken < end; r.taken++ {
		order := r.cq[r.taken]
		r.out = oe.Cancel{ClientSeq: uint64(r.nsent + 1), OrderID: book.OrderID(r.ids[order].Load())}.Append(r.out)
		r.cmds[r.nsent] = command{request: order - 1}
		r.nsent++
	}
}

// iocOutcome names the Done of an order of R1: it is canceled with the IOC
// remainder, and its Events are the two of an order that does not trade,
// accepted and canceled, which leave no room for a trade Event. Anything
// else fails the run.
func (r *oeRun) iocOutcome(d oe.Done) outcome {
	switch {
	case d.Status != oe.StatusOK, d.OrderID == 0, d.State != engine.StateCanceled, d.Reason != book.ReasonIOCRemainder,
		r.events != 2, r.kinds != [2]book.EventKind{book.EventAccepted, book.EventCanceled}:
		return commandError
	}
	return orderCanceled
}

// ackOutcome names the Done of an order or a Cancel of R2. An order rests:
// its Events are the two the engine sends for a limit order that rests,
// accepted and rested, which leave no room for a trade Event. A Cancel
// cancels at the user's request the order it named. Anything else fails the
// run.
func (r *oeRun) ackOutcome(c command, d oe.Done) outcome {
	switch {
	case d.Status != oe.StatusOK, r.eventTrades > 0:
	case c.order == 0 && d.State == engine.StateCanceled && d.Reason == book.ReasonUserCancel && d.OrderID == r.orderID(c):
		return cancelCanceled
	case c.order != 0 && d.OrderID != 0 && d.State == engine.StateOpen && r.events == 2 &&
		r.kinds == [2]book.EventKind{book.EventAccepted, book.EventRested}:
		return orderRested
	}
	return commandError
}

// orderID is the ID of the order that c, a command of R2, is or cancels.
func (r *oeRun) orderID(c command) book.OrderID {
	return book.OrderID(r.ids[c.request+1].Load())
}

// found says, for the report, what the first failed command of R1 or R2
// answered with status ok and what the flow wants of it. It is called for
// that command only.
func (r *oeRun) found(c command, d oe.Done) string {
	if d.Status != oe.StatusOK || (!r.cfg.ioc && !r.cfg.ackCancel) {
		return ""
	}
	want := "state open, a nonzero order ID and the Events accepted, rested"
	switch {
	case r.cfg.ioc:
		want = "state canceled, reason ioc_remainder, a nonzero order ID and the Events accepted, canceled"
	case c.order == 0:
		want = fmt.Sprintf("state canceled, reason user_cancel, order %d and no trade Event", r.orderID(c))
	}
	return fmt.Sprintf(" with order %d in state %v, reason %v and %d Events (%d trades, the first two %v, %v); want %s",
		d.OrderID, d.State, d.Reason, r.events, r.eventTrades, r.kinds[0], r.kinds[1], want)
}

// ackCancels fills rep for a run of R2 and returns how many of the window's
// orders went out in it, their send lags, and what makes the run invalid.
// The commands are the orders due in the window and their Cancels, which
// are timed from their own sends: reply is an order's due time to its Done,
// cancel a Cancel's send to its Done, and order_to_cancel the whole way.
// Only orders have a send lag.
func (r *oeRun) ackCancels(rep *report) (sent int, lag []int64, problems []string) {
	cfg := r.cfg
	rep.Mode, rep.Symbol = "oe_ack_cancel", cfg.symbol
	rep.Trades.Replies = r.trades
	start, end := int64(cfg.warmup), int64(cfg.warmup+cfg.duration)
	var reply, cancel, orderToCancel []int64
	missing, failed, warmupFailed, resting := 0, 0, 0, 0
	for seq, c := range r.cmds[:r.nsent] {
		j, out := int(c.request), r.res[seq]
		isCancel := c.order == 0
		if out == noDone {
			missing++
		}
		if out == orderRested && r.cancelAt[j] == 0 { // the warmup's too: it stays on the book
			resting++
		}
		if r.due(j) < start {
			if out.failed() {
				warmupFailed++
			}
			continue
		}
		switch {
		case isCancel:
			rep.Cancels++
		case r.due(j)+r.lag[j] < end:
			sent++
		}
		if out == noDone {
			continue
		}
		rep.Outcomes[outcomeNames[out]]++
		switch {
		case out.failed():
			failed++
		case isCancel:
			cancel = append(cancel, r.rtt[seq])
			orderToCancel = append(orderToCancel, r.cancelAt[j]+r.rtt[seq]-r.due(j))
		default:
			reply, lag = append(reply, r.rtt[seq]), append(lag, r.lag[j])
		}
	}
	problems = r.commandProblems(failed, warmupFailed, missing)
	if resting > 0 {
		problems = append(problems, fmt.Sprintf("orders that rested and were not canceled: %d", resting))
	}
	rep.Latency["reply"], rep.Latency["cancel"], rep.Latency["order_to_cancel"] = summarize(reply), summarize(cancel), summarize(orderToCancel)
	return sent, lag, problems
}
