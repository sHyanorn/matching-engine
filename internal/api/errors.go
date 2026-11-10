package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// writeJSON writes v with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client may be gone; nothing to do
}

// writeError writes the error shape of docs/API.md.
func writeError(w http.ResponseWriter, status int, code, message string) {
	var b errorBody
	b.Error.Code, b.Error.Message = code, message
	if status == http.StatusServiceUnavailable && code == "busy" {
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, status, b)
}

// engineStatus maps an engine error to an HTTP status and error code.
func engineStatus(err error) (int, string) {
	var inv *engine.InvalidError
	switch {
	case errors.As(err, &inv):
		// The engine sets Kind and a new order's ID itself, and a cancel's
		// ID is checked before it gets there: a server bug (D009).
		if inv.Reason == book.ReasonInvalidCommand || inv.Reason == book.ReasonInvalidOrderID {
			return http.StatusInternalServerError, "internal"
		}
		return http.StatusBadRequest, inv.Reason.String()
	case errors.Is(err, engine.ErrClientIDLen):
		return http.StatusBadRequest, "invalid_client_id"
	case errors.Is(err, engine.ErrUnknownSymbol):
		return http.StatusNotFound, "unknown_symbol"
	case errors.Is(err, engine.ErrUnknownOrder):
		return http.StatusNotFound, "unknown_order"
	case errors.Is(err, engine.ErrBusy):
		return http.StatusServiceUnavailable, "busy"
	case errors.Is(err, engine.ErrClosed), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, "shutting_down"
	case errors.Is(err, engine.ErrLogFailed):
		return http.StatusInternalServerError, "log_failed"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

// writeEngineError writes err, which the engine returned for a command it
// did not apply. An unexpected error is logged, and the client gets no
// detail.
func (s *Server) writeEngineError(w http.ResponseWriter, err error) {
	status, code := engineStatus(err)
	msg := err.Error()
	if code == "internal" {
		s.opts.Logger.Error("request failed", "err", err)
		msg = "internal error"
	}
	writeError(w, status, code, msg)
}

// writeAppliedFailure writes err, a log failure, for a command that took
// effect: the error together with the order and events.
func (s *Server) writeAppliedFailure(w http.ResponseWriter, err error, res engine.Result) {
	status, code := engineStatus(err)
	var b appliedFailure
	b.Error.Code, b.Error.Message = code, err.Error()
	if res.Status.ID != 0 {
		o := toOrderJSON(res.Status)
		b.Order = &o
	}
	b.Events = toEventsJSON(res.Events)
	writeJSON(w, status, b)
}
