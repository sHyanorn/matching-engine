package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// measureChecked runs cfg's OE1 flow and checks its trades against the
// engine's HTTP side, as the REST mode does (D036): the rise of
// matching_engine_trades_total against the trade Events of the replies, and
// with -push the stream of trades:SYMBOL against both, which also gives flow
// T, the time from the due time of each order that traded to its first push.
// The counter is read before the session starts and after it has ended, and
// the pushes are joined with the replies once the run is over, so none of
// that happens during the run. The stream reader does: with -push it runs
// while the session does and decodes each push, so its CPU time is in the
// run's latencies.
//
// A counter that cannot be read before the run stops it before anything is
// opened. One that cannot be read after it makes the run invalid.
func measureChecked(ctx context.Context, cfg config) (report, error) {
	l := &loader{cfg: cfg, base: "http://" + cfg.httpAddr}
	l.cfg.addr = cfg.httpAddr // where subscribe dials
	l.join.init()

	before, err := l.tradesTotal(ctx)
	if err != nil {
		return report{}, fmt.Errorf("reading matching_engine_trades_total before the run: %w", err)
	}
	var stream *websocket.Conn
	var closing atomic.Bool
	streamErr := make(chan error, 1)
	if cfg.push {
		if stream, err = l.subscribe(ctx); err != nil {
			return report{}, fmt.Errorf("subscribing to trades:%s: %w", cfg.symbol, err)
		}
		go func() { streamErr <- l.readTrades(stream, &closing) }()
		defer func() { _ = stream.CloseNow() }() // after a failure below
	}

	r, err := runOE(ctx, cfg)
	if err != nil {
		return report{}, err
	}
	rep := r.report()
	replied := tradeSum{n: r.trades, sum: r.tradeSum}
	missing := 0
	var readErr error
	if stream != nil {
		l.joinReplies(r)
		missing = l.join.wait(cfg.pushTimeout(), &l.tape.pushed, replied.n)
		closing.Store(true)
		_ = stream.Close(websocket.StatusNormalClosure, "")
		readErr = <-streamErr // the reader has stopped: l.tape is final
		rep.Latency["trade_push"] = summarize(l.join.samples)
	}
	after, afterErr := l.tradesTotal(ctx)
	var problems []string
	rep.Trades, problems = l.tradeProblems(replied, stream != nil, missing, readErr, after-before, afterErr)
	rep.CounterRead = afterErr == nil
	rep.Problems = append(rep.Problems, problems...)
	rep.Valid = len(rep.Problems) == 0
	return rep, nil
}

// joinReplies pairs the first push of each order that traded with the
// order's due time. A push is noted when it arrives, whether or not the
// order's Done is in, so it does not matter that the Done is told last.
func (l *loader) joinReplies(r *oeRun) {
	warmup := int64(r.cfg.warmup)
	for seq, c := range r.cmds[:r.nsent] {
		if r.res[seq] == orderTraded {
			due := r.due(int(c.request))
			l.join.reply(r.ids[c.order].Load(), r.t0.Add(time.Duration(due)), due >= warmup)
		}
	}
}
