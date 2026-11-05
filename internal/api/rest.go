package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

const (
	maxBodyBytes = 4 << 10
	defaultDepth = 20
	maxDepth     = 1000
)

var orderFields = jsonNames(orderRequest{})

func (s *Server) placeOrder(w http.ResponseWriter, r *http.Request) {
	var req orderRequest
	if !decodeBody(w, r, &req, orderFields) {
		return
	}
	// Unknown names stay zero, and validation reports them (D008).
	side, _ := book.ParseSide(req.Side)
	typ, _ := book.ParseOrderType(req.Type)
	tif, _ := book.ParseTIF(req.TIF)
	if req.TIF == "" {
		switch typ {
		case book.Limit:
			tif = book.GTC
		case book.Market:
			tif = book.IOC
		}
	}
	var res engine.Result
	err := engine.ErrClosed
	if !s.stopping.Load() { // see BeginShutdown
		res, err = s.eng.PlaceOrder(r.Context(), engine.NewOrder{
			Symbol: req.Symbol, Side: side, Type: typ, TIF: tif,
			Price: book.Price(req.Price), Qty: book.Qty(req.Qty), ClientID: req.ClientID,
		})
	}
	if s.symbols[req.Symbol] {
		s.metrics.orders.WithLabelValues(req.Symbol, orderResult(res, err)).Inc()
		s.countTrades(req.Symbol, res.Events)
	}
	switch {
	case err != nil && len(res.Events) > 0:
		s.writeAppliedFailure(w, err, res)
	case err != nil:
		s.writeEngineError(w, err)
	default:
		writeJSON(w, http.StatusOK, orderResponse{Order: toOrderJSON(res.Status), Events: toEventsJSON(res.Events)})
	}
}

// orderResult is the result label of a new order. A command with events
// reached the book, even if its log failed afterwards.
func orderResult(res engine.Result, err error) string {
	status, _ := engineStatus(err)
	switch {
	case len(res.Events) > 0 && res.Events[0].Kind == book.EventRejected:
		return "rejected"
	case len(res.Events) > 0:
		return "accepted"
	case status == http.StatusBadRequest:
		return "invalid"
	case errors.Is(err, engine.ErrBusy):
		return "busy"
	default:
		return "failed"
	}
}

func (s *Server) countTrades(symbol string, evs []book.Event) {
	n := 0
	for _, e := range evs {
		if e.Kind == book.EventTrade {
			n++
		}
	}
	if n > 0 {
		s.metrics.trades.WithLabelValues(symbol).Add(float64(n))
	}
}

func (s *Server) cancelOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathOrderID(w, r)
	if !ok {
		return
	}
	var res engine.Result
	err := engine.ErrClosed
	if !s.stopping.Load() { // see BeginShutdown
		res, err = s.eng.CancelOrder(r.Context(), id)
	}
	result := cancelResult(res, err)
	if symbol, issued := s.eng.SymbolOf(id); issued {
		s.metrics.cancels.WithLabelValues(symbol, result).Inc()
	}
	switch {
	case err != nil && len(res.Events) > 0:
		s.writeAppliedFailure(w, err, res)
	case err != nil:
		s.writeEngineError(w, err)
	case result == "finished":
		writeError(w, http.StatusConflict, "order_finished", fmt.Sprintf("order %d is %v", id, res.Status.State))
	case result == "unknown":
		writeError(w, http.StatusNotFound, "unknown_order", fmt.Sprintf("order %d is unknown", id))
	default:
		writeJSON(w, http.StatusOK, orderResponse{Order: toOrderJSON(res.Status), Events: toEventsJSON(res.Events)})
	}
}

// cancelResult is the result label of a cancel. A cancel of an order that
// is not resting reaches the book as a rejection.
func cancelResult(res engine.Result, err error) string {
	switch {
	case len(res.Events) > 0 && res.Events[0].Kind != book.EventRejected:
		return "canceled"
	case len(res.Events) > 0 && res.Status.ID != 0 && res.Status.Finished():
		return "finished"
	case len(res.Events) > 0, errors.Is(err, engine.ErrUnknownOrder):
		return "unknown"
	case errors.Is(err, engine.ErrBusy):
		return "busy"
	default:
		return "failed"
	}
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathOrderID(w, r)
	if !ok {
		return
	}
	st, found, err := s.eng.Order(r.Context(), id)
	switch {
	case err != nil:
		s.writeEngineError(w, err)
	case !found:
		writeError(w, http.StatusNotFound, "unknown_order", fmt.Sprintf("order %d is unknown", id))
	default:
		writeJSON(w, http.StatusOK, toOrderJSON(st))
	}
}

