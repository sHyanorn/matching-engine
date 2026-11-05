package api_test

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sHyanorn/matching-engine/internal/api"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

// These tests drive the real HTTP handler, backed by a real engine on a
// temporary directory, and check the wire contract of docs/API.md.

const (
	btc = "BTC-USD"
	eth = "ETH-USD"

	tradesBTC = "trades:" + btc
	bookBTC   = "book:" + btc

	// baseTS is the epoch of the test clock: command n is stamped
	// baseTS + n*1000.
	baseTS int64 = 1_730_000_000_000_000_000

	// waitFor bounds every wait for something asynchronous.
	waitFor = 10 * time.Second
)

// ---------------------------------------------------------------------------
// Test environment

// testClock is the engine's Config.Clock. It returns strictly increasing
// timestamps and can stall the book goroutine that calls it next: the
// engine calls Clock once per command on the symbol's goroutine, so a stall
// holds that goroutine in the middle of a batch while its queue fills up.
type testClock struct {
	n atomic.Int64

	mu       sync.Mutex
	armed    bool
	entered  chan struct{}
	release  chan struct{}
	releases []func()
}

func (c *testClock) now() int64 {
	c.mu.Lock()
	stall, entered, release := c.armed, c.entered, c.release
	c.armed = false
	c.mu.Unlock()
	if stall {
		close(entered)
		<-release
	}
	return baseTS + c.n.Add(1)*1000
}

// stall makes the next call to now block until release is called. entered
// is closed once a book goroutine is blocked in the clock; from then on
// nothing leaves that symbol's queue.
func (c *testClock) stall() (entered <-chan struct{}, release func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = true
	c.entered = make(chan struct{})
	ch := make(chan struct{})
	c.release = ch
	var once sync.Once
	release = func() { once.Do(func() { close(ch) }) }
	c.releases = append(c.releases, release)
	return c.entered, release
}

// releaseAll disarms the clock and ends every stall, so that cleanup never
// waits on a stalled book.
func (c *testClock) releaseAll() {
	c.mu.Lock()
	c.armed = false
	rs := slices.Clone(c.releases)
	c.mu.Unlock()
	for _, r := range rs {
		r()
	}
}

type envConfig struct {
	queueSize        int           // engine.Config.QueueSize; 0 for the default
	subscriberBuffer int           // api.Options.SubscriberBuffer; 0 for the default
	writeTimeout     time.Duration // api.Options.WriteTimeout; 0 for the default
	listener         net.Listener  // for the HTTP server; nil for a default one
}

// testEnv is an engine serving BTC-USD and ETH-USD behind an api.Server on
// an httptest.Server.
type testEnv struct {
	eng   *engine.Engine
	srv   *api.Server
	http  *httptest.Server
	clock *testClock
}

func newEnv(t *testing.T, cfg envConfig) *testEnv {
	t.Helper()
	clock := &testClock{}
	fsync := api.NewFsyncTimes()
	eng, err := engine.New(engine.Config{
		Symbols:   []string{btc, eth},
		DataDir:   t.TempDir(),
		QueueSize: cfg.queueSize,
		Clock:     clock.now,
		OnSync:    fsync.Observe,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() {
		if err := eng.Close(); err != nil {
			t.Errorf("engine.Close: %v", err)
		}
	})
	srv := api.New(eng, api.Options{SubscriberBuffer: cfg.subscriberBuffer, WriteTimeout: cfg.writeTimeout, FsyncTimes: fsync})
	hs := httptest.NewUnstartedServer(srv.Handler())
	if cfg.listener != nil {
		_ = hs.Listener.Close() // replaced; nothing listens on it yet
		hs.Listener = cfg.listener
	}
	hs.Start()
	t.Cleanup(hs.Close)
	hs.Client().Transport.(*http.Transport).DialContext = redial
	// Cleanups run last first: end any stall, then close the WebSocket
	// connections (httptest.Server.Close does not wait for hijacked ones),
	// then the HTTP server, then the engine.
	t.Cleanup(srv.CloseStreams)
	t.Cleanup(clock.releaseAll)
	return &testEnv{eng: eng, srv: srv, http: hs, clock: clock}
}

// redial dials, retrying a refused connection for a while: Windows refuses
// connections beyond its listen backlog at once, where Linux makes the
// client retry, and some tests open hundreds at a time.
func redial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	for deadline := time.Now().Add(waitFor); ; time.Sleep(10 * time.Millisecond) {
		c, err := d.DialContext(ctx, network, addr)
		if err == nil || ctx.Err() != nil || time.Now().After(deadline) {
			return c, err
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP helpers

type httpResult struct {
	status int
	header http.Header
	body   []byte
}

// send makes one request. It never touches t, so any goroutine may call it.
// The request, body included, is bounded by 3*waitFor, so a handler that
// never answers fails the test instead of hanging it; that is well above
// any wait a test step expects.
func (e *testEnv) send(method, path, body string) (httpResult, error) {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*waitFor)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, e.http.URL+path, r)
	if err != nil {
		return httpResult{}, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.http.Client().Do(req)
	if err != nil {
		return httpResult{}, err
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close() // the read error, if any, is the one to report
	if err != nil {
		return httpResult{}, err
	}
	return httpResult{status: resp.StatusCode, header: resp.Header, body: b}, nil
}

func (e *testEnv) do(t *testing.T, method, path, body string) httpResult {
	t.Helper()
	res, err := e.send(method, path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return res
}

func orderPath(id uint64) string { return "/v1/orders/" + strconv.FormatUint(id, 10) }

func requireJSON(t *testing.T, res httpResult) {
	t.Helper()
	ct := res.header.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
}

// decodeStrict decodes exactly one JSON value into v, rejecting fields that
// v does not declare.
func decodeStrict(t *testing.T, data []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decoding %s: %v", data, err)
	}
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		t.Fatalf("trailing data after the JSON value in %s", data)
	}
}

// fields decodes a JSON object into its raw fields.
func fields(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decoding %s: %v", data, err)
	}
	return m
}

func requireFields(t *testing.T, what string, got map[string]json.RawMessage, want ...string) {
	t.Helper()
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("%s lacks field %q: %v", what, k, slices.Sorted(maps.Keys(got)))
		}
	}
	for k := range got {
		if !slices.Contains(want, k) {
			t.Errorf("%s has undocumented field %q", what, k)
		}
	}
}

// expectError checks an error reply: its status and the documented shape
// {"error":{"code":...,"message":...}} with one of the given codes. More
// than one code is accepted only where docs/API.md leaves the choice open.
func expectError(t *testing.T, res httpResult, status int, codes ...string) {
	t.Helper()
	if res.status != status {
		t.Fatalf("status %d, want %d (%s); body %s", res.status, status, strings.Join(codes, " or "), res.body)
	}
	requireJSON(t, res)
	top := fields(t, res.body)
	requireFields(t, "error reply", top, "error")
	requireFields(t, "error object", fields(t, top["error"]), "code", "message")
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeStrict(t, res.body, &e)
	if !slices.Contains(codes, e.Error.Code) {
		t.Errorf("error code %q, want %s; body %s", e.Error.Code, strings.Join(codes, " or "), res.body)
	}
	if e.Error.Message == "" {
		t.Errorf("error %q has an empty message", e.Error.Code)
	}
}

// receive waits for one value from ch.
func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitFor):
		t.Fatalf("no %s within %v", what, waitFor)
		var zero T
		return zero
	}
}

// ---------------------------------------------------------------------------
// Orders

// order is the JSON body of POST /v1/orders.
type order map[string]any

func limitOrder(side string, price, qty int64) order {
	return order{"symbol": btc, "side": side, "type": "limit", "price": price, "qty": qty}
}

// marketOrder has neither price nor tif, so both take their defaults.
func marketOrder(side string, qty int64) order {
	return order{"symbol": btc, "side": side, "type": "market", "qty": qty}
}

// with returns a copy of o with field k set to v.
func (o order) with(k string, v any) order {
	c := maps.Clone(o)
	c[k] = v
	return c
}

func (o order) body() string {
	b, err := json.Marshal(o)
	if err != nil {
		panic(err) // a map of strings and integers always marshals
	}
	return string(b)
}

// orderStatus is the documented order status object.
type orderStatus struct {
	OrderID      uint64 `json:"order_id"`
	Symbol       string `json:"symbol"`
	ClientID     string `json:"client_id"`
	Side         string `json:"side"`
	Type         string `json:"type"`
	TIF          string `json:"tif"`
	Price        int64  `json:"price"`
	Qty          int64  `json:"qty"`
	FilledQty    int64  `json:"filled_qty"`
	RemainingQty int64  `json:"remaining_qty"`
	Status       string `json:"status"`
	Reason       string `json:"reason"`
	TS           int64  `json:"ts"`
}

// event is one entry of a reply's events array: the fields of book.Event.
type event struct {
	Seq       uint64 `json:"seq"`
	TS        int64  `json:"ts"`
	Type      string `json:"type"`
	OrderID   uint64 `json:"order_id"`
	MakerID   uint64 `json:"maker_id"`
	Side      string `json:"side"`
	Price     int64  `json:"price"`
	Qty       int64  `json:"qty"`
	Reason    string `json:"reason"`
	LevelSide string `json:"level_side"`
	LevelQty  int64  `json:"level_qty"`
}

