package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

const (
	maxBodyBytes = 4 << 10
	defaultDepth = 20
	maxDepth     = 1000
)

func (s *Server) placeOrder(w http.ResponseWriter, r *http.Request) {
	var req orderRequest
	if !decodeBody(w, r, &req) {
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
	res, err := s.eng.PlaceOrder(r.Context(), engine.NewOrder{
		Symbol: req.Symbol, Side: side, Type: typ, TIF: tif,
		Price: book.Price(req.Price), Qty: book.Qty(req.Qty), ClientID: req.ClientID,
	})
	if !errors.Is(err, engine.ErrUnknownSymbol) {
		s.countOrder(req.Symbol, res, err)
	}
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, orderResponse{Order: toOrderJSON(res.Status), Events: toEventsJSON(res.Events)})
}

func (s *Server) countOrder(symbol string, res engine.Result, err error) {
	result := "accepted"
	switch status, _ := engineStatus(err); {
	case err == nil && len(res.Events) > 0 && res.Events[0].Kind == book.EventRejected:
		result = "rejected"
	case err == nil:
	case status == http.StatusBadRequest:
		result = "invalid"
	case errors.Is(err, engine.ErrBusy):
		result = "busy"
	default:
		result = "failed"
	}
	s.metrics.orders.WithLabelValues(symbol, result).Inc()
	s.countTrades(symbol, res.Events)
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
	symbol, issued := s.eng.SymbolOf(id)
	res, err := s.eng.CancelOrder(r.Context(), id)
	count := func(result string) {
		if issued {
			s.metrics.cancels.WithLabelValues(symbol, result).Inc()
		}
	}
	if err != nil {
		switch status, _ := engineStatus(err); {
		case errors.Is(err, engine.ErrUnknownOrder):
			count("unknown")
		case status == http.StatusBadRequest:
			count("invalid")
		case errors.Is(err, engine.ErrBusy):
			count("busy")
		default:
			count("failed")
		}
		writeEngineError(w, err)
		return
	}
	if res.Events[0].Kind == book.EventRejected { // not resting
		if res.Status.ID != 0 && res.Status.Finished() {
			count("finished")
			writeError(w, http.StatusConflict, "order_finished", fmt.Sprintf("order %d is %v", id, res.Status.State))
			return
		}
		count("unknown")
		writeError(w, http.StatusNotFound, "unknown_order", fmt.Sprintf("order %d is unknown", id))
		return
	}
	count("canceled")
	writeJSON(w, http.StatusOK, orderResponse{Order: toOrderJSON(res.Status), Events: toEventsJSON(res.Events)})
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathOrderID(w, r)
	if !ok {
		return
	}
	st, found, err := s.eng.Order(r.Context(), id)
	switch {
	case err != nil:
		writeEngineError(w, err)
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
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bookResponse{Symbol: symbol, Seq: d.LastSeq, Bids: toLevelsJSON(d.Bids), Asks: toLevelsJSON(d.Asks)})
}

// decodeBody decodes one JSON object from a body of at most maxBodyBytes,
// rejecting unknown fields and trailing data, and writes the error if it
// fails.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errors.New("body must hold exactly one JSON object")
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("body is over %d bytes", maxBodyBytes))
	case err != nil:
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		return true
	}
	return false
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
