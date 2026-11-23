package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

// jsonKeys lists the keys of the JSON object that v encodes to, in order.
func jsonKeys(v any) ([]string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if _, err := dec.Token(); err != nil { // the opening brace
		return nil, err
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		keys = append(keys, k.(string))
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// cannedReport is loadgen's JSON for a run, as the tests make it.
type cannedReport struct {
	Valid            bool
	Problems         []string
	Target, Achieved float64
	Outcomes         map[string]int
	Trades           tradeCounts
	Latency          map[string]latencyRow
	Series           any // the report's "series", if it has one
}

type rowWithCurve struct {
	latencyRow
	Curve [][2]float64 `json:"curve"`
}

// JSON writes the report as loadgen does, curves included.
func (r cannedReport) JSON() []byte {
	latency := map[string]rowWithCurve{}
	for k, v := range r.Latency {
		latency[k] = rowWithCurve{v, [][2]float64{{50, v.P50}, {100, v.Max}}}
	}
	report := map[string]any{
		"mode": "oe", "valid": r.Valid, "problems": r.Problems, "target_rate": r.Target, "achieved_rate": r.Achieved,
		"outcomes": r.Outcomes, "trades": r.Trades, "latency_us": latency,
	}
	if r.Series != nil {
		report["series"] = r.Series
	}
	data, err := json.Marshal(report)
	if err != nil {
		panic(err)
	}
	return data
}

func row(p50, p99 float64) latencyRow {
	return latencyRow{Count: 600000, P50: p50, P90: (p50 + p99) / 2, P99: p99, P999: p99 * 1.5, P9999: p99 * 3, Max: p99 * 10}
}

// flowRows are the latency rows that loadgen reports for a flow, with the
// given p50 and p99 in the row the study cares about: pong for F, reply for
// the others. The send lag is 5 us at p99.9, inside the bound.
func flowRows(flow string, p50, p99 float64) map[string]latencyRow {
	rows := map[string]latencyRow{
		"send_lag": {Count: 600000, P50: 1, P90: 2, P99: 4, P999: 5, P9999: 6, Max: 9},
		"hiccup":   row(1, 3),
	}
	switch flow {
	case flowF:
		rows["pong"] = row(p50, p99)
	case flowR2:
		rows["reply"], rows["cancel"], rows["order_to_cancel"] = row(p50, p99), row(p50+1, p99+1), row(p50+2, p99+2)
	case flowT, flowREST:
		rows["reply"], rows["service"], rows["trade_push"] = row(p50, p99), row(p50-1, p99-1), row(p50+3, p99+3)
	default:
		rows["reply"], rows["service"] = row(p50, p99), row(p50-1, p99-1)
	}
	return rows
}

// flowOutcomes are outcomes that a valid run of a flow reports.
func flowOutcomes(flow string) map[string]int {
	switch flow {
	case flowF:
		return map[string]int{"ping": 600000, "pong": 600000}
	case flowR1:
		return map[string]int{"order_rested": 600000}
	}
	return map[string]int{"order_rested": 420000, "order_traded": 60000, "cancel_canceled": 100000, "cancel_finished": 15000, "cancel_unknown": 5000, "cancel_skipped": 10}
}

// validReport is a report that every rule accepts.
func validReport(flow string, p50, p99 float64) cannedReport {
	return cannedReport{
		Valid: true, Target: 10000, Achieved: 10000, Outcomes: flowOutcomes(flow),
		Trades:  tradeCounts{Replies: 5},
		Latency: flowRows(flow, p50, p99),
	}
}

// decodeReport parses canned JSON as the runner reads it.
func decodeReport(t *testing.T, data []byte) *loadgenReport {
	t.Helper()
	var r loadgenReport
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return &r
}