// orderReply is the reply to POST /v1/orders and DELETE /v1/orders/{id}.
type orderReply struct {
	Order  orderStatus `json:"order"`
	Events []event     `json:"events"`

	rawEvents []map[string]json.RawMessage
}

var orderFields = []string{
	"order_id", "symbol", "client_id", "side", "type", "tif", "price",
	"qty", "filled_qty", "remaining_qty", "status", "reason", "ts",
}

// checkOrderFields requires every documented order status field;
// client_id may be left out when the order had none.
func checkOrderFields(t *testing.T, raw map[string]json.RawMessage) {
	t.Helper()
	want := orderFields
	if _, ok := raw["client_id"]; !ok {
		want = slices.DeleteFunc(slices.Clone(want), func(k string) bool { return k == "client_id" })
	}
	requireFields(t, "order status", raw, want...)
}

func decodeOrderReply(t *testing.T, res httpResult) orderReply {
	t.Helper()
	if res.status != http.StatusOK {
		t.Fatalf("status %d, want 200; body %s", res.status, res.body)
	}
	requireJSON(t, res)
	top := fields(t, res.body)
	requireFields(t, "order reply", top, "order", "events")
	checkOrderFields(t, fields(t, top["order"]))
	var r orderReply
	if err := json.Unmarshal(top["events"], &r.rawEvents); err != nil {
		t.Fatalf("decoding events %s: %v", top["events"], err)
	}
	decodeStrict(t, res.body, &r)
	return r
}

func (e *testEnv) place(t *testing.T, o order) orderReply {
	t.Helper()
	return decodeOrderReply(t, e.do(t, http.MethodPost, "/v1/orders", o.body()))
}

func (e *testEnv) cancel(t *testing.T, id uint64) orderReply {
	t.Helper()
	return decodeOrderReply(t, e.do(t, http.MethodDelete, orderPath(id), ""))
}

func (e *testEnv) getOrder(t *testing.T, id uint64) orderStatus {
	t.Helper()
	res := e.do(t, http.MethodGet, orderPath(id), "")
	if res.status != http.StatusOK {
		t.Fatalf("GET order %d: status %d, want 200; body %s", id, res.status, res.body)
	}
	requireJSON(t, res)
	checkOrderFields(t, fields(t, res.body))
	var s orderStatus
	decodeStrict(t, res.body, &s)
	return s
}

func checkStatus(t *testing.T, got, want orderStatus) {
	t.Helper()
	if got != want {
		t.Errorf("order status\n got %+v\nwant %+v", got, want)
	}
}

// checkEvents checks r's events against want, whose Seq and TS are left
// zero: the events must carry Seq firstSeq, firstSeq+1, ... and all the
// same TS, which must equal ts unless ts is 0. It also checks which fields
// each event carries.
func checkEvents(t *testing.T, r orderReply, firstSeq uint64, ts int64, want ...event) {
	t.Helper()
	if len(r.Events) != len(want) {
		t.Fatalf("%d events %+v, want %d %+v", len(r.Events), r.Events, len(want), want)
	}
	if ts == 0 {
		ts = r.Events[0].TS
		if ts <= 0 {
			t.Errorf("event ts %d, want a Unix nanosecond timestamp", ts)
		}
	}
	for i, got := range r.Events {
		if got.Seq != firstSeq+uint64(i) {
			t.Errorf("event %d (%s): seq %d, want %d", i, got.Type, got.Seq, firstSeq+uint64(i))
		}
		if got.TS != ts {
			t.Errorf("event %d (%s): ts %d, want %d", i, got.Type, got.TS, ts)
		}
		got.Seq, got.TS = 0, 0
		if got != want[i] {
			t.Errorf("event %d\n got %+v\nwant %+v", i, got, want[i])
		}
		checkEventFields(t, i, r.rawEvents[i], want[i])
	}
}

// checkEventFields checks that an event carries exactly the fields its
// type uses (docs/API.md, book.Event): unused fields are omitted, and
// level_qty is present whenever level_side is, even when it is 0. A market
// order's price is 0, so its accepted and canceled events may either omit
// price or send 0.
func checkEventFields(t *testing.T, i int, raw map[string]json.RawMessage, w event) {
	t.Helper()
	required := []string{"seq", "ts", "type", "order_id"}
	var zeroOK []string
	priceField := func() {
		if w.Price != 0 {
			required = append(required, "price")
		} else {
			zeroOK = append(zeroOK, "price")
		}
	}
	switch w.Type {
	case "accepted":
		required = append(required, "side", "qty")
		priceField()
	case "rejected":
		required = append(required, "reason")
	case "trade":
		required = append(required, "maker_id", "side", "price", "qty", "level_side", "level_qty")
	case "rested":
		required = append(required, "side", "price", "qty", "level_side", "level_qty")
	case "canceled":
		required = append(required, "side", "qty", "reason")
		priceField()
		if w.LevelSide != "" {
			required = append(required, "level_side", "level_qty")
		}
	default:
		t.Fatalf("unknown event type %q in test", w.Type)
	}
	for _, k := range required {
		if _, ok := raw[k]; !ok {
			t.Errorf("event %d (%s) lacks field %q", i, w.Type, k)
		}
	}
	for k, v := range raw {
		if slices.Contains(required, k) || slices.Contains(zeroOK, k) && string(v) == "0" {
			continue
		}
		t.Errorf("event %d (%s) has field %q (%s), which its type does not use", i, w.Type, k, v)
	}
}

// ---------------------------------------------------------------------------
// Books

type level struct {
	Price  int64 `json:"price"`
	Qty    int64 `json:"qty"`
	Orders int   `json:"orders"`
}

type bookReply struct {
	Symbol string  `json:"symbol"`
	Seq    uint64  `json:"seq"`
	Bids   []level `json:"bids"`
	Asks   []level `json:"asks"`
}

func (e *testEnv) book(t *testing.T, symbol, query string) bookReply {
	t.Helper()
	res := e.do(t, http.MethodGet, "/v1/books/"+symbol+query, "")
	if res.status != http.StatusOK {
		t.Fatalf("GET book %s%s: status %d, want 200; body %s", symbol, query, res.status, res.body)
	}
	requireJSON(t, res)
	requireFields(t, "book", fields(t, res.body), "symbol", "seq", "bids", "asks")
	var raw struct {
		Bids []map[string]json.RawMessage `json:"bids"`
		Asks []map[string]json.RawMessage `json:"asks"`
	}
	if err := json.Unmarshal(res.body, &raw); err != nil {
		t.Fatalf("decoding %s: %v", res.body, err)
	}
	for _, l := range append(raw.Bids, raw.Asks...) {
		requireFields(t, "book level", l, "price", "qty", "orders")
	}
	var b bookReply
	decodeStrict(t, res.body, &b)
	if b.Symbol != symbol {
		t.Errorf("book symbol %q, want %q", b.Symbol, symbol)
	}
	return b
}

func checkLevels(t *testing.T, what string, got, want []level) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s\n got %+v\nwant %+v", what, got, want)
	}
}

// ---------------------------------------------------------------------------
// Metrics

// metricSet is a parsed Prometheus text exposition.
type metricSet struct {
	types  map[string]string  // family name -> type, from # TYPE lines
	values map[string]float64 // sampleKey -> value
}

// sampleKey names one sample: the metric name and its labels, given as
// name/value pairs in any order.
func sampleKey(name string, labels ...string) string {
	var pairs []string
	for i := 0; i+1 < len(labels); i += 2 {
		pairs = append(pairs, labels[i]+"="+strconv.Quote(labels[i+1]))
	}
	slices.Sort(pairs)
	return name + "{" + strings.Join(pairs, ",") + "}"
}

// expectDisconnects checks how many WebSocket connections were closed for
// the cause counted as reason.
func expectDisconnects(t *testing.T, env *testEnv, reason string, want float64) {
	t.Helper()
	if got := env.metrics(t).value("matching_engine_ws_disconnects_total", "reason", reason); got != want {
		t.Errorf("matching_engine_ws_disconnects_total{reason=%q} = %v, want %v", reason, got, want)
	}
}

// value returns a sample's value; a counter that was never incremented may
// be absent, which reads as 0.
func (m metricSet) value(name string, labels ...string) float64 {
	return m.values[sampleKey(name, labels...)]
}

// parseSample splits a sample line such as `name{a="x",b="y"} 3` into its
// key and value.
func parseSample(line string) (string, float64, error) {
	name, rest := line, ""
	if i := strings.IndexAny(line, "{ "); i >= 0 {
		name, rest = line[:i], line[i:]
	}
	var labels []string
	if strings.HasPrefix(rest, "{") {
		rest = rest[1:]
		for !strings.HasPrefix(rest, "}") {
			eq := strings.IndexByte(rest, '=')
			if eq < 0 {
				return "", 0, fmt.Errorf("bad labels in %q", line)
			}
			k := strings.TrimSpace(rest[:eq])
			q, err := strconv.QuotedPrefix(rest[eq+1:])
			if err != nil {
				return "", 0, fmt.Errorf("bad label value in %q: %w", line, err)
			}
			v, err := strconv.Unquote(q)
			if err != nil {
				return "", 0, fmt.Errorf("bad label value in %q: %w", line, err)
			}
			labels = append(labels, k, v)
			rest = strings.TrimPrefix(rest[eq+1+len(q):], ",")
		}
		rest = rest[1:]
	}
	f := strings.Fields(rest)
	if len(f) == 0 {
		return "", 0, fmt.Errorf("no value in %q", line)
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return "", 0, fmt.Errorf("bad value in %q: %w", line, err)
	}
	return sampleKey(name, labels...), v, nil
}

