package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeTree writes files, named by their paths below dir, creating the
// directories they need.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// readTree returns the content of every file below dir by its slash-separated
// path, and each directory as its path with a final slash and no content.
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			tree[rel+"/"] = ""
			return nil
		}
		data, err := os.ReadFile(path)
		tree[rel] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// slimOf makes the slim copy of a directory holding files and returns what
// the copy holds.
func slimOf(t *testing.T, files map[string]string) map[string]string {
	t.Helper()
	in := t.TempDir()
	writeTree(t, in, files)
	out := filepath.Join(t.TempDir(), "out")
	if err := slim(in, out); err != nil {
		t.Fatal(err)
	}
	return readTree(t, out)
}

// compare reports every file in which got differs from want.
func compare(t *testing.T, got, want map[string]string) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if g, ok := got[name]; !ok {
			t.Errorf("%s is missing", name)
		} else if g != want[name] {
			t.Errorf("%s:\n got %q\nwant %q", name, g, want[name])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(got)) {
		if _, ok := want[name]; !ok {
			t.Errorf("%s is there but should not be", name)
		}
	}
}

// indentedReport is a report as loadgen writes it, curves included. Its
// numbers are ones that a trip through float64 would write differently, and
// its problem has characters that json.Marshal would escape.
const indentedReport = `{
  "mode": "oe",
  "target_rate": 10000,
  "achieved_rate": 9999.987654321,
  "valid": true,
  "problems": [
    "p99.9 <= 20 & more"
  ],
  "latency_us": {
    "reply": {
      "count": 123456789012345678,
      "p50": 87.951,
      "p99": 1e-07,
      "max": 2066.672,
      "curve": [
        [
          0,
          0.031
        ],
        [
          100,
          2066.672
        ]
      ]
    },
    "send_lag": {
      "count": 29937,
      "p50": 0.143,
      "p99": 31.191,
      "max": 1E+3,
      "curve": [
        [
          0,
          0.143
        ]
      ]
    }
  }
}
`

// slimmedReport is indentedReport as the slim copy holds it.
const slimmedReport = `{"achieved_rate":9999.987654321,"latency_us":{` +
	`"reply":{"count":123456789012345678,"max":2066.672,"p50":87.951,"p99":1e-07},` +
	`"send_lag":{"count":29937,"max":1E+3,"p50":0.143,"p99":31.191}},` +
	`"mode":"oe","problems":["p99.9 <= 20 & more"],"target_rate":10000,"valid":true}` + "\n"

func TestSlimReportLosesOnlyItsCurves(t *testing.T) {
	got := slimOf(t, map[string]string{"run-1.json": indentedReport})["run-1.json"]
	if strings.Contains(got, `"curve"`) {
		t.Errorf("a curve is left in %s", got)
	}
	if got != slimmedReport {
		t.Errorf("got  %s\nwant %s", got, slimmedReport)
	}
}

func TestSlimKeepsTheSeries(t *testing.T) {
	const series = `{"window_s":1,"start":"2026-11-23T05:55:32.123456789Z","n":[10000,9998],"rows":{` +
		`"reply":{"p50":[45.1,45],"p99":[110.2,111],"p99_9":[260,261.5],"max":[3100.3,2900]},` +
		`"send_lag":{"p50":[0.3,0.4],"p99":[12,13.5],"p99_9":[60,61],"max":[400,399.9]}}}`
	var indented bytes.Buffer
	if err := json.Indent(&indented, []byte(series), "  ", "  "); err != nil {
		t.Fatal(err)
	}
	report := fmt.Sprintf(`{"valid": true, "latency_us": {"reply": {"p50": 1, "curve": [[0, 1]]}}, "series": %s}`, indented.String())
	got := slimOf(t, map[string]string{"runs/01.json": report})["runs/01.json"]
	if !strings.Contains(got, `"series":`+series) {
		t.Errorf("the series is not in the copy as it was written: %s", got)
	}
	if strings.Contains(got, `"curve"`) {
		t.Errorf("a curve is left in %s", got)
	}
}

