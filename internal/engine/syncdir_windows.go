package engine

// syncDir does nothing on Windows, which cannot flush a directory: a
// directory opens read-only, and FlushFileBuffers needs write access.
// Pebble and RocksDB skip it there too.
func syncDir(string) error { return nil }