func (e *testEnv) metrics(t *testing.T) metricSet {
	t.Helper()
	res := e.do(t, http.MethodGet, "/metrics", "")
	if res.status != http.StatusOK {
		t.Fatalf("GET /metrics: status %d; body %s", res.status, res.body)
	}
	m := metricSet{types: map[string]string{}, values: map[string]float64{}}
	for line := range strings.SplitSeq(string(res.body), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "# TYPE "):
			if f := strings.Fields(line); len(f) == 4 {
				m.types[f[2]] = f[3]
			}
		case strings.HasPrefix(line, "#"):
		default:
			k, v, err := parseSample(line)
			if err != nil {
				t.Fatalf("parsing /metrics: %v", err)
			}
			m.values[k] = v
		}
	}
	return m
}

// rise is how much one counter sample should have grown.
type rise struct {
	name   string
	labels []string
	by     float64
}

// metricsAfter waits until every counter in rises has grown by at least its
// amount since before, then checks that each grew by exactly that amount,
// and returns the last scrape. A counter may be fed from the engine's event
// bus (SPEC §4) rather than by the handler, so it can lag the HTTP reply
// that caused it; the wait is bounded by waitFor.
func (e *testEnv) metricsAfter(t *testing.T, before metricSet, rises ...rise) metricSet {
	t.Helper()
	grown := func(m metricSet, r rise) float64 {
		return m.value(r.name, r.labels...) - before.value(r.name, r.labels...)
	}
	deadline := time.Now().Add(waitFor)
	for {
		m := e.metrics(t)
		done := true
		for _, r := range rises {
			if grown(m, r) < r.by {
				done = false
			}
		}
		if done || time.Now().After(deadline) {
			for _, r := range rises {
				if got := grown(m, r); got != r.by {
					t.Errorf("%s %v rose by %v, want %v", r.name, r.labels, got, r.by)
				}
			}
			return m
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// WebSocket helpers

type change struct {
	Side  string `json:"side"`
	Price int64  `json:"price"`
	Qty   int64  `json:"qty"`
}

// streamMsg holds the fields of every server message type.
type streamMsg struct {
	Type         string   `json:"type"`
	Channel      string   `json:"channel"`
	Seq          uint64   `json:"seq"`
	BookSeq      uint64   `json:"book_seq"`
	Bids         []level  `json:"bids"`
	Asks         []level  `json:"asks"`
	Changes      []change `json:"changes"`
	Price        int64    `json:"price"`
	Qty          int64    `json:"qty"`
	TakerSide    string   `json:"taker_side"`
	TakerOrderID uint64   `json:"taker_order_id"`
	MakerOrderID uint64   `json:"maker_order_id"`
	TS           int64    `json:"ts"`
	Code         string   `json:"code"`
	Message      string   `json:"message"`

	keys map[string]json.RawMessage
	raw  string
}

func parseStreamMsg(data []byte) streamMsg {
	var m streamMsg
	if err := json.Unmarshal(data, &m); err != nil {
		m.Type = "<not JSON: " + err.Error() + ">"
	}
	_ = json.Unmarshal(data, &m.keys) // a decode error is already in m.Type
	m.raw = string(data)
	return m
}

// isData reports whether a message type counts in its channel's seq.
func isData(typ string) bool {
	return typ == "book_snapshot" || typ == "book_delta" || typ == "trade"
}

func (e *testEnv) streamURL() string {
	return "ws" + strings.TrimPrefix(e.http.URL, "http") + "/v1/stream"
}

// dial opens a WebSocket connection that is closed when the test ends.
func (e *testEnv) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	// The context bounds only the handshake; it is not canceled before
	// the test ends, in case the transport ties the connection to it.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	timer := time.AfterFunc(waitFor, cancel)
	conn, _, err := websocket.Dial(ctx, e.streamURL(), nil)
	timer.Stop()
	if err != nil {
		t.Fatalf("dialing %s: %v", e.streamURL(), err)
	}
	conn.SetReadLimit(1 << 20) // ample for the books these tests build
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func writeMsg(t *testing.T, conn *websocket.Conn, msg string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		t.Fatalf("writing %s: %v", msg, err)
	}
}

// readMsg reads one message directly, for a client that has no reader
// goroutine.
func readMsg(t *testing.T, conn *websocket.Conn) streamMsg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	return parseStreamMsg(data)
}

// stream is a WebSocket client whose reader goroutine queues every server
// message. Tests wait on the queue instead of using read deadlines, which
// would close the connection, and can see what arrived in between.
type stream struct {
	conn    *websocket.Conn
	in      chan streamMsg
	err     error       // why reading stopped; set before in is closed
	pending []streamMsg // received, not yet taken, in arrival order
	lastSeq map[string]uint64
	acked   map[string]bool // channels whose subscribed has arrived
}

func newStream(conn *websocket.Conn) *stream {
	s := &stream{
		conn: conn, in: make(chan streamMsg, 4096),
		lastSeq: map[string]uint64{}, acked: map[string]bool{},
	}
	go func() {
		defer close(s.in)
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				s.err = err
				return
			}
			s.in <- parseStreamMsg(data)
		}
	}()
	return s
}

func (e *testEnv) stream(t *testing.T) *stream {
	t.Helper()
	return newStream(e.dial(t))
}

func (s *stream) send(t *testing.T, msg string) {
	t.Helper()
	writeMsg(t, s.conn, msg)
}

func describe(msgs []streamMsg) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString("\n  " + m.raw)
	}
	return b.String()
}

// next takes the first message, in arrival order, that match accepts,
// waiting for it if needed; other messages stay queued.
func (s *stream) next(t *testing.T, what string, match func(streamMsg) bool) streamMsg {
	t.Helper()
	for i, m := range s.pending {
		if match(m) {
			s.pending = slices.Delete(s.pending, i, i+1)
			s.check(t, m)
			return m
		}
	}
	timeout := time.NewTimer(waitFor)
	defer timeout.Stop()
	for {
		select {
		case m, ok := <-s.in:
			if !ok {
				t.Fatalf("connection ended while waiting for %s: %v; unmatched:%s", what, s.err, describe(s.pending))
			}
			s.arrived(t, m)
			if match(m) {
				s.check(t, m)
				return m
			}
			s.pending = append(s.pending, m)
		case <-timeout.C:
			t.Fatalf("no %s within %v; unmatched:%s", what, waitFor, describe(s.pending))
		}
	}
}

// arrived checks what docs/API.md says about arrival order, which next
// cannot see once messages are taken out of order: a channel's subscribed
// acknowledgement comes before its first data message.
func (s *stream) arrived(t *testing.T, m streamMsg) {
	t.Helper()
	switch {
	case m.Type == "subscribed":
		s.acked[m.Channel] = true
	case isData(m.Type) && !s.acked[m.Channel]:
		t.Errorf("%s on %s arrived before its subscribed acknowledgement: %s", m.Type, m.Channel, m.raw)
	}
}

// data takes the next snapshot, delta or trade on channel.
func (s *stream) data(t *testing.T, channel string) streamMsg {
	t.Helper()
	return s.next(t, "data on "+channel, func(m streamMsg) bool {
		return m.Channel == channel && isData(m.Type)
	})
}

// control takes the next message of type typ on channel.
func (s *stream) control(t *testing.T, typ, channel string) streamMsg {
	t.Helper()
	return s.next(t, typ+" "+channel, func(m streamMsg) bool {
		return m.Type == typ && m.Channel == channel
	})
}

// errorMsg takes the next error message and checks it has a code and a
// message.
func (s *stream) errorMsg(t *testing.T) streamMsg {
	t.Helper()
	m := s.next(t, "error message", func(m streamMsg) bool { return m.Type == "error" })
	if m.Code == "" || m.Message == "" {
		t.Errorf("error message without code or message: %s", m.raw)
	}
	return m
}

// check verifies the fields of a message as it is taken and, for data
// messages, that its channel's seq counts 1, 2, 3, ... without gaps.
func (s *stream) check(t *testing.T, m streamMsg) {
	t.Helper()
	var required []string
	switch m.Type {
	case "subscribed", "unsubscribed":
		required = []string{"type", "channel"}
	case "error":
		required = []string{"type", "code", "message"}
	case "book_snapshot":
		required = []string{"type", "channel", "seq", "book_seq", "bids", "asks"}
	case "book_delta":
		required = []string{"type", "channel", "seq", "book_seq", "changes"}
	case "trade":
		required = []string{"type", "channel", "seq", "book_seq", "price", "qty",
			"taker_side", "taker_order_id", "maker_order_id", "ts"}
	default:
		t.Errorf("unknown message type: %s", m.raw)
	}
	for _, k := range required {
		if _, ok := m.keys[k]; !ok {
			t.Errorf("%s message lacks field %q: %s", m.Type, k, m.raw)
		}
	}
	if isData(m.Type) {
		want := s.lastSeq[m.Channel] + 1
		if m.Seq != want {
			t.Errorf("%s on %s: seq %d, want %d", m.Type, m.Channel, m.Seq, want)
		}
		s.lastSeq[m.Channel] = m.Seq
	}
}

