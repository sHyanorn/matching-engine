package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

// The server sets a command's Kind and OrderID, so a command invalid in
// either is the server's fault (D009); every other reason is the client's.
func TestInvalidCommandStatus(t *testing.T) {
	for _, tc := range []struct {
		reason book.Reason
		status int
		code   string
	}{
		{book.ReasonInvalidCommand, http.StatusInternalServerError, "internal"},
		{book.ReasonInvalidOrderID, http.StatusInternalServerError, "internal"},
		{book.ReasonInvalidPrice, http.StatusBadRequest, "invalid_price"},
	} {
		status, code := engineStatus(&engine.InvalidError{Reason: tc.reason})
		if status != tc.status || code != tc.code {
			t.Errorf("%v: %d %s, want %d %s", tc.reason, status, code, tc.status, tc.code)
		}
	}
}

// An unexpected engine error is logged, and the client gets only its code.
func TestUnexpectedErrorHidesDetail(t *testing.T) {
	var logged bytes.Buffer
	s := &Server{opts: Options{Logger: slog.New(slog.NewTextHandler(&logged, nil))}}
	rec := httptest.NewRecorder()
	s.writeEngineError(rec, errors.New("open /srv/engine/ids.tmp: is a directory"))

	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError || body.Error.Code != "internal" || strings.Contains(rec.Body.String(), "/srv") {
		t.Errorf("reply %d %s, want 500 internal without the error's detail", rec.Code, rec.Body)
	}
	if !strings.Contains(logged.String(), "ids.tmp") {
		t.Errorf("log %q lacks the error", logged.String())
	}
}
