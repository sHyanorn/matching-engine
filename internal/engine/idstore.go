package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
)

// The order-ID counter is reserved in blocks recorded in <DataDir>/ids
// before any ID of a block is issued (D021). A machine crash can drop the
// last unsynced log records, and with them the highest IDs the logs show,
// but never the reservation, so no ID handed out before a crash is issued
// again after it. A restart skips the rest of the current block.

const idBlock = 1 << 16 // counter values reserved at a time

// idStore persists the highest reserved counter value.
type idStore struct {
	path string
}

// load returns the reserved high-water mark, or 0 if none was recorded.
func (s idStore) load() (uint64, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(data) != 12 || crc32.Checksum(data[:8], castagnoli) != binary.LittleEndian.Uint32(data[8:]) {
		return 0, fmt.Errorf("%w: %s", ErrCorruptLog, s.path)
	}
	return binary.LittleEndian.Uint64(data), nil
}

// store durably records high as the reserved high-water mark: written to a
// temporary file, synced, renamed into place, and the directory synced.
func (s idStore) store(high uint64) error {
	data := binary.LittleEndian.AppendUint64(nil, high)
	data = binary.LittleEndian.AppendUint32(data, crc32.Checksum(data, castagnoli))
	tmp := s.path + ".tmp"
	if err := writeFileSync(tmp, data); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(s.path))
}