// requireNoPending fails if a message arrived that no step took.
func (s *stream) requireNoPending(t *testing.T) {
	t.Helper()
	if len(s.pending) > 0 {
		t.Errorf("unexpected messages:%s", describe(s.pending))
	}
}

// closeError waits for the server to end the connection, discarding queued
// messages, and returns the error the read ended with.
func (s *stream) closeError(t *testing.T) error {
	t.Helper()
	timeout := time.NewTimer(waitFor)
	defer timeout.Stop()
	for {
		select {
		case _, ok := <-s.in:
			if !ok {
				return s.err
			}
		case <-timeout.C:
			t.Fatalf("connection still open after %v", waitFor)
		}
	}
}

func requireClose(t *testing.T, err error, code websocket.StatusCode, reason string) {
	t.Helper()
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("connection ended with %v, want close code %d", err, code)
	}
	if ce.Code != code {
		t.Errorf("close code %d (%q), want %d", ce.Code, ce.Reason, code)
	}
	if reason != "" && ce.Reason != reason {
		t.Errorf("close reason %q, want %q", ce.Reason, reason)
	}
}

// ---------------------------------------------------------------------------
// POST /v1/orders

func TestPlaceOrderReplies(t *testing.T) {
	env := newEnv(t, envConfig{})

	// A limit order without tif is GTC; with nothing to trade against, it
	// rests in full.
	s1 := env.place(t, limitOrder("sell", 6500100, 5).with("client_id", "c-001"))
	id1 := s1.Order.OrderID
	if id1 == 0 || s1.Order.TS <= 0 {
		t.Fatalf("order_id %d, ts %d: want both set by the engine", id1, s1.Order.TS)
	}
	checkStatus(t, s1.Order, orderStatus{
		OrderID: id1, Symbol: btc, ClientID: "c-001", Side: "sell", Type: "limit", TIF: "gtc",
		Price: 6500100, Qty: 5, RemainingQty: 5, Status: "open", TS: s1.Order.TS,
	})
	checkEvents(t, s1, 1, s1.Order.TS,
		event{Type: "accepted", OrderID: id1, Side: "sell", Price: 6500100, Qty: 5},
		event{Type: "rested", OrderID: id1, Side: "sell", Price: 6500100, Qty: 5, LevelSide: "sell", LevelQty: 5},
	)

	s2 := env.place(t, limitOrder("sell", 6500200, 3))
	id2 := s2.Order.OrderID
	checkEvents(t, s2, 3, s2.Order.TS,
		event{Type: "accepted", OrderID: id2, Side: "sell", Price: 6500200, Qty: 3},
		event{Type: "rested", OrderID: id2, Side: "sell", Price: 6500200, Qty: 3, LevelSide: "sell", LevelQty: 3},
	)
	if id2 == id1 || s2.Order.TS <= s1.Order.TS {
		t.Errorf("second order: id %d, ts %d; want a new id and a later ts than %d, %d", id2, s2.Order.TS, id1, s1.Order.TS)
	}

	// A market order without tif or price is IOC with price 0: it sweeps
	// both levels, best price first, and its remainder is canceled.
	m := env.place(t, marketOrder("buy", 10))
	idm := m.Order.OrderID
	checkStatus(t, m.Order, orderStatus{
		OrderID: idm, Symbol: btc, Side: "buy", Type: "market", TIF: "ioc",
		Qty: 10, FilledQty: 8, Status: "canceled", Reason: "ioc_remainder", TS: m.Order.TS,
	})
	checkEvents(t, m, 5, m.Order.TS,
		event{Type: "accepted", OrderID: idm, Side: "buy", Qty: 10},
		event{Type: "trade", OrderID: idm, MakerID: id1, Side: "buy", Price: 6500100, Qty: 5, LevelSide: "sell"},
		event{Type: "trade", OrderID: idm, MakerID: id2, Side: "buy", Price: 6500200, Qty: 3, LevelSide: "sell"},
		event{Type: "canceled", OrderID: idm, Side: "buy", Qty: 2, Reason: "ioc_remainder"},
	)

	// A limit buy without tif rests on the bid side.
	b := env.place(t, limitOrder("buy", 6499900, 7))
	idb := b.Order.OrderID
	checkEvents(t, b, 9, b.Order.TS,
		event{Type: "accepted", OrderID: idb, Side: "buy", Price: 6499900, Qty: 7},
		event{Type: "rested", OrderID: idb, Side: "buy", Price: 6499900, Qty: 7, LevelSide: "buy", LevelQty: 7},
	)

	// A crossing GTC sell fills against the bid and rests the remainder.
	p := env.place(t, limitOrder("sell", 6499900, 10).with("tif", "gtc"))
	idp := p.Order.OrderID
	checkStatus(t, p.Order, orderStatus{
		OrderID: idp, Symbol: btc, Side: "sell", Type: "limit", TIF: "gtc", Price: 6499900,
		Qty: 10, FilledQty: 7, RemainingQty: 3, Status: "open", TS: p.Order.TS,
	})
	checkEvents(t, p, 11, p.Order.TS,
		event{Type: "accepted", OrderID: idp, Side: "sell", Price: 6499900, Qty: 10},
		event{Type: "trade", OrderID: idp, MakerID: idb, Side: "sell", Price: 6499900, Qty: 7, LevelSide: "buy"},
		event{Type: "rested", OrderID: idp, Side: "sell", Price: 6499900, Qty: 3, LevelSide: "sell", LevelQty: 3},
	)

	// A limit IOC trades at the maker's price; its remainder is canceled
	// without ever resting, so that event changes no level.
	i := env.place(t, limitOrder("buy", 6500000, 5).with("tif", "ioc"))
	idi := i.Order.OrderID
	checkStatus(t, i.Order, orderStatus{
		OrderID: idi, Symbol: btc, Side: "buy", Type: "limit", TIF: "ioc", Price: 6500000,
		Qty: 5, FilledQty: 3, Status: "canceled", Reason: "ioc_remainder", TS: i.Order.TS,
	})
	checkEvents(t, i, 14, i.Order.TS,
		event{Type: "accepted", OrderID: idi, Side: "buy", Price: 6500000, Qty: 5},
		event{Type: "trade", OrderID: idi, MakerID: idp, Side: "buy", Price: 6499900, Qty: 3, LevelSide: "sell"},
		event{Type: "canceled", OrderID: idi, Side: "buy", Price: 6500000, Qty: 2, Reason: "ioc_remainder"},
	)

	// Every order has finished; GET reports final statuses, including the
	// fills orders received as makers.
	filled := func(o orderStatus) orderStatus {
		o.FilledQty, o.RemainingQty, o.Status, o.Reason = o.Qty, 0, "filled", ""
		return o
	}
	for _, want := range []orderStatus{
		filled(s1.Order), filled(s2.Order), m.Order, filled(b.Order), filled(p.Order), i.Order,
	} {
		checkStatus(t, env.getOrder(t, want.OrderID), want)
	}
	bk := env.book(t, btc, "")
	if bk.Seq != 16 || len(bk.Bids) != 0 || len(bk.Asks) != 0 {
		t.Errorf("book %+v, want empty at seq 16", bk)
	}
}

