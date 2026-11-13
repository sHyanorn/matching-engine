package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

const (
	maxClientMessage = 4 << 10
	outQueue         = 256     // messages waiting for the connection's writer
	maxQueuedBytes   = 1 << 20 // bytes waiting for the writer, apart from the largest message
)

type clientMessage struct {
	Op       string   `json:"op"`
	Channels []string `json:"channels"`
}

type levelChange struct {
	Side   string `json:"side"`
	Price  int64  `json:"price"`
	Qty    int64  `json:"qty"`
	Orders uint32 `json:"orders"`
}

// serverMessage is every message the server sends; fields a type does not
// use are omitted.
type serverMessage struct {
	Type    string        `json:"type"`
	Channel string        `json:"channel,omitempty"`
	Seq     uint64        `json:"seq,omitempty"`
	BookSeq *uint64       `json:"book_seq,omitempty"`
	Bids    *[]levelJSON  `json:"bids,omitempty"` // pointers: an empty side is [], not omitted
	Asks    *[]levelJSON  `json:"asks,omitempty"`
	Changes []levelChange `json:"changes,omitempty"`
	// trade
	Price        int64        `json:"price,omitempty"`
	Qty          int64        `json:"qty,omitempty"`
	TakerSide    string       `json:"taker_side,omitempty"`
	TakerOrderID book.OrderID `json:"taker_order_id,omitempty"`
	MakerOrderID book.OrderID `json:"maker_order_id,omitempty"`
	TS           int64        `json:"ts,omitempty"`
	// error
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// closeCause is why a connection ends: its close code and reason, and the
// label it is counted under in matching_engine_ws_disconnects_total.
type closeCause struct {
	code   websocket.StatusCode
	reason string
	label  string
}

var (
	closedByClient = closeCause{websocket.StatusNormalClosure, "", "client"}
	shuttingDown   = closeCause{websocket.StatusGoingAway, "shutting down", "shutdown"}
	writeFailed    = closeCause{websocket.StatusInternalError, "write failed", "write_failed"}
	// A slow consumer: its engine subscription filled, its unsent
	// messages, apart from the largest, passed maxQueuedBytes, or writing
	// one message took longer than the write timeout.
	slowSubscription = closeCause{websocket.StatusPolicyViolation, "slow consumer", "slow_consumer_updates"}
	slowBacklog      = closeCause{websocket.StatusPolicyViolation, "slow consumer", "slow_consumer_bytes"}
	slowWrite        = closeCause{websocket.StatusPolicyViolation, "slow consumer", "slow_consumer_write"}

	closeCauses = []closeCause{closedByClient, shuttingDown, writeFailed, slowSubscription, slowBacklog, slowWrite}
)

// stream is one WebSocket connection. A reader handles subscriptions, a
// writer sends queued messages, and each subscribed channel has a pump
// that turns engine updates into messages.
type stream struct {
	srv        *Server
	conn       *websocket.Conn
	ctx        context.Context
	cancel     context.CancelFunc
	out        chan []byte
	queued     backlog       // the messages in out
	writerDone chan struct{} // closed when the writer has stopped
	once       sync.Once     // close

	// channels is touched only by the handler's goroutine: the reader, and
	// the cleanup after it.
	channels map[string]*channel
}

// channel is one subscription of a stream.
type channel struct {
	name string
	sub  *engine.Subscription
	done chan struct{} // closed when its pump has stopped
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	if s.stopping.Load() {
		writeError(w, http.StatusServiceUnavailable, "shutting_down", "server is shutting down")
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept has written the HTTP error
	}
	conn.SetReadLimit(maxClientMessage)
	ctx, cancel := context.WithCancel(context.Background())
	st := &stream{
		srv: s, conn: conn, ctx: ctx, cancel: cancel,
		out: make(chan []byte, outQueue), writerDone: make(chan struct{}), channels: map[string]*channel{},
	}

	s.mu.Lock()
	if s.stopping.Load() { // CloseStreams may have run since the check above
		s.mu.Unlock()
		close(st.writerDone) // no writer was started
		st.close(shuttingDown)
		return
	}
	s.streams[st] = struct{}{}
	s.wg.Add(1)
	s.mu.Unlock()
	s.metrics.streams.Inc()
	defer func() {
		s.mu.Lock()
		delete(s.streams, st)
		s.mu.Unlock()
		s.metrics.streams.Dec()
		s.wg.Done()
	}()

	go st.writeLoop()
	st.readLoop()
	st.close(closedByClient)
	for _, ch := range st.channels {
		ch.sub.Close()
		<-ch.done
	}
}

// close ends the connection once, for cause: it counts the cause, stops
// the writer and the pumps, then, off every caller's path, lets a message
// being written finish (for up to the write timeout) and runs the close
// handshake, which the library bounds too.
//
// Reads and writes never use a context that can end: the library drops
// the connection without a close frame when an operation's context ends.
func (st *stream) close(cause closeCause) {
	st.once.Do(func() {
		st.srv.metrics.wsClosed.WithLabelValues(cause.label).Inc()
		st.cancel()
		go func() {
			select {
			case <-st.writerDone:
			case <-time.After(st.srv.opts.WriteTimeout):
			}
			_ = st.conn.Close(cause.code, cause.reason)
		}()
	})
}

// readLoop handles client messages until the connection ends.
func (st *stream) readLoop() {
	for {
		_, data, err := st.conn.Read(context.Background())
		if err != nil {
			return
		}
		var m clientMessage
		if err := json.Unmarshal(data, &m); err != nil {
			st.sendError("bad_request", err.Error())
			continue
		}
		switch m.Op {
		case "subscribe":
			for _, name := range m.Channels {
				st.subscribe(name)
			}
		case "unsubscribe":
			for _, name := range m.Channels {
				st.unsubscribe(name)
			}
		default:
			st.sendError("unknown_op", fmt.Sprintf("unknown op %q", m.Op))
		}
	}
}

// writeLoop writes queued messages. A message that takes longer than the
// write timeout marks the client as a slow consumer.
func (st *stream) writeLoop() {
	defer close(st.writerDone)
	for {
		select {
		case msg := <-st.out:
			st.queued.remove(len(msg))
			if st.ctx.Err() != nil {
				return
			}
			slow := time.AfterFunc(st.srv.opts.WriteTimeout, func() { st.close(slowWrite) })
			err := st.conn.Write(context.Background(), websocket.MessageText, msg)
			slow.Stop()
			if err != nil {
				st.close(writeFailed) // the connection is gone
				return
			}
		case <-st.ctx.Done():
			return
		}
	}
}

// send queues msg, waiting while the writer is behind. A pump blocked
// here lets its engine subscription fill, so a client that does not read
// is disconnected as a slow consumer; so is one whose queued messages,
// apart from the largest, pass maxQueuedBytes.
func (st *stream) send(msg serverMessage) bool {
	data, err := json.Marshal(msg)
	if err != nil {
		return false
	}
	if !st.queued.add(len(data)) {
		st.close(slowBacklog)
		return false
	}
	select {
	case st.out <- data:
		return true
	case <-st.ctx.Done():
		st.queued.remove(len(data))
		return false
	}
}

// backlog holds the sizes of the messages queued for a connection's
// writer. Leaving the largest out of the bound lets a message of any size
// through to a client that reads, while a client that does not read holds
// at most maxQueuedBytes plus one message in the queue.
type backlog struct {
	mu    sync.Mutex
	total int
	sizes []int
}

// add records a message of n bytes, unless the queued messages, apart from
// the largest, would then pass maxQueuedBytes.
func (b *backlog) add(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if total := b.total + n; total > maxQueuedBytes {
		largest := n
		for _, size := range b.sizes {
			largest = max(largest, size)
		}
		if total-largest > maxQueuedBytes {
			return false
		}
	}
	b.total += n
	b.sizes = append(b.sizes, n)
	return true
}

// remove forgets a message of n bytes that add recorded.
func (b *backlog) remove(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	i := slices.Index(b.sizes, n)
	b.sizes[i] = b.sizes[len(b.sizes)-1]
	b.sizes = b.sizes[:len(b.sizes)-1]
	b.total -= n
}

func (st *stream) sendError(code, message string) {
	st.send(serverMessage{Type: "error", Code: code, Message: message})
}

// subscribe starts channel name, "trades:SYMBOL" or "book:SYMBOL".
func (st *stream) subscribe(name string) {
	kind, symbol, _ := strings.Cut(name, ":")
	if kind != "trades" && kind != "book" {
		st.sendError("unknown_channel", fmt.Sprintf("unknown channel %q", name))
		return
	}
	if _, dup := st.channels[name]; dup {
		st.sendError("already_subscribed", fmt.Sprintf("already subscribed to %q", name))
		return
	}
	depth := 0
	if kind == "book" {
		depth = math.MaxInt // the whole book, which the deltas then keep current
	}
	sub, err := st.srv.eng.Subscribe(st.ctx, symbol, st.srv.opts.SubscriberBuffer, depth)
	switch {
	case errors.Is(err, engine.ErrUnknownSymbol):
		st.sendError("unknown_channel", fmt.Sprintf("unknown channel %q", name))
		return
	case err != nil:
		_, code := engineStatus(err)
		st.sendError(code, err.Error())
		return
	}
	ch := &channel{name: name, sub: sub, done: make(chan struct{})}
	st.channels[name] = ch
	st.send(serverMessage{Type: "subscribed", Channel: name})
	go st.pump(ch, kind == "book")
}

// unsubscribe stops channel name; the acknowledgement follows the
// channel's last message.
func (st *stream) unsubscribe(name string) {
	ch, ok := st.channels[name]
	if !ok {
		st.sendError("not_subscribed", fmt.Sprintf("not subscribed to %q", name))
		return
	}
	delete(st.channels, name)
	ch.sub.Close()
	<-ch.done
	st.send(serverMessage{Type: "unsubscribed", Channel: name})
}

// pump sends one channel's messages, numbered from 1.
func (st *stream) pump(ch *channel, isBook bool) {
	defer close(ch.done)
	var seq uint64
	next := func() uint64 { seq++; return seq }
	if isBook {
		snap := ch.sub.Snapshot
		bookSeq := snap.LastSeq
		bids, asks := toLevelsJSON(snap.Bids), toLevelsJSON(snap.Asks)
		if !st.send(serverMessage{Type: "book_snapshot", Channel: ch.name, Seq: next(), BookSeq: &bookSeq, Bids: &bids, Asks: &asks}) {
			return
		}
	}
	for u := range ch.sub.C {
		if isBook {
			if msg, ok := bookDelta(ch.name, u.Events); ok {
				msg.Seq = next()
				if !st.send(msg) {
					return
				}
			}
			continue
		}
		for _, e := range u.Events { // one message at a time: a sweep can hold many trades
			if e.Kind == book.EventTrade {
				msg := tradeMessage(ch.name, e)
				msg.Seq = next()
				if !st.send(msg) {
					return
				}
			}
		}
	}
	switch err := ch.sub.Err(); {
	case errors.Is(err, engine.ErrSlowConsumer):
		st.close(slowSubscription)
	case errors.Is(err, engine.ErrClosed):
		st.close(shuttingDown)
	}
}

// tradeMessage is the message of one execution.
func tradeMessage(name string, e book.Event) serverMessage {
	bookSeq := e.Seq
	return serverMessage{
		Type: "trade", Channel: name, BookSeq: &bookSeq, Price: int64(e.Price), Qty: int64(e.Qty),
		TakerSide: e.Side.String(), TakerOrderID: e.OrderID, MakerOrderID: e.MakerID, TS: e.Timestamp,
	}
}

// bookDelta is the book_delta of one command's events: every level the
// command changed, at its final total. ok is false if no level changed.
func bookDelta(name string, evs []book.Event) (msg serverMessage, ok bool) {
	var changes []levelChange
	at := map[book.LevelUpdate]int{} // keyed by side and price, with Qty and Orders 0
	for _, e := range evs {
		lu, changed := e.LevelUpdate()
		if !changed {
			continue
		}
		key := book.LevelUpdate{Side: lu.Side, Price: lu.Price}
		if i, seen := at[key]; seen {
			changes[i].Qty, changes[i].Orders = int64(lu.Qty), lu.Orders
			continue
		}
		at[key] = len(changes)
		changes = append(changes, levelChange{Side: lu.Side.String(), Price: int64(lu.Price), Qty: int64(lu.Qty), Orders: lu.Orders})
	}
	if len(changes) == 0 {
		return serverMessage{}, false
	}
	bookSeq := evs[len(evs)-1].Seq
	return serverMessage{Type: "book_delta", Channel: name, BookSeq: &bookSeq, Changes: changes}, true
}
