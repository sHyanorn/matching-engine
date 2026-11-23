package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRedactAddresses(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"the remote of an engine log line",
			`{"time":"2026-11-23T05:46:09.28696149Z","level":"INFO","msg":"oe connection closed","session":0,"remote":"10.1.2.3:46376","reason":"EOF"}`,
			`{"time":"2026-11-23T05:46:09.28696149Z","level":"INFO","msg":"oe connection closed","session":0,"remote":"x.x.x.x","reason":"EOF"}`},
		{"both addresses of a network error",
			`read tcp 10.1.2.3:9000->4.5.6.7:8899: connection reset by peer`,
			`read tcp x.x.x.x->x.x.x.x: connection reset by peer`},
		{"an address without a port", "peer 4.5.6.7 left", "peer x.x.x.x left"},
		{"an address at the start and at the end", "4.5.6.7 and 255.255.255.255", "x.x.x.x and x.x.x.x"},
		{"addresses side by side", "10.0.0.1,10.0.0.2;10.0.0.3", "x.x.x.x,x.x.x.x;x.x.x.x"},
		{"an address that ends a sentence", "no route to 4.5.6.7.", "no route to x.x.x.x."},
		{"a port is part of the address only with digits", "4.5.6.7: gone", "x.x.x.x: gone"},
		{"the listen address", "listening on 0.0.0.0:8080 and 0.0.0.0", "listening on 0.0.0.0:8080 and 0.0.0.0"},
		{"loopback", `"remote":"127.0.0.1:46376" 127.0.0.1`, `"remote":"127.0.0.1:46376" 127.0.0.1`},
		{"loopback next to another address", "127.0.0.1:9000->4.5.6.7:8899", "127.0.0.1:9000->x.x.x.x"},
		{"five numbers", "version 1.2.3.4.5 of it", "version 1.2.3.4.5 of it"},
		{"five numbers, the address first", "1.2.3.4.5", "1.2.3.4.5"},
		{"five numbers, the address last", "12.1.2.3.4", "12.1.2.3.4"},
		{"a timestamp", "2026-11-23T05:46:09.271175058Z", "2026-11-23T05:46:09.271175058Z"},
		{"a longer number", "data-3568997236 session 17890137111478110228", "data-3568997236 session 17890137111478110228"},
		{"numbers that are too large or too long", "256.1.1.1 and 1.2.3.456 and 1.2.3.4567 and 991.2.3.4", "256.1.1.1 and 1.2.3.456 and 1.2.3.4567 and 991.2.3.4"},
		{"three numbers", "go1.27.4 and 1.2.3", "go1.27.4 and 1.2.3"},
		{"IPv6 is not redacted", "peer [2001:db8::1]:9000", "peer [2001:db8::1]:9000"},
		{"nothing", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := string(redactAddresses([]byte(c.in))); got != c.want {
				t.Errorf("redacted %q as %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// redactOf makes the masked copy of a directory holding files and returns what
// the copy holds.
func redactOf(t *testing.T, files map[string]string) map[string]string {
	t.Helper()
	in := t.TempDir()
	writeTree(t, in, files)
	out := filepath.Join(t.TempDir(), "out")
	if err := redact(in, out); err != nil {
		t.Fatal(err)
	}
	return readTree(t, out)
}

func TestRedactMasksTheAddressInEveryKindOfFile(t *testing.T) {
	for _, c := range []struct{ name, file, in, want string }{
		{"the args of a run in runs.jsonl", "runs.jsonl",
			`{"seq":1,"config":"F","args":["-oe","203.0.113.7:9000","-http","203.0.113.7:8080","-rate","10000"],"loadgen_exit":0}` + "\n" +
				`{"seq":2,"config":"REST","args":["-addr","203.0.113.7:8080","-duration","3s"],"loadgen_exit":0}` + "\n",
			`{"seq":1,"config":"F","args":["-oe","x.x.x.x","-http","x.x.x.x","-rate","10000"],"loadgen_exit":0}` + "\n" +
				`{"seq":2,"config":"REST","args":["-addr","x.x.x.x","-duration","3s"],"loadgen_exit":0}` + "\n"},
		{"a value of plan.json", "plan.json",
			"{\n  \"configs\": [\n    {\n      \"name\": \"F\",\n      \"oe\": \"203.0.113.7:9000\",\n      \"http\": \"203.0.113.7:8080\"\n    }\n  ]\n}\n",
			"{\n  \"configs\": [\n    {\n      \"name\": \"F\",\n      \"oe\": \"x.x.x.x\",\n      \"http\": \"x.x.x.x\"\n    }\n  ]\n}\n"},
		{"a note that quotes a network error, in runs.jsonl", "runs.jsonl",
			`{"seq":3,"notes":["fetching the engine log: Get \"http://203.0.113.7:9100/engine-log?start=3\": dial tcp 203.0.113.7:9100: connect: connection refused"]}` + "\n",
			`{"seq":3,"notes":["fetching the engine log: Get \"http://x.x.x.x/engine-log?start=3\": dial tcp x.x.x.x: connect: connection refused"]}` + "\n"},
		{"a problem of a loadgen report", "runs/02-R1-r1.json",
			`{"valid":false,"problems":["logging in to 203.0.113.7:9000: dial tcp 203.0.113.7:9000: connect: connection refused"]}`,
			`{"valid":false,"problems":["logging in to x.x.x.x: dial tcp x.x.x.x: connect: connection refused"]}`},
		{"a loadgen text table", "runs/02-R1-r1.txt",
			"OE1 IOC orders on BTC-USD: target 10000/s, achieved 10000/s over 3s after 1s warmup\n" +
				"latency (µs)             count      p50      p90      p99    p99.9   p99.99      max\n" +
				"request to reply         30000     51.6     98.6    346.9   2500.9   3921.2   4221.2\n" +
				"loadgen: logging in to 203.0.113.7:9000: dial tcp 203.0.113.7:9000: connect: connection refused\n",
			"OE1 IOC orders on BTC-USD: target 10000/s, achieved 10000/s over 3s after 1s warmup\n" +
				"latency (µs)             count      p50      p90      p99    p99.9   p99.99      max\n" +
				"request to reply         30000     51.6     98.6    346.9   2500.9   3921.2   4221.2\n" +
				"loadgen: logging in to x.x.x.x: dial tcp x.x.x.x: connect: connection refused\n"},
		{"an address without a port", "notes.txt", "client at 203.0.113.7 reached it\n", "client at x.x.x.x reached it\n"},
		{"an engine log", "runs/01-F-r1.engine.log",
			`{"msg":"oe connection closed","remote":"203.0.113.7:46376"}` + "\n",
			`{"msg":"oe connection closed","remote":"x.x.x.x"}` + "\n"},
		{"a file with no extension", "NOTES", "engine 203.0.113.7:9000\n", "engine x.x.x.x\n"},
		{"a file with an extension of no results file", "engine.out", "engine 203.0.113.7:9000\n", "engine x.x.x.x\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := redactOf(t, map[string]string{c.file: c.in})[filepath.ToSlash(c.file)]
			if got != c.want {
				t.Errorf("%s:\n got %q\nwant %q", c.file, got, c.want)
			}
		})
	}
}

func TestRedactKeepsWhatIsNotAnAddress(t *testing.T) {
	const kept = "listening on 0.0.0.0:8080, client 127.0.0.1:9000, 0.0.0.0 and 127.0.0.1\n" +
		"go: go version go1.27.4 linux/amd64\n" +
		"os: Linux 6.18.44-fc-v726 x86_64, 4 CPUs allowed\n" +
		"started 2026-11-23T05:46:09.271175058Z, session data-3568997236\n" +
		"version 1.2.3.4.5 of it\n"
	got := redactOf(t, map[string]string{
		"env.txt":   kept,
		"mixed.txt": "engine 203.0.113.7:9000 listens on 0.0.0.0:8080, loopback 127.0.0.1:9000, go1.27.4, 1.2.3.4.5\n",
	})
	compare(t, got, map[string]string{
		"env.txt":   kept,
		"mixed.txt": "engine x.x.x.x listens on 0.0.0.0:8080, loopback 127.0.0.1:9000, go1.27.4, 1.2.3.4.5\n",
	})
}

func TestRedactCopiesAFileThatIsNotTextByteForByte(t *testing.T) {
	// Each holds an address that the mask would change.
	files := map[string]string{
		"nul.log":     "engine \x00 203.0.113.7:9000 stopped\n",
		"invalid.log": "engine \xff\xfe 203.0.113.7:9000 stopped\n",
		"cut.log":     "engine 203.0.113.7:9000 \xc2", // a multi-byte character cut short
	}
	compare(t, redactOf(t, files), files)
}

func TestRedactMasksATextFileWithMultibyteCharacters(t *testing.T) {
	got := redactOf(t, map[string]string{"run.txt": "latency (µs) for 日本 at 203.0.113.7:9000\n"})["run.txt"]
	if want := "latency (µs) for 日本 at x.x.x.x\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRedactKeepsTheFilesPermissionBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no permission bits here")
	}
	// 0o664 and 0o640 are what a umask of 022 would clear.
	modes := map[string]os.FileMode{"a-private": 0o600, "b-group": 0o640, "c-shared": 0o664, "d-exec": 0o755, "e-readonly": 0o444}
	in := t.TempDir()
	for name, mode := range modes {
		writeTree(t, in, map[string]string{name: "engine 203.0.113.7:9000\n"})
		if err := os.Chmod(filepath.Join(in, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(t.TempDir(), "out")
	if err := redact(in, out); err != nil {
		t.Fatal(err)
	}
	for name, want := range modes {
		fi, err := os.Stat(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s has mode %v, want %v", name, fi.Mode().Perm(), want)
		}
	}
}

func TestRedactKeepsRelativePaths(t *testing.T) {
	in := t.TempDir()
	writeTree(t, in, map[string]string{
		"summary.txt":              "s",
		"runs/01-F-r1.engine.log":  "from 203.0.113.7:46376",
		"runs/a/b/deep.json":       `{"oe":"203.0.113.7:9000"}`,
		"runs/a/b/deep.engine.log": "deep",
		"runs/other/note.txt":      "note",
	})
	if err := os.Mkdir(filepath.Join(in, "runs", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out")
	if err := redact(in, out); err != nil {
		t.Fatal(err)
	}
	compare(t, readTree(t, out), map[string]string{
		"summary.txt":              "s",
		"runs/":                    "",
		"runs/01-F-r1.engine.log":  "from x.x.x.x",
		"runs/a/":                  "",
		"runs/a/b/":                "",
		"runs/a/b/deep.json":       `{"oe":"x.x.x.x"}`,
		"runs/a/b/deep.engine.log": "deep",
		"runs/empty/":              "",
		"runs/other/":              "",
		"runs/other/note.txt":      "note",
	})
}

func TestRedactOfAnEmptyInMakesAnEmptyOut(t *testing.T) {
	compare(t, redactOf(t, nil), map[string]string{})
}

func TestRedactKeepsJSONValid(t *testing.T) {
	const in = `{"oe":"203.0.113.7:9000","peers":["203.0.113.7","198.51.100.2:8080"],` +
		`"error":"dial tcp 203.0.113.7:9000: connect: connection refused","rate":[1.5,2.25,3],"ok":true}`
	got := redactOf(t, map[string]string{"plan.json": in})["plan.json"]
	var v struct {
		OE    string
		Peers []string
		Error string
		Rate  []float64
	}
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("the masked file is not JSON: %v\n%s", err, got)
	}
	if v.OE != "x.x.x.x" || len(v.Peers) != 2 || v.Peers[0] != "x.x.x.x" || v.Peers[1] != "x.x.x.x" ||
		v.Error != "dial tcp x.x.x.x: connect: connection refused" || len(v.Rate) != 3 || v.Rate[1] != 2.25 {
		t.Errorf("the masked file holds %+v", v)
	}
	if strings.Contains(got, "203.0.113.7") || strings.Contains(got, "198.51.100.2") {
		t.Errorf("an address is left in %s", got)
	}
}

func TestRedactRefusesAnOutThatExists(t *testing.T) {
	in := t.TempDir()
	writeTree(t, in, map[string]string{"summary.txt": "s"})
	for name, setup := range map[string]func(string){
		"directory": func(out string) { writeTree(t, out, map[string]string{"keep.txt": "mine"}) },
		"empty": func(out string) {
			if err := os.Mkdir(out, 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"file": func(out string) { writeTree(t, filepath.Dir(out), map[string]string{filepath.Base(out): "mine"}) },
	} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out")
			setup(out)
			before := readTree(t, filepath.Dir(out))
			if err := redact(in, out); !errors.Is(err, fs.ErrExist) {
				t.Errorf("want an error that says out exists, got %v", err)
			}
			compare(t, readTree(t, filepath.Dir(out)), before)
		})
	}
}

func TestRedactOfAMissingInMakesNothing(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	if err := redact(filepath.Join(t.TempDir(), "missing"), out); err == nil {
		t.Error("redact of a directory that does not exist succeeded")
	}
	if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("out was made: %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	writeTree(t, filepath.Dir(file), map[string]string{"file": "x"})
	if err := redact(file, out); err == nil {
		t.Error("redact of a file succeeded")
	}
}

func TestRedactNamesWhatIsNotAFileOrDirectory(t *testing.T) {
	for name, target := range map[string]string{
		"a link to a file":      "summary.txt",
		"a link to a directory": "runs",
		"a link to nothing":     "missing",
	} {
		t.Run(name, func(t *testing.T) {
			in := t.TempDir()
			writeTree(t, in, map[string]string{"summary.txt": "s", "runs/01.txt": "r"})
			if err := os.Symlink(target, filepath.Join(in, "link")); err != nil {
				t.Skip("no symbolic links here:", err)
			}
			err := redact(in, filepath.Join(t.TempDir(), "out"))
			if want := filepath.Join(in, "link"); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("want an error that names %s, got %v", want, err)
			}
		})
	}
}

func TestRedactDoesNotChangeItsInput(t *testing.T) {
	in := t.TempDir()
	files := map[string]string{
		"runs.jsonl":         `{"args":["-oe","203.0.113.7:9000"]}` + "\n",
		"runs/01.engine.log": "from 203.0.113.7:46376",
		"bin":                "\x00 203.0.113.7",
	}
	writeTree(t, in, files)
	modes := func() map[string]os.FileMode {
		m := map[string]os.FileMode{}
		for name := range files {
			fi, err := os.Stat(filepath.Join(in, name))
			if err != nil {
				t.Fatal(err)
			}
			m[name] = fi.Mode()
		}
		return m
	}
	before, modesBefore := readTree(t, in), modes()
	if err := redact(in, filepath.Join(t.TempDir(), "out")); err != nil {
		t.Fatal(err)
	}
	compare(t, readTree(t, in), before)
	for name, mode := range modes() {
		if mode != modesBefore[name] {
			t.Errorf("%s has mode %v, was %v", name, mode, modesBefore[name])
		}
	}
}

func TestRunRedact(t *testing.T) {
	in := t.TempDir()
	writeTree(t, in, map[string]string{"runs.jsonl": `{"args":["-oe","203.0.113.7:9000"]}` + "\n", "env.txt": "env"})
	out := filepath.Join(t.TempDir(), "out")
	if err := run(context.Background(), []string{"redact", "-in", in, "-out", out}, io.Discard); err != nil {
		t.Fatal(err)
	}
	compare(t, readTree(t, out), map[string]string{"runs.jsonl": `{"args":["-oe","x.x.x.x"]}` + "\n", "env.txt": "env"})
	for _, args := range [][]string{
		{"redact"},
		{"redact", "-in", in},
		{"redact", "-out", out + "2"},
		{"redact", "-in", in, "-out", out + "3", "extra"},
		{"redact", "-in", in, "-out", out},
	} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Errorf("run %v succeeded", args)
		}
	}
	if err := run(context.Background(), []string{"redact"}, io.Discard); err == nil || !strings.Contains(err.Error(), "pairrun redact -in DIR -out DIR") {
		t.Errorf("the usage does not show redact: %v", err)
	}
}