func TestFOKOrders(t *testing.T) {
	env := newEnv(t, envConfig{})

	// A FOK order that cannot fill in full is answered 200 as rejected and
	// changes nothing; its only event is the rejection.
	f1 := env.place(t, limitOrder("buy", 100, 5).with("tif", "fok"))
	checkStatus(t, f1.Order, orderStatus{
		OrderID: f1.Order.OrderID, Symbol: btc, Side: "buy", Type: "limit", TIF: "fok", Price: 100,
		Qty: 5, Status: "rejected", Reason: "fok_unfillable", TS: f1.Order.TS,
	})
	checkEvents(t, f1, 1, f1.Order.TS,
		event{Type: "rejected", OrderID: f1.Order.OrderID, Reason: "fok_unfillable"})

	mk := env.place(t, limitOrder("sell", 100, 3))
	f2 := env.place(t, limitOrder("buy", 100, 5).with("tif", "fok"))
	if f2.Order.Status != "rejected" || f2.Order.Reason != "fok_unfillable" || f2.Order.FilledQty != 0 {
		t.Errorf("FOK for 5 against 3: %+v, want rejected with fok_unfillable and no fill", f2.Order)
	}
	checkEvents(t, f2, 4, f2.Order.TS,
		event{Type: "rejected", OrderID: f2.Order.OrderID, Reason: "fok_unfillable"})

	f3 := env.place(t, marketOrder("buy", 4).with("tif", "fok"))
	checkStatus(t, f3.Order, orderStatus{
		OrderID: f3.Order.OrderID, Symbol: btc, Side: "buy", Type: "market", TIF: "fok",
		Qty: 4, Status: "rejected", Reason: "fok_unfillable", TS: f3.Order.TS,
	})
	checkEvents(t, f3, 5, f3.Order.TS,
		event{Type: "rejected", OrderID: f3.Order.OrderID, Reason: "fok_unfillable"})

	bk := env.book(t, btc, "")
	checkLevels(t, "asks after rejected FOK orders", bk.Asks, []level{{Price: 100, Qty: 3, Orders: 1}})
	if bk.Seq != 5 || len(bk.Bids) != 0 {
		t.Errorf("book %+v, want seq 5 and no bids", bk)
	}

	// A FOK order that can fill does so in full.
	f4 := env.place(t, limitOrder("buy", 100, 3).with("tif", "fok"))
	checkStatus(t, f4.Order, orderStatus{
		OrderID: f4.Order.OrderID, Symbol: btc, Side: "buy", Type: "limit", TIF: "fok", Price: 100,
		Qty: 3, FilledQty: 3, Status: "filled", TS: f4.Order.TS,
	})
	checkEvents(t, f4, 6, f4.Order.TS,
		event{Type: "accepted", OrderID: f4.Order.OrderID, Side: "buy", Price: 100, Qty: 3},
		event{Type: "trade", OrderID: f4.Order.OrderID, MakerID: mk.Order.OrderID, Side: "buy", Price: 100, Qty: 3, LevelSide: "sell"},
	)

	// A rejected order has finished: GET reports it, DELETE refuses it.
	checkStatus(t, env.getOrder(t, f1.Order.OrderID), f1.Order)
	checkStatus(t, env.getOrder(t, f3.Order.OrderID), f3.Order)
	expectError(t, env.do(t, http.MethodDelete, orderPath(f1.Order.OrderID), ""), http.StatusConflict, "order_finished")
}

