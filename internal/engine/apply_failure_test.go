package engine_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sHyanorn/matching-engine/internal/api"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

// An order that took effect before its log failed is reported with its ID
// and events, and counted as accepted (D025). A later command is not
// applied and gets the plain error. The failure shows in /healthz and
// /metrics.
func TestAppliedOrderReportedWhenSyncFails(t *testing.T) {
	e, err := engine.New(engine.Config{Symbols: []string{"A"}, DataDir: t.TempDir(), SyncInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Close(); !errors.Is(err, engine.ErrLogFailed) {
			t.Errorf("Close = %v, want ErrLogFailed", err)
		}
	})
	engine.FailSyncs(e)
	hs := httptest.NewServer(api.New(e, api.Options{}).Handler())
	t.Cleanup(hs.Close)

	type reply struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Order *struct {
			OrderID uint64 `json:"order_id"`
			Status  string `json:"status"`
		} `json:"order"`
		Events []json.RawMessage `json:"events"`
	}
	request := func(method, path, body string) (int, reply, string) {
		t.Helper()
		req, err := http.NewRequest(method, hs.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := hs.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		var r reply
		_ = json.Unmarshal(data, &r) // checked through its fields
		return resp.StatusCode, r, string(data)
	}

	status, r, body := request("POST", "/v1/orders", `{"symbol":"A","side":"buy","type":"limit","price":100,"qty":1}`)
	if status != http.StatusInternalServerError || r.Error.Code != "log_failed" {
		t.Fatalf("got %d %s, want 500 log_failed", status, body)
	}
	if r.Order == nil || r.Order.OrderID == 0 || r.Order.Status != "open" || len(r.Events) != 2 {
		t.Fatalf("reply %s, want the open order and its 2 events", body)
	}

	status, r, body = request("DELETE", "/v1/orders/"+jsonNumber(r.Order.OrderID), "")
	if status != http.StatusInternalServerError || r.Error.Code != "log_failed" || r.Order != nil || r.Events != nil {
		t.Fatalf("cancel after the failure: %d %s, want 500 log_failed with no order or events", status, body)
	}

	// The failure shows in /healthz, which stays 200 (D029), and in a gauge.
	if status, _, body := request("GET", "/healthz", ""); status != http.StatusOK || body != `{"status":"degraded","log_failed":["A"]}`+"\n" {
		t.Errorf("/healthz after the failure: %d %s, want 200 degraded with A", status, body)
	}
	_, _, metrics := request("GET", "/metrics", "")
	for _, want := range []string{
		`matching_engine_orders_total{result="accepted",symbol="A"} 1`,
		`matching_engine_orders_total{result="failed",symbol="A"} 0`,
		`matching_engine_cancels_total{result="failed",symbol="A"} 1`,
		`matching_engine_log_failed{symbol="A"} 1`,
	} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("metrics lack %s", want)
		}
	}
}

func jsonNumber(n uint64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