func TestSlimLeavesOutTablesOfReports(t *testing.T) {
	got := slimOf(t, map[string]string{
		"run-1.json":   indentedReport,
		"run-1.txt":    "table of run 1",
		"run-2.txt":    "no report next to it, so this is all there is of run 2",
		"summary.json": `{"runs": 3}`,
		"summary.txt":  "summary",
		"env.txt":      "env",
		"list.json":    `{"latency_us": 5}`,
		"list.txt":     "its json has no latency_us object",
		"null.json":    `{"latency_us": null}`,
		"null.txt":     "nor has this",
		// The sibling is the file of the same name in the same directory.
		"a/x.txt":        "kept",
		"b/x.json":       indentedReport,
		"runs/01-F.json": indentedReport,
		"runs/01-F.txt":  "table",
	})
	names := slices.Sorted(maps.Keys(got))
	want := []string{"a/", "a/x.txt", "b/", "b/x.json", "env.txt", "list.json", "list.txt", "log-copy.txt",
		"null.json", "null.txt", "run-1.json", "run-2.txt", "runs/", "runs/01-F.json", "summary.json", "summary.txt"}
	if !slices.Equal(names, want) {
		t.Errorf("the copy holds\n %v\nwant\n %v", names, want)
	}
}

func TestSlimCopiesOtherFilesByteForByte(t *testing.T) {
	files := map[string]string{
		"runs.jsonl":   "{\"seq\":1}\r\n{\"latency_us\":{\"reply\":{\"curve\":[]}}}\n{\"seq\":3} \xff\xfe",
		"summary.json": "{ \"runs\" : 1e-07,\n\t\"configs\": {\"R1\": {\"curve\": [[1, 2]]}} }",
		"plan.json":    "[1,\n 2]\n",
		"g2.json":      "null",
		"scalar.json":  `"curve"`,
		"odd.json":     `{"latency_us": 5, "curve": [1]}`,
		"engine.log":   "engine \x00\x01 stopped\n\n",
		"env.txt":      "go: go1.27.4\nlscpu\n",
		"empty.log":    "",
	}
	compare(t, slimOf(t, files), mergeNote(files))
}

// mergeNote returns files with the file that a slim copy adds.
func mergeNote(files map[string]string) map[string]string {
	want := maps.Clone(files)
	want["log-copy.txt"] = logNote
	return want
}

func TestSlimKeepsRelativePaths(t *testing.T) {
	files := map[string]string{
		"summary.txt":              "s",
		"runs/01-F-r1.engine.log":  "log",
		"runs/a/b/deep.json":       indentedReport,
		"runs/a/b/deep.txt":        "table",
		"runs/a/b/deep.engine.log": "deep log",
		"runs/other/note.txt":      "note",
	}
	in := t.TempDir()
	writeTree(t, in, files)
	if err := os.Mkdir(filepath.Join(in, "runs", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out")
	if err := slim(in, out); err != nil {
		t.Fatal(err)
	}
	compare(t, readTree(t, out), map[string]string{
		"summary.txt":              "s",
		"log-copy.txt":             logNote,
		"runs/":                    "",
		"runs/01-F-r1.engine.log":  "log",
		"runs/a/":                  "",
		"runs/a/b/":                "",
		"runs/a/b/deep.json":       slimmedReport,
		"runs/a/b/deep.engine.log": "deep log",
		"runs/empty/":              "",
		"runs/other/":              "",
		"runs/other/note.txt":      "note",
	})
}

func TestSlimNamesAFileThatIsNotJSON(t *testing.T) {
	for name, content := range map[string]string{
		"truncated":    `{"latency_us": {`,
		"trailing":     `{"valid": true} x`,
		"empty":        ``,
		"not a report": `{not json}`,
	} {
		t.Run(name, func(t *testing.T) {
			in := t.TempDir()
			writeTree(t, in, map[string]string{"summary.txt": "s", "runs/bad.json": content})
			err := slim(in, filepath.Join(t.TempDir(), "out"))
			if err == nil {
				t.Fatal("slim succeeded")
			}
			if want := filepath.Join(in, "runs", "bad.json"); !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name the file %s: %v", want, err)
			}
		})
	}
}

func TestSlimNamesAReportWhoseRowIsNotAnObject(t *testing.T) {
	in := t.TempDir()
	writeTree(t, in, map[string]string{"run-1.json": `{"latency_us": {"reply": 5}}`})
	err := slim(in, filepath.Join(t.TempDir(), "out"))
	if err == nil || !strings.Contains(err.Error(), filepath.Join(in, "run-1.json")) || !strings.Contains(err.Error(), `"reply"`) {
		t.Errorf("want an error that names the file and the row, got %v", err)
	}
}

func TestSlimNamesATableWhoseReportIsNotJSON(t *testing.T) {
	// The table may not be left out on the word of a file that is no report.
	in := t.TempDir()
	writeTree(t, in, map[string]string{"run-1.json": `{"latency_us": {`, "run-1.txt": "table"})
	err := slim(in, filepath.Join(t.TempDir(), "out"))
	if err == nil || !strings.Contains(err.Error(), "run-1.json") {
		t.Errorf("want an error that names run-1.json, got %v", err)
	}
}

func TestSlimRefusesAnOutThatExists(t *testing.T) {
	in := t.TempDir()
	writeTree(t, in, map[string]string{"summary.txt": "s"})
	for name, setup := range map[string]func(string){
		"directory": func(out string) { writeTree(t, out, map[string]string{"keep.txt": "mine"}) },
		"file":      func(out string) { writeTree(t, filepath.Dir(out), map[string]string{filepath.Base(out): "mine"}) },
	} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out")
			setup(out)
			before := readTree(t, filepath.Dir(out))
			if err := slim(in, out); !errors.Is(err, fs.ErrExist) {
				t.Errorf("want an error that says out exists, got %v", err)
			}
			compare(t, readTree(t, filepath.Dir(out)), before)
		})
	}
}

