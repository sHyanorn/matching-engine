package main

import (
	"encoding/json"
	"os"
)

// writeFileAtomic writes data to name so that a reader sees the old file or
// the whole new one, never part of it: a client that is killed leaves whole
// files, which the Pod can still pack and serve.
func writeFileAtomic(name string, data []byte) error {
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, name); err != nil {
		_ = os.Remove(tmp) // the error above is the one to report
		return err
	}
	return nil
}

// writeJSON writes v as indented JSON, with a final newline, atomically.
func writeJSON(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(name, append(data, '\n'))
}