func TestRequestErrors(t *testing.T) {
	env := newEnv(t, envConfig{})

	const valid = `"symbol":"BTC-USD","side":"buy","type":"limit","price":100,"qty":1`
	orderWith := func(fields string) string { return "{" + fields + "}" }
	tooLarge := `{` + valid + `,"client_id":"c"` + strings.Repeat(" ", 5000) + `}`

	// code is the expected error code; "a|b" accepts either, where
	// docs/API.md requires the 400 but leaves the code open: it says a
	// number with a fraction is rejected without naming a code, and a
	// value of the wrong JSON type is both malformed (bad_request) and an
	// invalid value of its field.
	cases := []struct {
		name         string
		method, path string
		body         string
		status       int
		code         string
	}{
		{"malformed JSON", "POST", "/v1/orders", `{"symbol":"BTC-USD",`, 400, "bad_request"},
		{"not an object", "POST", "/v1/orders", `[1,2,3]`, 400, "bad_request"},
		{"unknown field", "POST", "/v1/orders", orderWith(valid + `,"color":"red"`), 400, "bad_request"},
		{"fractional price", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":"buy","type":"limit","price":100.5,"qty":1}`, 400, "bad_request|invalid_price"},
		{"string price", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":"buy","type":"limit","price":"100","qty":1}`, 400, "bad_request|invalid_price"},
		{"fractional qty", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":"buy","type":"limit","price":100,"qty":1.5}`, 400, "bad_request|invalid_qty"},
		{"string qty", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":"buy","type":"limit","price":100,"qty":"1"}`, 400, "bad_request|invalid_qty"},
		{"numeric side", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":1,"type":"limit","price":100,"qty":1}`, 400, "bad_request|invalid_side"},

		{"misspelled side", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":"hold","type":"limit","price":100,"qty":1}`, 400, "invalid_side"},
		{"missing side", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","type":"limit","price":100,"qty":1}`, 400, "invalid_side"},
		{"misspelled type", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":"buy","type":"stop","price":100,"qty":1}`, 400, "invalid_type"},
		{"missing type", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":"buy","price":100,"qty":1}`, 400, "invalid_type"},
		{"misspelled tif", "POST", "/v1/orders", orderWith(valid + `,"tif":"day"`), 400, "invalid_tif"},
		{"market gtc", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":"buy","type":"market","tif":"gtc","qty":1}`, 400, "market_gtc"},
		{"limit price zero", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":"buy","type":"limit","price":0,"qty":1}`, 400, "invalid_price"},
		{"limit price missing", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":"buy","type":"limit","qty":1}`, 400, "invalid_price"},
		{"limit price negative", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":"buy","type":"limit","price":-5,"qty":1}`, 400, "invalid_price"},
		{"limit price above max", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":"buy","type":"limit","price":1000000001,"qty":1}`, 400, "invalid_price"},
		{"market with price", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":"buy","type":"market","price":100,"qty":1}`, 400, "invalid_price"},
		{"qty zero", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":"buy","type":"limit","price":100,"qty":0}`, 400, "invalid_qty"},
		{"qty missing", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":"buy","type":"limit","price":100}`, 400, "invalid_qty"},
		{"qty negative", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":"buy","type":"limit","price":100,"qty":-1}`, 400, "invalid_qty"},
		{"qty above max", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":"buy","type":"limit","price":100,"qty":1000000001}`, 400, "invalid_qty"},
		{"first failed check wins", "POST", "/v1/orders",
			`{"symbol":"BTC-USD","side":"hold","type":"limit","price":0,"qty":0}`, 400, "invalid_side"},
		{"client_id of 65 bytes", "POST", "/v1/orders",
			orderWith(valid + `,"client_id":"` + strings.Repeat("x", 65) + `"`), 400, "invalid_client_id"},
		{"unknown symbol", "POST", "/v1/orders",
			`{"symbol":"DOGE-USD","side":"buy","type":"limit","price":100,"qty":1}`, 404, "unknown_symbol"},
		{"body over 4 KB", "POST", "/v1/orders", tooLarge, 413, "body_too_large"},
		{"over 4 KB after the object", "POST", "/v1/orders", orderWith(valid) + strings.Repeat(" ", 5000), 413, "body_too_large"},
		{"empty body", "POST", "/v1/orders", "", 400, "bad_request"},
		{"null body", "POST", "/v1/orders", "null", 400, "bad_request"},
		{"two objects", "POST", "/v1/orders", orderWith(valid) + orderWith(valid), 400, "bad_request"},
		{"field name in other case", "POST", "/v1/orders", orderWith(valid + `,"QTY":7`), 400, "bad_request"},
		{"every name in other case", "POST", "/v1/orders",
			`{"SYMBOL":"BTC-USD","Side":"buy","TYPE":"limit","Price":100,"Qty":1}`, 400, "bad_request"},
		{"repeated field", "POST", "/v1/orders", orderWith(valid + `,"qty":7`), 400, "bad_request"},

		{"unknown route", "GET", "/v1/nothing", "", 404, "not_found"},
		{"method not allowed", "PUT", "/v1/orders/5", "", 405, "method_not_allowed"},

		{"GET non-numeric id", "GET", "/v1/orders/abc", "", 400, "invalid_order_id"},
		{"GET id 0", "GET", "/v1/orders/0", "", 400, "invalid_order_id"},
		{"GET negative id", "GET", "/v1/orders/-1", "", 400, "invalid_order_id"},
		{"GET id beyond uint64", "GET", "/v1/orders/99999999999999999999999", "", 400, "invalid_order_id"},
		{"DELETE non-numeric id", "DELETE", "/v1/orders/abc", "", 400, "invalid_order_id"},
		{"DELETE id 0", "DELETE", "/v1/orders/0", "", 400, "invalid_order_id"},
		{"GET never issued id", "GET", "/v1/orders/987654321", "", 404, "unknown_order"},
		{"DELETE never issued id", "DELETE", "/v1/orders/987654321", "", 404, "unknown_order"},

		{"depth 0", "GET", "/v1/books/BTC-USD?depth=0", "", 400, "invalid_depth"},
		{"depth 1001", "GET", "/v1/books/BTC-USD?depth=1001", "", 400, "invalid_depth"},
		{"depth negative", "GET", "/v1/books/BTC-USD?depth=-1", "", 400, "invalid_depth"},
		{"depth not numeric", "GET", "/v1/books/BTC-USD?depth=abc", "", 400, "invalid_depth"},
		{"book of unknown symbol", "GET", "/v1/books/DOGE-USD", "", 404, "unknown_symbol"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			expectError(t, env.do(t, c.method, c.path, c.body), c.status, strings.Split(c.code, "|")...)
		})
	}

	// None of these requests applied anything.
	for _, s := range []string{btc, eth} {
		if bk := env.book(t, s, ""); bk.Seq != 0 || len(bk.Bids)+len(bk.Asks) != 0 {
			t.Errorf("%s book after rejected requests: %+v, want empty at seq 0", s, bk)
		}
	}

	// 64 bytes is the longest client_id, and it is echoed back.
	id64 := strings.Repeat("y", 64)
	r := env.place(t, limitOrder("buy", 100, 1).with("client_id", id64))
	if r.Order.ClientID != id64 {
		t.Errorf("client_id %q, want %q", r.Order.ClientID, id64)
	}
	if got := env.getOrder(t, r.Order.OrderID); got.ClientID != id64 {
		t.Errorf("GET client_id %q, want %q", got.ClientID, id64)
	}
}

// ---------------------------------------------------------------------------
// DELETE and GET /v1/orders/{id}

func TestCancelPartlyFilledOrder(t *testing.T) {
	env := newEnv(t, envConfig{})

	maker := env.place(t, limitOrder("sell", 200, 10).with("client_id", "maker-1"))
	mid := maker.Order.OrderID
	taker := env.place(t, limitOrder("buy", 200, 4).with("tif", "ioc"))
	tid := taker.Order.OrderID
	checkStatus(t, taker.Order, orderStatus{
		OrderID: tid, Symbol: btc, Side: "buy", Type: "limit", TIF: "ioc", Price: 200,
		Qty: 4, FilledQty: 4, Status: "filled", TS: taker.Order.TS,
	})
	// A fully filled order ends with its last trade.
	checkEvents(t, taker, 3, taker.Order.TS,
		event{Type: "accepted", OrderID: tid, Side: "buy", Price: 200, Qty: 4},
		event{Type: "trade", OrderID: tid, MakerID: mid, Side: "buy", Price: 200, Qty: 4, LevelSide: "sell", LevelQty: 6},
	)

	// The maker rests, partly filled.
	want := maker.Order
	want.FilledQty, want.RemainingQty = 4, 6
	checkStatus(t, env.getOrder(t, mid), want)

	// Canceling it keeps the fill and the order's own timestamp.
	c := env.cancel(t, mid)
	want.Status, want.Reason, want.RemainingQty = "canceled", "user_cancel", 0
	checkStatus(t, c.Order, want)
	checkEvents(t, c, 5, 0,
		event{Type: "canceled", OrderID: mid, Side: "sell", Price: 200, Qty: 6, Reason: "user_cancel", LevelSide: "sell"})
	if c.Events[0].TS <= taker.Order.TS {
		t.Errorf("cancel event ts %d, want later than the orders before it (%d)", c.Events[0].TS, taker.Order.TS)
	}
	checkStatus(t, env.getOrder(t, mid), want)
	if bk := env.book(t, btc, ""); bk.Seq != 5 || len(bk.Bids)+len(bk.Asks) != 0 {
		t.Errorf("book %+v, want empty at seq 5", bk)
	}

	// Neither order rests any more. The API may answer these from the
	// order's status or by sending the cancel to the book, which rejects
	// it with one event (Seq counts rejections too), so the book's seq
	// may have moved on by one per DELETE, but nothing else changed.
	expectError(t, env.do(t, http.MethodDelete, orderPath(mid), ""), http.StatusConflict, "order_finished")
	expectError(t, env.do(t, http.MethodDelete, orderPath(tid), ""), http.StatusConflict, "order_finished")
	checkStatus(t, env.getOrder(t, mid), want)
	checkStatus(t, env.getOrder(t, tid), taker.Order)
	if bk := env.book(t, btc, ""); bk.Seq < 5 || bk.Seq > 7 || len(bk.Bids)+len(bk.Asks) != 0 {
		t.Errorf("book %+v, want empty at seq 5 to 7", bk)
	}
}

func TestNeverIssuedOrder(t *testing.T) {
	env := newEnv(t, envConfig{})
	r := env.place(t, limitOrder("buy", 100, 1))
	id := r.Order.OrderID
	// D019: an ID's low 8 bits are its symbol's index (BTC-USD is 0), so
	// the same counter value with ETH-USD's index was never issued, though
	// it routes to a real book.
	if id&0xff != 0 {
		t.Fatalf("order ID %#x does not carry BTC-USD's symbol index 0 (D019)", id)
	}
	for _, bad := range []uint64{id | 1, 987654321 << 8} {
		expectError(t, env.do(t, http.MethodGet, orderPath(bad), ""), http.StatusNotFound, "unknown_order")
		expectError(t, env.do(t, http.MethodDelete, orderPath(bad), ""), http.StatusNotFound, "unknown_order")
	}
	checkStatus(t, env.getOrder(t, r.Order.OrderID), r.Order)
}

// ---------------------------------------------------------------------------
// GET /v1/books/{symbol}

func TestBookDepth(t *testing.T) {
	env := newEnv(t, envConfig{})

	// 25 bid levels, 1000 to 1024 with qty 1 to 25, plus a second order at
	// the best bid; 22 ask levels, 2000 to 2021 with qty 10 to 31.
	var last uint64
	lastSeq := func(r orderReply) { last = r.Events[len(r.Events)-1].Seq }
	for i := range int64(25) {
		lastSeq(env.place(t, limitOrder("buy", 1000+i, 1+i)))
	}
	lastSeq(env.place(t, limitOrder("buy", 1024, 7)))
	for i := range int64(22) {
		lastSeq(env.place(t, limitOrder("sell", 2000+i, 10+i)))
	}
	var bids, asks []level
	for i := int64(24); i >= 0; i-- {
		l := level{Price: 1000 + i, Qty: 1 + i, Orders: 1}
		if i == 24 {
			l.Qty, l.Orders = 25+7, 2
		}
		bids = append(bids, l)
	}
	for i := range int64(22) {
		asks = append(asks, level{Price: 2000 + i, Qty: 10 + i, Orders: 1})
	}

	for _, c := range []struct {
		query string
		depth int
	}{
		{"", 20}, // the default
		{"?depth=20", 20},
		{"?depth=1", 1},
		{"?depth=5", 5},
		{"?depth=22", 22},
		{"?depth=1000", 1000},
	} {
		bk := env.book(t, btc, c.query)
		if bk.Seq != last {
			t.Errorf("%q: seq %d, want %d", c.query, bk.Seq, last)
		}
		checkLevels(t, c.query+" bids", bk.Bids, bids[:min(c.depth, len(bids))])
		checkLevels(t, c.query+" asks", bk.Asks, asks[:min(c.depth, len(asks))])
	}

	// Books are independent: ETH-USD is still empty.
	if bk := env.book(t, eth, ""); bk.Seq != 0 || len(bk.Bids)+len(bk.Asks) != 0 {
		t.Errorf("ETH-USD book %+v, want empty at seq 0", bk)
	}
}

// ---------------------------------------------------------------------------
// Backpressure and shutdown

type sendResult struct {
	res httpResult
	err error
}

// postAsync posts each body on its own goroutine; the results arrive on the
// returned channel in completion order.
func (e *testEnv) postAsync(results chan<- sendResult, bodies ...string) {
	for _, b := range bodies {
		go func() {
			res, err := e.send(http.MethodPost, "/v1/orders", b)
			results <- sendResult{res, err}
		}()
	}
}

func TestBusyWhenQueueFull(t *testing.T) {
	const queue, extra = 2, 6
	env := newEnv(t, envConfig{queueSize: queue})
	before := env.metrics(t)

	results := make(chan sendResult, 1+queue+extra)
	entered, release := env.clock.stall()
	env.postAsync(results, limitOrder("buy", 100, 1).body())
	receive(t, entered, "stalled book goroutine")

	// The book goroutine is stalled inside a batch, so its queue takes
	// exactly `queue` more orders and the rest are refused at once.
	var bodies []string
	for i := range int64(queue + extra) {
		bodies = append(bodies, limitOrder("buy", 101+i, 1).body())
	}
	env.postAsync(results, bodies...)
	for range extra {
		r := receive(t, results, "busy reply")
		if r.err != nil {
			t.Fatalf("POST: %v", r.err)
		}
		expectError(t, r.res, http.StatusServiceUnavailable, "busy")
		if got := r.res.header.Get("Retry-After"); got != "1" {
			t.Errorf("Retry-After %q, want 1", got)
		}
	}

	// Once the book moves again, every queued order is applied.
	release()
	for range 1 + queue {
		r := receive(t, results, "reply to a queued order")
		if r.err != nil {
			t.Fatalf("POST: %v", r.err)
		}
		if o := decodeOrderReply(t, r.res); o.Order.Status != "open" {
			t.Errorf("queued order %+v, want open", o.Order)
		}
	}
	if bk := env.book(t, btc, ""); len(bk.Bids) != 1+queue {
		t.Errorf("%d bid levels, want %d", len(bk.Bids), 1+queue)
	}

	env.metricsAfter(t, before,
		rise{"matching_engine_orders_total", []string{"symbol", btc, "result", "busy"}, extra},
		rise{"matching_engine_orders_total", []string{"symbol", btc, "result", "accepted"}, 1 + queue},
	)
}

func TestShuttingDownAfterEngineClosed(t *testing.T) {
	env := newEnv(t, envConfig{})
	resting := env.place(t, limitOrder("sell", 100, 1))
	if err := env.eng.Close(); err != nil {
		t.Fatalf("engine.Close: %v", err)
	}
	expectError(t, env.do(t, http.MethodPost, "/v1/orders", limitOrder("buy", 100, 1).body()),
		http.StatusServiceUnavailable, "shutting_down")
	expectError(t, env.do(t, http.MethodDelete, orderPath(resting.Order.OrderID), ""),
		http.StatusServiceUnavailable, "shutting_down")
	expectError(t, env.do(t, http.MethodGet, "/v1/books/BTC-USD", ""),
		http.StatusServiceUnavailable, "shutting_down")
}

func expectHealth(t *testing.T, res httpResult, status int, want string) {
	t.Helper()
	if res.status != status {
		t.Fatalf("/healthz status %d, want %d; body %s", res.status, status, res.body)
	}
	requireJSON(t, res)
	var h struct {
		Status string `json:"status"`
	}
	decodeStrict(t, res.body, &h)
	if h.Status != want {
		t.Errorf("/healthz status %q, want %q", h.Status, want)
	}
}

func TestHealthzAndBeginShutdown(t *testing.T) {
	env := newEnv(t, envConfig{})
	expectHealth(t, env.do(t, http.MethodGet, "/healthz", ""), http.StatusOK, "ok")

	ws := env.stream(t)
	ws.send(t, `{"op":"subscribe","channels":["trades:BTC-USD"]}`)
	ws.control(t, "subscribed", tradesBTC)

	env.srv.BeginShutdown()
	expectHealth(t, env.do(t, http.MethodGet, "/healthz", ""), http.StatusServiceUnavailable, "shutting_down")

	// New WebSocket connections are refused.
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, env.streamURL(), nil)
	if err == nil {
		_ = conn.CloseNow()
		t.Fatal("WebSocket dial succeeded after BeginShutdown")
	}
	if resp == nil {
		t.Fatalf("WebSocket dial failed without an HTTP response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the refusal: %v", err)
	}
	expectError(t, httpResult{status: resp.StatusCode, header: resp.Header, body: body},
		http.StatusServiceUnavailable, "shutting_down")

	// Connections already open end with 1001 once streams are closed.
	env.srv.CloseStreams()
	requireClose(t, ws.closeError(t), websocket.StatusGoingAway, "")
}

func TestCloseStreams(t *testing.T) {
	env := newEnv(t, envConfig{})
	subscribed := env.stream(t)
	subscribed.send(t, `{"op":"subscribe","channels":["trades:BTC-USD","book:BTC-USD"]}`)
	subscribed.control(t, "subscribed", tradesBTC)
	subscribed.control(t, "subscribed", bookBTC)
	// A connection with no channels; the round trip of a bad message makes
	// sure the server is serving it before streams are closed.
	bare := env.stream(t)
	bare.send(t, `{"op":"bogus"}`)
	bare.errorMsg(t)

	env.srv.CloseStreams()
	requireClose(t, subscribed.closeError(t), websocket.StatusGoingAway, "")
	requireClose(t, bare.closeError(t), websocket.StatusGoingAway, "")
	expectDisconnects(t, env, "shutdown", 2)
}

// ---------------------------------------------------------------------------
// GET /metrics

func TestMetrics(t *testing.T) {
	env := newEnv(t, envConfig{})
	ws := env.stream(t)
	ws.send(t, `{"op":"subscribe","channels":["trades:BTC-USD"]}`)
	ws.control(t, "subscribed", tradesBTC)

	before := env.metrics(t)
	maker := env.place(t, limitOrder("sell", 100, 5))
	taker := env.place(t, limitOrder("buy", 100, 2))
	if taker.Order.Status != "filled" {
		t.Fatalf("taker %+v, want filled", taker.Order)
	}
	if fok := env.place(t, limitOrder("buy", 100, 10).with("tif", "fok")); fok.Order.Status != "rejected" {
		t.Fatalf("FOK %+v, want rejected", fok.Order)
	}
	expectError(t, env.do(t, http.MethodPost, "/v1/orders", limitOrder("buy", 100, 0).body()),
		http.StatusBadRequest, "invalid_qty")
	env.cancel(t, maker.Order.OrderID)
	expectError(t, env.do(t, http.MethodDelete, orderPath(taker.Order.OrderID), ""),
		http.StatusConflict, "order_finished")
	after := env.metricsAfter(t, before,
		rise{"matching_engine_orders_total", []string{"symbol", btc, "result", "accepted"}, 2},
		rise{"matching_engine_orders_total", []string{"symbol", btc, "result", "rejected"}, 1},
		rise{"matching_engine_orders_total", []string{"symbol", btc, "result", "invalid"}, 1},
		rise{"matching_engine_orders_total", []string{"symbol", btc, "result", "busy"}, 0},
		rise{"matching_engine_trades_total", []string{"symbol", btc}, 1},
		rise{"matching_engine_trades_total", []string{"symbol", eth}, 0},
		rise{"matching_engine_cancels_total", []string{"symbol", btc, "result", "canceled"}, 1},
		rise{"matching_engine_cancels_total", []string{"symbol", btc, "result", "finished"}, 1},
	)
	if v, ok := after.values[sampleKey("matching_engine_trades_total", "symbol", btc)]; !ok || v < 1 {
		t.Errorf("no sample matching_engine_trades_total{symbol=%q} >= 1 in /metrics", btc)
	}

	for name, typ := range map[string]string{
		"matching_engine_orders_total":                  "counter",
		"matching_engine_cancels_total":                 "counter",
		"matching_engine_trades_total":                  "counter",
		"matching_engine_queue_length":                  "gauge",
		"matching_engine_log_failed":                    "gauge",
		"matching_engine_log_fsync_duration_seconds":    "histogram",
		"matching_engine_ws_disconnects_total":          "counter",
		"go_goroutines":                                 "gauge",
		"matching_engine_http_request_duration_seconds": "histogram",
		"matching_engine_ws_connections":                "gauge",
	} {
		if got := after.types[name]; got != typ {
			t.Errorf("# TYPE %s is %q, want %q", name, got, typ)
		}
	}
	if v, ok := after.values[sampleKey("matching_engine_log_failed", "symbol", btc)]; !ok || v != 0 {
		t.Errorf("matching_engine_log_failed{symbol=%q} = %v (present %v), want 0", btc, v, ok)
	}
	if v := after.value("matching_engine_ws_connections"); v < 1 {
		t.Errorf("matching_engine_ws_connections %v with a connection open, want >= 1", v)
	}
}

// ---------------------------------------------------------------------------
// WebSocket /v1/stream

// bookState is an L2 book rebuilt from a snapshot and deltas:
// side -> price -> qty.
type bookState map[string]map[int64]int64

func newBookState(snap streamMsg) bookState {
	b := bookState{"buy": {}, "sell": {}}
	for _, l := range snap.Bids {
		b["buy"][l.Price] = l.Qty
	}
	for _, l := range snap.Asks {
		b["sell"][l.Price] = l.Qty
	}
	return b
}

func (b bookState) apply(t *testing.T, changes []change) {
	t.Helper()
	for _, c := range changes {
		side, ok := b[c.Side]
		if !ok {
			t.Fatalf("change on unknown side %q", c.Side)
		}
		if c.Qty == 0 {
			delete(side, c.Price)
		} else {
			side[c.Price] = c.Qty
		}
	}
}

// priceQty is a level without its order count, which deltas do not carry.
type priceQty struct{ Price, Qty int64 }

// levels returns one side, best price first.
func (b bookState) levels(side string) []priceQty {
	var out []priceQty
	for p, q := range b[side] {
		out = append(out, priceQty{p, q})
	}
	slices.SortFunc(out, func(x, y priceQty) int {
		if side == "buy" {
			return cmp.Compare(y.Price, x.Price)
		}
		return cmp.Compare(x.Price, y.Price)
	})
	return out
}

func priceQtys(ls []level) []priceQty {
	var out []priceQty
	for _, l := range ls {
		out = append(out, priceQty{l.Price, l.Qty})
	}
	return out
}

func sortedChanges(cs []change) []change {
	cs = slices.Clone(cs)
	slices.SortFunc(cs, func(x, y change) int {
		return cmp.Or(strings.Compare(x.Side, y.Side), cmp.Compare(x.Price, y.Price))
	})
	return cs
}

// checkTrade checks a trade message against the trade event it reports.
func checkTrade(t *testing.T, m streamMsg, ev event) {
	t.Helper()
	type view struct {
		Type, Channel  string
		BookSeq        uint64
		Price, Qty     int64
		TakerSide      string
		TakerID, Maker uint64
		TS             int64
	}
	got := view{m.Type, m.Channel, m.BookSeq, m.Price, m.Qty, m.TakerSide, m.TakerOrderID, m.MakerOrderID, m.TS}
	want := view{"trade", tradesBTC, ev.Seq, ev.Price, ev.Qty, ev.Side, ev.OrderID, ev.MakerID, ev.TS}
	if got != want {
		t.Errorf("trade message\n got %+v\nwant %+v", got, want)
	}
}

func tradeEvents(r orderReply) []event {
	var out []event
	for _, e := range r.Events {
		if e.Type == "trade" {
			out = append(out, e)
		}
	}
	return out
}

func TestStreamSnapshotDeltasAndTrades(t *testing.T) {
	env := newEnv(t, envConfig{})
	env.place(t, limitOrder("sell", 101, 5))
	env.place(t, limitOrder("sell", 102, 3))
	o3 := env.place(t, limitOrder("buy", 99, 4))
	before := env.book(t, btc, "?depth=1000")

	ws := env.stream(t)
	ws.send(t, `{"op":"subscribe","channels":["trades:BTC-USD","book:BTC-USD"]}`)
	ws.control(t, "subscribed", tradesBTC)
	ws.control(t, "subscribed", bookBTC)

	// The book channel starts with a snapshot of the book as it is.
	snap := ws.data(t, bookBTC)
	if snap.Type != "book_snapshot" {
		t.Fatalf("first message on %s: %s, want book_snapshot", bookBTC, snap.raw)
	}
	if snap.BookSeq != before.Seq {
		t.Errorf("snapshot book_seq %d, want %d", snap.BookSeq, before.Seq)
	}
	if got, want := priceQtys(snap.Bids), priceQtys(before.Bids); !slices.Equal(got, want) {
		t.Errorf("snapshot bids %+v, want %+v", got, want)
	}
	if got, want := priceQtys(snap.Asks), priceQtys(before.Asks); !slices.Equal(got, want) {
		t.Errorf("snapshot asks %+v, want %+v", got, want)
	}
	state := newBookState(snap)

	// Each command below changes the book, so each yields one delta naming
	// every level it changed once, with the level's new total.
	steps := []struct {
		name    string
		act     func() orderReply
		changes []change
	}{
		{"buy sweeps two ask levels", func() orderReply { return env.place(t, limitOrder("buy", 102, 6)) },
			[]change{{"sell", 101, 0}, {"sell", 102, 2}}},
		{"sell rests at a new level", func() orderReply { return env.place(t, limitOrder("sell", 100, 2)) },
			[]change{{"sell", 100, 2}}},
		{"buy joins a bid level", func() orderReply { return env.place(t, limitOrder("buy", 99, 1)) },
			[]change{{"buy", 99, 5}}},
		{"cancel shrinks a level", func() orderReply { return env.cancel(t, o3.Order.OrderID) },
			[]change{{"buy", 99, 1}}},
		{"market sell empties the bids", func() orderReply { return env.place(t, marketOrder("sell", 3)) },
			[]change{{"buy", 99, 0}}},
		{"buy trades, then rests", func() orderReply { return env.place(t, limitOrder("buy", 100, 4)) },
			[]change{{"sell", 100, 0}, {"buy", 100, 2}}},
	}
	var trades []event
	lastBookSeq := snap.BookSeq
	for _, st := range steps {
		r := st.act()
		trades = append(trades, tradeEvents(r)...)
		d := ws.data(t, bookBTC)
		if d.Type != "book_delta" {
			t.Fatalf("%s: got %s, want book_delta", st.name, d.raw)
		}
		if got, want := sortedChanges(d.Changes), sortedChanges(st.changes); !slices.Equal(got, want) {
			t.Errorf("%s: changes %+v, want %+v", st.name, got, want)
		}
		// book_seq is the event the delta brings the book up to: the
		// command's last event that changed a level, or its last event.
		var lo uint64
		for _, e := range r.Events {
			if e.LevelSide != "" {
				lo = e.Seq
			}
		}
		hi := r.Events[len(r.Events)-1].Seq
		if d.BookSeq < lo || d.BookSeq > hi || d.BookSeq <= lastBookSeq {
			t.Errorf("%s: book_seq %d, want in [%d, %d] and above %d", st.name, d.BookSeq, lo, hi, lastBookSeq)
		}
		lastBookSeq = d.BookSeq
		state.apply(t, d.Changes)
	}

	// The snapshot plus the deltas is the book.
	after := env.book(t, btc, "?depth=1000")
	if got, want := state.levels("buy"), priceQtys(after.Bids); !slices.Equal(got, want) {
		t.Errorf("rebuilt bids %+v, want %+v", got, want)
	}
	if got, want := state.levels("sell"), priceQtys(after.Asks); !slices.Equal(got, want) {
		t.Errorf("rebuilt asks %+v, want %+v", got, want)
	}
	if lastBookSeq != after.Seq {
		t.Errorf("last delta book_seq %d, book seq %d", lastBookSeq, after.Seq)
	}

	// The trade channel reports every fill of BTC-USD, in order.
	if len(trades) != 4 {
		t.Fatalf("%d trades in the replies, want 4", len(trades))
	}
	for _, ev := range trades {
		checkTrade(t, ws.data(t, tradesBTC), ev)
	}

	// A trade on another symbol is not on this channel.
	env.place(t, limitOrder("sell", 50, 1).with("symbol", eth))
	env.place(t, limitOrder("buy", 50, 1).with("symbol", eth))

	// After unsubscribing, the book channel goes quiet while the trade
	// channel carries on with its own seq.
	ws.send(t, `{"op":"unsubscribe","channels":["book:BTC-USD"]}`)
	ws.control(t, "unsubscribed", bookBTC)
	for range 2 {
		r := env.place(t, limitOrder("sell", 100, 1))
		ev := tradeEvents(r)
		if len(ev) != 1 {
			t.Fatalf("sell into the bid: %d trades, want 1", len(ev))
		}
		checkTrade(t, ws.data(t, tradesBTC), ev[0])
	}
	// Nothing else may have arrived: no book data after the unsubscribe and
	// nothing for ETH-USD. The round trip of a bad message gives anything
	// sent for the earlier commands time to arrive (the contract does not
	// order the error reply against data messages, so this can miss a late
	// stray message but never flags a legitimate one).
	ws.send(t, `{"op":"sync-check"}`)
	ws.errorMsg(t)
	ws.requireNoPending(t)
}

func TestStreamBadMessages(t *testing.T) {
	env := newEnv(t, envConfig{})
	ws := env.stream(t)
	for _, msg := range []string{
		`{"op":"bogus","channels":["trades:BTC-USD"]}`,      // unknown op
		`{"op":"subscribe","channels":["candles:BTC-USD"]}`, // unknown channel kind
		`{"op":"subscribe","channels":["book:DOGE-USD"]}`,   // unknown symbol
		`{"op":"subscribe","channels":["BTC-USD"]}`,         // no channel kind
		`{"op":"subscribe",`,                                // not JSON
	} {
		ws.send(t, msg)
		ws.errorMsg(t)
	}

	// The connection stays open and works.
	ws.send(t, `{"op":"subscribe","channels":["trades:BTC-USD"]}`)
	ws.control(t, "subscribed", tradesBTC)
	env.place(t, limitOrder("sell", 100, 1))
	r := env.place(t, limitOrder("buy", 100, 1))
	checkTrade(t, ws.data(t, tradesBTC), tradeEvents(r)[0])
	ws.requireNoPending(t)
}

func TestStreamSlowConsumerIsDisconnected(t *testing.T) {
	// queue stays below the default MaxBatch (256) so the whole queue is
	// applied as one batch.
	const queue, extra = 250, 10
	env := newEnv(t, envConfig{queueSize: queue, subscriberBuffer: 1})

	conn := env.dial(t)
	writeMsg(t, conn, `{"op":"subscribe","channels":["book:BTC-USD"]}`)
	// The acknowledgement comes before the channel's first data message,
	// which is its snapshot.
	for _, want := range []string{"subscribed", "book_snapshot"} {
		if m := readMsg(t, conn); m.Type != want || m.Channel != bookBTC {
			t.Fatalf("got %s, want %s on %s", m.raw, want, bookBTC)
		}
	}
	// From here on the client reads nothing until the burst is over.

	// Fill the queue behind a stalled book goroutine (the busy replies
	// prove it is full), then let it apply the whole queue as one batch:
	// 250 updates back to back for a subscription with room for one
	// (Options.SubscriberBuffer is the capacity of the channel's engine
	// subscription), far faster than any connection can write them, so the
	// engine drops the subscription (D018) and the server closes the
	// connection.
	results := make(chan sendResult, 1+queue+extra)
	entered, release := env.clock.stall()
	env.postAsync(results, limitOrder("buy", 1000, 1).body())
	receive(t, entered, "stalled book goroutine")
	var bodies []string
	for i := range int64(queue + extra) {
		bodies = append(bodies, limitOrder("buy", 1001+i, 1).body())
	}
	env.postAsync(results, bodies...)
	for range extra {
		r := receive(t, results, "busy reply")
		if r.err != nil {
			t.Fatalf("POST: %v", r.err)
		}
		expectError(t, r.res, http.StatusServiceUnavailable, "busy")
	}
	release()
	for range 1 + queue {
		r := receive(t, results, "reply to a queued order")
		if r.err != nil {
			t.Fatalf("POST: %v", r.err)
		}
		decodeOrderReply(t, r.res)
	}

	requireClose(t, newStream(conn).closeError(t), websocket.StatusPolicyViolation, "slow consumer")
	expectDisconnects(t, env, "slow_consumer_updates", 1)

	// Matching was not held up by the slow client.
	if bk := env.book(t, btc, "?depth=1000"); len(bk.Bids) != 1+queue {
		t.Errorf("%d bid levels, want %d", len(bk.Bids), 1+queue)
	}
}
