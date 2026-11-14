//go:build harness

// Command harness-adapter is the matching core as a shared library for the
// flash1-dev matching engine benchmark: it exports the C functions of the
// benchmark's api/matching_engine_api.h and passes each message to
// internal/harness, which maps it onto one book.Book. scripts/harness.sh
// builds it with
//
//	go build -tags harness -buildmode=c-shared -o me_adapter.so ./cmd/harness-adapter
//
// The benchmark calls the library from a thread of its own. Crossing
// between C and Go for every message costs more than all the other work on
// it (bench-results/harness/adapter), so the library exports
// engine_on_batch, for the benchmark to hand it runs of messages, and
// pushes reports into the benchmark's transport in runs too: they collect
// in a buffer that goes to C in one call when it holds flushAt reports and
// at the end of every call into the library (D040). Matching is
// synchronous, so engine_flush has nothing left to do.
//
// A message the book cannot hold (internal/harness) panics, which ends the
// benchmark run.
package main

/*
#include <stdint.h>

// The types below mirror api/matching_engine_api.h of the benchmark at
// commit 424e4e5 (MIT License, Copyright (c) 2026 Flash One Technologies
// LLC) field for field. The header itself is not included because cgo
// declares the exported functions without its const qualifiers.

typedef struct {
	uint64_t order_id;
	uint64_t sequence_number;
	int64_t  price_ticks;
	uint32_t quantity;
	uint8_t  side;
	uint8_t  ioc;
	uint8_t  _reserved[2];
} new_order_t;

typedef struct {
	uint64_t order_id;
	uint64_t sequence_number;
} cancel_t;

typedef struct {
	uint64_t order_id;
	uint64_t sequence_number;
	int64_t  new_price_ticks;
	uint32_t new_quantity;
	uint8_t  side;
	uint8_t  _reserved[3];
} modify_t;

// me_msg_t holds one of the three in a union at offset 8; payload stands
// for the union.
typedef struct {
	uint8_t type;
	uint8_t _reserved[7];
	uint8_t payload[32];
} me_msg_t;

typedef struct {
	uint8_t  type;
	uint8_t  side;
	uint8_t  _reserved[6];
	uint64_t sequence_number;
	uint64_t order_id;
	int64_t  price_ticks;
	uint32_t quantity;
	uint32_t _reserved2;
	uint64_t maker_order_id;
	uint64_t taker_order_id;
	uint64_t _reserved3;
} me_report_t;

typedef struct {
	void*    (*create)(uint32_t capacity);
	int      (*push)(void* handle, const me_report_t* report);
	uint32_t (*drain)(void* handle, me_report_t* out, uint32_t max);
	void     (*flush)(void* handle);
	void     (*destroy)(void* handle);
} me_transport_t;

_Static_assert(sizeof(new_order_t) == 32, "new_order_t");
_Static_assert(sizeof(cancel_t) == 16, "cancel_t");
_Static_assert(sizeof(modify_t) == 32, "modify_t");
_Static_assert(sizeof(me_msg_t) == 40, "me_msg_t");
_Static_assert(sizeof(me_report_t) == 64, "me_report_t");

// push pushes n reports, waiting while the transport is full.
static void push(const me_transport_t* t, void* sink, const me_report_t* r, uint32_t n) {
	for (uint32_t i = 0; i < n; i++)
		while (!t->push(sink, &r[i])) {
		}
}
*/
import "C"

import (
	"unsafe"

	"github.com/sHyanorn/matching-engine/internal/harness"
)

// flushAt is the number of reports that makes the buffer go to C before a
// call ends.
const flushAt = 1024

var (
	transport *C.me_transport_t
	sink      unsafe.Pointer
	engine    *harness.Engine
	reports   []harness.Report // reports not yet pushed
	wire      []C.me_report_t  // the same, laid out for C
)

//export engine_init
func engine_init(_ C.uint64_t, t *C.me_transport_t, s unsafe.Pointer) {
	transport, sink = t, s
	engine = harness.New()
	reports = make([]harness.Report, 0, flushAt)
	wire = make([]C.me_report_t, flushAt)
}

//export engine_shutdown
func engine_shutdown() {
	engine, reports, wire = nil, nil, nil
}

//export engine_on_new_order
func engine_on_new_order(o *C.new_order_t) {
	apply(newOrder(o))
	push()
}

//export engine_on_cancel
func engine_on_cancel(c *C.cancel_t) {
	apply(cancel(c))
	push()
}

//export engine_on_modify
func engine_on_modify(m *C.modify_t) {
	apply(modify(m))
	push()
}

// engine_on_batch applies n messages in order, each exactly as if it came
// alone.
//
//export engine_on_batch
func engine_on_batch(msgs *C.me_msg_t, n C.uint32_t) {
	batch := unsafe.Slice(msgs, n)
	for i := range batch {
		m := &batch[i]
		p := unsafe.Pointer(&m.payload)
		switch m._type {
		case 0:
			apply(newOrder((*C.new_order_t)(p)))
		case 1:
			apply(cancel((*C.cancel_t)(p)))
		case 2:
			apply(modify((*C.modify_t)(p)))
		default:
			panic("engine_on_batch: unknown message type")
		}
	}
	push()
}

//export engine_flush
func engine_flush() {}

//export engine_query_best_bid
func engine_query_best_bid() C.int64_t { return C.int64_t(engine.BestBid()) }

//export engine_query_best_ask
func engine_query_best_ask() C.int64_t { return C.int64_t(engine.BestAsk()) }

//export engine_query_depth_at
func engine_query_depth_at(price C.int64_t, side C.uint8_t) C.uint64_t {
	return C.uint64_t(engine.DepthAt(int64(price), uint8(side)))
}

func newOrder(o *C.new_order_t) harness.Msg {
	return harness.Msg{
		Type: harness.MsgNew, Seq: uint64(o.sequence_number), OrderID: uint64(o.order_id),
		Price: int64(o.price_ticks), Qty: uint32(o.quantity), Side: uint8(o.side), IOC: o.ioc != 0,
	}
}

func cancel(c *C.cancel_t) harness.Msg {
	return harness.Msg{Type: harness.MsgCancel, Seq: uint64(c.sequence_number), OrderID: uint64(c.order_id)}
}

func modify(m *C.modify_t) harness.Msg {
	return harness.Msg{
		Type: harness.MsgModify, Seq: uint64(m.sequence_number), OrderID: uint64(m.order_id),
		Price: int64(m.new_price_ticks), Qty: uint32(m.new_quantity), Side: uint8(m.side),
	}
}

// apply applies m and pushes the buffered reports if there are flushAt or
// more.
func apply(m harness.Msg) {
	var err error
	if reports, err = engine.Apply(m, reports); err != nil {
		panic(err)
	}
	if len(reports) >= flushAt {
		push()
	}
}

// push copies the buffered reports into the C layout and pushes them in
// one call to C.
func push() {
	if len(reports) == 0 {
		return
	}
	if len(reports) > len(wire) {
		wire = make([]C.me_report_t, len(reports))
	}
	for i, r := range reports {
		wire[i] = C.me_report_t{
			_type: C.uint8_t(r.Type), side: C.uint8_t(r.Side), sequence_number: C.uint64_t(r.Seq),
			order_id: C.uint64_t(r.OrderID), price_ticks: C.int64_t(r.Price), quantity: C.uint32_t(r.Qty),
			maker_order_id: C.uint64_t(r.MakerID), taker_order_id: C.uint64_t(r.TakerID),
		}
	}
	C.push(transport, sink, &wire[0], C.uint32_t(len(reports)))
	reports = reports[:0]
}

func main() {}