func (s *Server) getBook(w http.ResponseWriter, r *http.Request) {
	depth := defaultDepth
	if q := r.URL.Query().Get("depth"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 || n > maxDepth {
			writeError(w, http.StatusBadRequest, "invalid_depth", fmt.Sprintf("depth must be 1 to %d", maxDepth))
			return
		}
		depth = n
	}
	symbol := r.PathValue("symbol")
	d, err := s.eng.Depth(r.Context(), symbol, depth)
	if err != nil {
		s.writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bookResponse{Symbol: symbol, Seq: d.LastSeq, Bids: toLevelsJSON(d.Bids), Asks: toLevelsJSON(d.Asks)})
}

// decodeBody decodes a body of at most maxBodyBytes holding one JSON
// object into v, and writes the error if it fails. The object's keys must
// be in fields, exactly, and appear once: encoding/json alone would match
// "QTY" to qty and let a repeated key override an earlier one.
func decodeBody(w http.ResponseWriter, r *http.Request, v any, fields map[string]uint) bool {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err == nil {
		err = checkKeys(data, fields)
	}
	if err == nil {
		err = json.Unmarshal(data, v)
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		// The handler's ResponseWriter is wrapped, which hides the hook
		// MaxBytesReader uses to close the connection instead of reading
		// the rest of the body; ask for it here.
		w.Header().Set("Connection", "close")
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("body is over %d bytes", maxBodyBytes))
	case err != nil:
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		return true
	}
	return false
}

// checkKeys checks that data is one JSON object whose keys are in fields
// and unique. json.Valid checks the syntax first, so the scan only has to
// find where each key and value ends. Decoding each value with a
// json.Decoder instead built a syntax error after every number, which
// json.Decoder discards, and cost the engine about 9% of its CPU under
// load (bench-results/m5-opt).
func checkKeys(data []byte, fields map[string]uint) error {
	if !json.Valid(data) {
		return json.Unmarshal(data, new(any)) // the syntax error
	}
	i := skipSpace(data, 0)
	if data[i] != '{' {
		return errors.New("body must be a JSON object")
	}
	var seen uint64
	for i = skipSpace(data, i+1); data[i] != '}'; {
		end := stringEnd(data, i)
		bit, known := fields[string(data[i+1:end-1])]
		if !known && bytes.IndexByte(data[i:end], '\\') >= 0 {
			bit, known = fields[jsonString(data[i:end])]
		}
		switch {
		case !known:
			return fmt.Errorf("unknown field %q", jsonString(data[i:end]))
		case seen&(1<<bit) != 0:
			return fmt.Errorf("repeated field %q", jsonString(data[i:end]))
		}
		seen |= 1 << bit
		colon := skipSpace(data, end)
		i = skipSpace(data, valueEnd(data, skipSpace(data, colon+1)))
		if data[i] == ',' {
			i = skipSpace(data, i+1)
		}
	}
	return nil
}

// jsonString decodes a valid JSON string.
func jsonString(quoted []byte) string {
	var s string
	_ = json.Unmarshal(quoted, &s) // valid, so it decodes
	return s
}

// skipSpace returns the index of the first byte at or after i that is not
// JSON whitespace.
func skipSpace(data []byte, i int) int {
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r') {
		i++
	}
	return i
}

// stringEnd returns the index just past the valid JSON string at data[i].
func stringEnd(data []byte, i int) int {
	for i++; data[i] != '"'; i++ {
		if data[i] == '\\' {
			i++
		}
	}
	return i + 1
}

// valueEnd returns the index just past the valid JSON value at data[i].
func valueEnd(data []byte, i int) int {
	switch data[i] {
	case '"':
		return stringEnd(data, i)
	case '{', '[':
		for depth := 0; ; i++ {
			switch data[i] {
			case '"':
				i = stringEnd(data, i) - 1
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return i + 1
				}
			}
		}
	}
	for ; i < len(data); i++ { // a number, true, false or null
		switch data[i] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			return i
		}
	}
	return i
}

// jsonNames maps the JSON names of a struct's fields to distinct bits, for
// checkKeys.
func jsonNames(v any) map[string]uint {
	t := reflect.TypeOf(v)
	names := make(map[string]uint, t.NumField())
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		names[name] = uint(i)
	}
	return names
}

// pathOrderID parses the {id} path value, and writes the error if it is
// not a positive integer.
func pathOrderID(w http.ResponseWriter, r *http.Request) (book.OrderID, bool) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil || id == 0 {
		writeError(w, http.StatusBadRequest, "invalid_order_id", "order id must be a positive integer")
		return 0, false
	}
	return book.OrderID(id), true
}
