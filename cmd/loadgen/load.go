package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/sHyanorn/matching-engine/internal/flow"
)

// config is one measurement: the flow offered to the engine at addr at a
// fixed rate.
type config struct {
	addr     string
	symbol   string
	rate     float64 // requests per second
	warmup   time.Duration
	duration time.Duration
	conns    int
	seed     uint64
	sd       float64
}

// report is the result of a measurement, written as JSON.
type report struct {
	Symbol     string             `json:"symbol"`
	TargetRate float64            `json:"target_rate"`
	Achieved   float64            `json:"achieved_rate"`
	Warmup     float64            `json:"warmup_s"`
	Duration   float64            `json:"duration_s"`
	Conns      int                `json:"conns"`
	Seed       uint64             `json:"seed"`
	SD         float64            `json:"sd"`
	Go         string             `json:"go"`
	CPUs       int                `json:"cpus"`
	Valid      bool               `json:"valid"`
	Problems   []string           `json:"problems,omitempty"`
	Outcomes   map[string]int     `json:"outcomes"`
	Latency    map[string]summary `json:"latency_us"`
}

// job is one request of the flow, due at planned.
type job struct {
	op       flow.Op
	planned  time.Time
	measured bool // planned inside the measurement window
}

// loader is the state the workers and the stream reader share.
type loader struct {
	cfg  config
	base string
	end  time.Time       // of the measurement window
	ids  []atomic.Uint64 // order ID by flow order number, once its reply is in
	join tradeJoin
}

// measure offers cfg's flow and measures it. Every request is due at a
// fixed time, t0 + i/rate, whether or not earlier requests have been
// answered, and its latency counts from that time, as in wrk2: a server
// that stalls is charged for every request it delayed, not only for those
// in flight (D030). Requests due during the warmup are sent but not
// counted.
func measure(ctx context.Context, cfg config) (report, error) {
	l := &loader{cfg: cfg, base: "http://" + cfg.addr}
	l.join.init()

	stream, err := l.subscribe(ctx)
	if err != nil {
		return report{}, fmt.Errorf("subscribing to trades:%s: %w", cfg.symbol, err)
	}
	var closing atomic.Bool
	streamErr := make(chan error, 1)
	go func() { streamErr <- l.readTrades(stream, &closing) }()
	defer func() { _ = stream.CloseNow() }() // after a failure below

	// One connection per worker, each opened before the load starts and
	// one at a time: Windows refuses connections beyond its listen
	// backlog.
	workers := make([]*worker, cfg.conns)
	for i := range workers {
		if workers[i], err = l.newWorker(ctx); err != nil {
			return report{}, fmt.Errorf("opening connection %d: %w", i+1, err)
		}
	}

	total := int(cfg.rate * (cfg.warmup + cfg.duration).Seconds())
	l.ids = make([]atomic.Uint64, total+1)
	period := float64(time.Second) / cfg.rate
	t0 := time.Now()
	due := func(i int) time.Time { return t0.Add(time.Duration(float64(i) * period)) }
	start := t0.Add(cfg.warmup)
	l.end = start.Add(cfg.duration)
	hiccups := make(chan []int64, 1)
	go func() { hiccups <- sampleHiccups(start, l.end) }()

	jobs := make(chan job, cfg.conns)
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				w.handle(ctx, j)
			}
		}()
	}

	// Pace in small steps, sending every request that is due: at 50k/s
	// one arrives every 20 µs, finer than a sleep can resolve. If every
	// worker is busy, sends wait, and the delay shows in the latency.
	gen := flow.New(cfg.seed, cfg.sd)
send:
	for i := 0; i < total; {
		for n := min(total, int(float64(time.Since(t0))/period)+1); i < n; i++ {
			select {
			case jobs <- job{op: gen.Next(), planned: due(i), measured: !due(i).Before(start)}:
			case <-ctx.Done():
				break send
			}
		}
		if i < total {
			time.Sleep(time.Until(due(i)))
		}
	}
	close(jobs)
	wg.Wait()
	for _, w := range workers {
		w.client.CloseIdleConnections()
	}
	if err := ctx.Err(); err != nil {
		return report{}, err
	}
	missing := l.join.wait(5 * time.Second)
	closing.Store(true)
	_ = stream.Close(websocket.StatusNormalClosure, "")
	r := l.report(workers, missing, <-streamErr)
	r.Latency["hiccup"] = summarize(<-hiccups)
	return r, nil
}

// sampleHiccups records, from start to end, how late each 1 ms sleep
// wakes up: a pause of loadgen or of the machine, which adds to every
// latency measured meanwhile.
func sampleHiccups(start, end time.Time) []int64 {
	var late []int64
	time.Sleep(time.Until(start))
	for now := time.Now(); now.Before(end); {
		time.Sleep(hiccupTick)
		t := time.Now()
		late = wokeLate(late, t.Sub(now)-hiccupTick)
		now = t
	}
	return late
}

const hiccupTick = time.Millisecond

// wokeLate appends a wakeup that came d late and, as jHiccup does, the
// wakeups the pause swallowed, each one tick less late, down to one tick:
// so the percentiles weigh a pause by how long it lasted.
func wokeLate(late []int64, d time.Duration) []int64 {
	late = append(late, int64(d))
	for m := d - hiccupTick; m >= hiccupTick; m -= hiccupTick {
		late = append(late, int64(m))
	}
	return late
}

// subscribe opens the stream and subscribes to the symbol's trades.
func (l *loader) subscribe(ctx context.Context) (*websocket.Conn, error) {
	c, _, err := websocket.Dial(ctx, "ws://"+l.cfg.addr+"/v1/stream", nil)
	if err != nil {
		return nil, err
	}
	msg, _ := json.Marshal(map[string]any{"op": "subscribe", "channels": []string{"trades:" + l.cfg.symbol}})
	if err := c.Write(ctx, websocket.MessageText, msg); err != nil {
		_ = c.CloseNow()
		return nil, err
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		_ = c.CloseNow()
		return nil, err
	}
	var m struct{ Type string }
	if err := json.Unmarshal(data, &m); err != nil || m.Type != "subscribed" {
		_ = c.CloseNow()
		return nil, fmt.Errorf("got %s, want subscribed", data)
	}
	return c, nil
}

// readTrades notes when the first trade push for each taker order arrives,
// until the stream ends. It returns nil if the stream ended because the
// run closed it.
func (l *loader) readTrades(c *websocket.Conn, closing *atomic.Bool) error {
	var last uint64
	for {
		_, data, err := c.Read(context.Background())
		at := time.Now()
		if err != nil {
			if closing.Load() {
				return nil
			}
			return err
		}
		var m struct {
			Type          string
			TakerOrderID  uint64 `json:"taker_order_id"`
			Code, Message string
		}
		if err := json.Unmarshal(data, &m); err != nil {
			return err
		}
		switch m.Type {
		case "trade":
			if m.TakerOrderID != last { // one order's trades come together
				last = m.TakerOrderID
				l.join.push(m.TakerOrderID, at)
			}
		case "error":
			return fmt.Errorf("stream error %s: %s", m.Code, m.Message)
		}
	}
}
