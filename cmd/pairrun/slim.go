package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// logNote is the text of log-copy.txt, the one file that a slim copy adds.
const logNote = "This copy leaves out the percentile curves of the loadgen reports and loadgen's text tables.\n" +
	"results.tgz, served on the Pod's port 8000, has them.\n"

func runSlim(args []string) error {
	fs := flag.NewFlagSet("slim", flag.ContinueOnError)
	in := fs.String("in", "", "results directory")
	out := fs.String("out", "", "directory to create for the copy; it must not exist")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("pairrun slim: unexpected argument %q, want flags only", fs.Arg(0))
	}
	if *in == "" || *out == "" {
		return fmt.Errorf("pairrun slim: -in and -out are required\n%s", usage)
	}
	return slim(*in, *out)
}

// slim copies the results directory in to out, which must not exist, in a form
// small enough for a Pod's log, of which the platform returns only the last
// 5000 lines. It leaves out what the full archive, which the Pod serves, still
// has and the log can do without: the percentile curves, the bulk of a loadgen
// report, and loadgen's text tables, which are derived from the reports. A
// loadgen report is any .json file that is an object with a latency_us object,
// whatever its name, as the floor runs and the pilot name theirs differently.
// Everything else is copied byte for byte, and log-copy.txt says what is
// missing.
func slim(in, out string) error {
	if fi, err := os.Stat(in); err != nil {
		return err
	} else if !fi.IsDir() {
		return fmt.Errorf("%s: not a directory", in)
	}
	if err := os.Mkdir(out, 0o755); err != nil {
		return err
	}
	err := filepath.WalkDir(in, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(in, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(out, rel)
		switch {
		case d.IsDir():
			if rel == "." {
				return nil
			}
			return os.Mkdir(dst, 0o755)
		case !d.Type().IsRegular():
			return fmt.Errorf("%s: not a regular file", path)
		}
		data, keep, err := slimFile(path)
		if err != nil || !keep {
			return err
		}
		return writeNew(dst, data)
	})
	if err != nil {
		return err
	}
	return writeNew(filepath.Join(out, "log-copy.txt"), []byte(logNote))
}

// slimFile returns what the copy holds of the file at path: the file's bytes,
// or a loadgen report without its curves, and false if the copy leaves the
// file out.
func slimFile(path string) (data []byte, keep bool, err error) {
	ext := filepath.Ext(path)
	if ext == ".txt" {
		table, err := hasReport(strings.TrimSuffix(path, ".txt") + ".json")
		if err != nil || table {
			return nil, false, err
		}
	}
	data, err = os.ReadFile(path)
	if err != nil || ext != ".json" {
		return data, err == nil, err
	}
	top, rows, err := parseReport(path, data)
	if err != nil {
		return nil, false, err
	}
	if rows == nil {
		return data, true, nil
	}
	if data, err = slimReport(top, rows); err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// hasReport tells whether name is a loadgen report. A file that does not exist
// is not.
func hasReport(name string) (bool, error) {
	data, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, rows, err := parseReport(name, data)
	return rows != nil, err
}

// parseReport parses data, the content of the file at path. If it is a
// loadgen report it returns its fields and the rows of its latency_us, both as
// the file has them: numbers and everything else stay the text they are, as a
// decoding into float64 would not keep them. Valid JSON of another shape is
// not a report and gives nil rows. Data that is not JSON, or a report with a
// row that is not an object, is an error that names the file.
func parseReport(path string, data []byte) (top map[string]json.RawMessage, rows map[string]map[string]json.RawMessage, err error) {
	wrap := func(err error) error { return fmt.Errorf("%s: %w", path, err) }
	switch err := json.Unmarshal(data, &top); {
	case err == nil:
	case errors.As(err, new(*json.UnmarshalTypeError)):
		return nil, nil, nil // valid JSON, but not an object
	default:
		return nil, nil, wrap(err)
	}
	latency, ok := top["latency_us"]
	if !ok {
		return nil, nil, nil
	}
	var rawRows map[string]json.RawMessage
	if err := json.Unmarshal(latency, &rawRows); err != nil || rawRows == nil {
		return nil, nil, nil // not an object
	}
	rows = make(map[string]map[string]json.RawMessage, len(rawRows))
	for name, raw := range rawRows {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return nil, nil, wrap(fmt.Errorf("latency_us row %q is not an object", name))
		}
		rows[name] = fields
	}
	return top, rows, nil
}

// slimReport returns the report as compact JSON, with the curve of every
// latency row removed and every other value as it was written.
func slimReport(top map[string]json.RawMessage, rows map[string]map[string]json.RawMessage) ([]byte, error) {
	for _, fields := range rows {
		delete(fields, "curve")
	}
	var err error
	if top["latency_us"], err = compactJSON(rows); err != nil {
		return nil, err
	}
	data, err := compactJSON(top)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// compactJSON encodes v without whitespace and without the HTML escaping that
// json.Marshal adds, which would change the text of a raw value.
func compactJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// writeNew writes data to a file that must not exist yet, so that nothing in
// the copy overwrites another file.
func writeNew(name string, data []byte) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}
