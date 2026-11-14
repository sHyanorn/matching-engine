package harness

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestConformance replays the 34 cases of the benchmark's
// scripts/conformance_check.py from the workload files the benchmark reads
// (testdata/conformance/*.bin) and compares the canonical report stream
// with the one Liquibook, CppTrader and geseq/orderbook each produced for
// the case (*.txt), as the benchmark's --write-canonical-output writes it
// (scripts/harness.sh cases).
func TestConformance(t *testing.T) {
	files, err := filepath.Glob("testdata/conformance/*.bin")
	if err != nil || len(files) != 34 {
		t.Fatalf("found %d cases (%v), want 34", len(files), err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".bin")
		t.Run(name, func(t *testing.T) {
			want, err := os.ReadFile(strings.TrimSuffix(f, ".bin") + ".txt")
			if err != nil {
				t.Fatal(err)
			}
			e := New()
			var reports []Report
			for _, m := range readWorkload(t, f) {
				if reports, err = e.Apply(m, reports); err != nil {
					t.Fatal(err)
				}
			}
			got := canonical(reports)
			if got != string(want) {
				g, w := strings.Split(got, "\n"), strings.Split(string(want), "\n")
				i := 0
				for i < min(len(g), len(w)) && g[i] == w[i] {
					i++
				}
				t.Fatalf("line %d: got %q, want %q (%d and %d lines)", i+1, at(g, i), at(w, i), len(g), len(w))
			}
		})
	}
}

func at(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return "<end>"
}

// readWorkload reads a workload file of the benchmark: a 16-byte header,
// [u64 magic][u32 version][u32 count], then count 40-byte records,
// [u8 type][u8 side][u8 ioc][u8 pad][u32 quantity][u64 sequence_number]
// [u64 order_id][i64 price_ticks][i64 reserved], little-endian.
func readWorkload(t *testing.T, path string) []Msg {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	if len(b) < 16 || le.Uint64(b) != 0x4D4542575F303031 || len(b) != 16+40*int(le.Uint32(b[12:])) {
		t.Fatalf("%s: not a workload file of %d bytes", path, len(b))
	}
	var msgs []Msg
	for r := b[16:]; len(r) > 0; r = r[40:] {
		msgs = append(msgs, Msg{
			Type: MsgType(r[0]), Side: r[1], IOC: r[2] != 0, Qty: le.Uint32(r[4:]),
			Seq: le.Uint64(r[8:]), OrderID: le.Uint64(r[16:]), Price: int64(le.Uint64(r[24:])),
		})
	}
	return msgs
}

// canonical returns the benchmark's canonical form of reports: sorted by
// sequence number and then type, keeping the order of equal keys, one line
// per report in the format of its type, joined by newlines with none at
// the end.
func canonical(reports []Report) string {
	reports = slices.Clone(reports)
	slices.SortStableFunc(reports, func(a, b Report) int {
		return cmp.Or(cmp.Compare(a.Seq, b.Seq), cmp.Compare(a.Type, b.Type))
	})
	lines := make([]string, len(reports))
	for i, r := range reports {
		switch r.Type {
		case OrderAck, ModifyAck:
			lines[i] = fmt.Sprintf("%d,%d,%d,%d,%d,%d", r.Type, r.Seq, r.Side, r.OrderID, r.Price, r.Qty)
		case Trade:
			lines[i] = fmt.Sprintf("%d,%d,%d,%d,%d,%d", r.Type, r.Seq, r.Price, r.Qty, r.MakerID, r.TakerID)
		case CancelAck:
			lines[i] = fmt.Sprintf("%d,%d,%d,%d,%d", r.Type, r.Seq, r.Side, r.OrderID, r.Price)
		case CancelReject, ModifyReject:
			lines[i] = fmt.Sprintf("%d,%d,%d", r.Type, r.Seq, r.OrderID)
		}
	}
	return strings.Join(lines, "\n")
}