func TestSlimOfAMissingInMakesNothing(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	if err := slim(filepath.Join(t.TempDir(), "missing"), out); err == nil {
		t.Error("slim of a directory that does not exist succeeded")
	}
	if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("out was made: %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	writeTree(t, filepath.Dir(file), map[string]string{"file": "x"})
	if err := slim(file, out); err == nil {
		t.Error("slim of a file succeeded")
	}
}

func TestSlimOfAnEmptyInHoldsOnlyTheNote(t *testing.T) {
	got := slimOf(t, nil)
	compare(t, got, map[string]string{"log-copy.txt": logNote})
	lines := strings.Split(strings.TrimSuffix(got["log-copy.txt"], "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("the note has %d lines, want 2: %q", len(lines), got["log-copy.txt"])
	}
	for _, want := range []string{"curves", "text tables"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the first line does not say that %s are left out: %q", want, lines[0])
		}
	}
	for _, want := range []string{"results.tgz", "port 8000"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("the second line does not name %s: %q", want, lines[1])
		}
	}
}

func TestSlimDoesNotOverwriteItsNote(t *testing.T) {
	in := t.TempDir()
	writeTree(t, in, map[string]string{"log-copy.txt": "mine"})
	err := slim(in, filepath.Join(t.TempDir(), "out"))
	if !errors.Is(err, fs.ErrExist) || !strings.Contains(err.Error(), "log-copy.txt") {
		t.Errorf("want an error that says log-copy.txt exists, got %v", err)
	}
}

func TestSlimRefusesWhatIsNotAFileOrDirectory(t *testing.T) {
	in := t.TempDir()
	writeTree(t, in, map[string]string{"summary.txt": "s"})
	if err := os.Symlink("summary.txt", filepath.Join(in, "link.txt")); err != nil {
		t.Skip("no symbolic links here:", err)
	}
	err := slim(in, filepath.Join(t.TempDir(), "out"))
	if err == nil || !strings.Contains(err.Error(), "link.txt") {
		t.Errorf("want an error that names link.txt, got %v", err)
	}
}

func TestRunSlim(t *testing.T) {
	in := t.TempDir()
	writeTree(t, in, map[string]string{"run-1.json": indentedReport, "run-1.txt": "table", "env.txt": "env"})
	out := filepath.Join(t.TempDir(), "out")
	if err := run(context.Background(), []string{"slim", "-in", in, "-out", out}, io.Discard); err != nil {
		t.Fatal(err)
	}
	compare(t, readTree(t, out), map[string]string{
		"run-1.json":   slimmedReport,
		"env.txt":      "env",
		"log-copy.txt": logNote,
	})
	for _, args := range [][]string{
		{"slim"},
		{"slim", "-in", in},
		{"slim", "-out", out + "2"},
		{"slim", "-in", in, "-out", out + "3", "extra"},
	} {
		err := run(context.Background(), args, io.Discard)
		if err == nil {
			t.Errorf("run %v succeeded", args)
		}
	}
	if err := run(context.Background(), []string{"slim"}, io.Discard); err == nil || !strings.Contains(err.Error(), "pairrun slim -in DIR -out DIR") {
		t.Errorf("the usage does not show slim: %v", err)
	}
}
