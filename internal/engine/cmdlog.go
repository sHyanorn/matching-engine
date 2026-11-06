package engine

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/sHyanorn/matching-engine/internal/book"
)

// The command log is one append-only file per symbol (D016). It holds
// every command the symbol's book applied, in order, with OrderID and
// Timestamp filled in, so replaying it into a fresh book reproduces the
// book and its events exactly.
//
// File layout, all integers little-endian:
//
//	header:  magic "MECMDLOG" | version u16 | symbol index u16 |
//	         symbol length u8 | symbol
//	record:  payload length u32 | payload | CRC-32C of payload u32
//	payload: kind u8 | side u8 | type u8 | tif u8 | order ID u64 |
//	         price i64 | qty i64 | timestamp i64 |
//	         client_id length u8 | client_id
//
// A crash can leave the last record incomplete. Reading stops there and
// reports a torn tail, which the engine truncates; damage anywhere before
// the last record is reported as corruption instead (see readRecords).

const (
	logMagic   = "MECMDLOG"
	logVersion = 1

	payloadFixed = 4 + 4*8 + 1 // enums, four 64-bit fields, client_id length
	payloadMax   = payloadFixed + MaxClientIDLen
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrCorruptLog reports a command log that cannot be replayed safely.
var ErrCorruptLog = errors.New("engine: corrupt command log")

// logRecord is one command as stored in the log.
type logRecord struct {
	cmd      book.Command
	clientID string
}

// appendHeader appends the file header for symbol at index to dst.
func appendHeader(dst []byte, symbol string, index int) []byte {
	dst = append(dst, logMagic...)
	dst = binary.LittleEndian.AppendUint16(dst, logVersion)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(index))
	dst = append(dst, byte(len(symbol)))
	return append(dst, symbol...)
}

// readHeader reads and checks the file header, and returns its length.
func readHeader(r io.Reader, symbol string, index int) (int64, error) {
	var fixed [len(logMagic) + 2 + 2 + 1]byte
	if _, err := io.ReadFull(r, fixed[:]); err != nil {
		return 0, fmt.Errorf("%w: header: %w", ErrCorruptLog, err)
	}
	if string(fixed[:len(logMagic)]) != logMagic {
		return 0, fmt.Errorf("%w: not a command log", ErrCorruptLog)
	}
	rest := fixed[len(logMagic):]
	if v := binary.LittleEndian.Uint16(rest); v != logVersion {
		return 0, fmt.Errorf("engine: command log version %d, this engine reads %d", v, logVersion)
	}
	name := make([]byte, rest[4])
	if _, err := io.ReadFull(r, name); err != nil {
		return 0, fmt.Errorf("%w: header: %w", ErrCorruptLog, err)
	}
	if got := int(binary.LittleEndian.Uint16(rest[2:])); string(name) != symbol || got != index {
		return 0, fmt.Errorf("engine: command log is for symbol %q at index %d, configured %q at index %d", name, got, symbol, index)
	}
	return int64(len(fixed) + len(name)), nil
}

// appendRecord appends the encoding of rec to dst.
func appendRecord(dst []byte, rec logRecord) []byte {
	start := len(dst)
	dst = append(dst, 0, 0, 0, 0) // payload length, filled in below
	c := rec.cmd
	dst = append(dst, byte(c.Kind), byte(c.Side), byte(c.Type), byte(c.TIF))
	dst = binary.LittleEndian.AppendUint64(dst, uint64(c.OrderID))
	dst = binary.LittleEndian.AppendUint64(dst, uint64(c.Price))
	dst = binary.LittleEndian.AppendUint64(dst, uint64(c.Qty))
	dst = binary.LittleEndian.AppendUint64(dst, uint64(c.Timestamp))
	dst = append(dst, byte(len(rec.clientID)))
	dst = append(dst, rec.clientID...)
	payload := dst[start+4:]
	binary.LittleEndian.PutUint32(dst[start:], uint32(len(payload)))
	return binary.LittleEndian.AppendUint32(dst, crc32.Checksum(payload, castagnoli))
}

// decodePayload decodes one record's payload, whose checksum has passed.
func decodePayload(p []byte) (logRecord, error) {
	if len(p) < payloadFixed || int(p[payloadFixed-1]) != len(p)-payloadFixed {
		return logRecord{}, errors.New("payload length does not match its client_id length")
	}
	le := binary.LittleEndian
	return logRecord{
		cmd: book.Command{
			Kind: book.CommandKind(p[0]), Side: book.Side(p[1]), Type: book.OrderType(p[2]), TIF: book.TIF(p[3]),
			OrderID:   book.OrderID(le.Uint64(p[4:])),
			Price:     book.Price(le.Uint64(p[12:])),
			Qty:       book.Qty(le.Uint64(p[20:])),
			Timestamp: int64(le.Uint64(p[28:])),
		},
		clientID: string(p[payloadFixed:]),
	}, nil
}

// readRecords reads the records that follow the header, from offset start
// of a file of size bytes, and calls fn for each in order. It returns the
// offset just past the last good record and whether the file ends in a
// torn record, one a crash cut short: a record that runs past the end of
// the file, the last record failing its checksum, or a tail of zero bytes
// wherever it starts (left by some file systems). Any other damage is
// ErrCorruptLog, because records after it could not be trusted to be the
// ones that were applied.
func readRecords(r io.Reader, start, size int64, fn func(logRecord) error) (good int64, torn bool, err error) {
	br := bufio.NewReader(r)
	off := start
	var lenBuf [4]byte
	payload := make([]byte, payloadMax+4)
	for off < size {
		corrupt := func(why string) (int64, bool, error) {
			return off, false, fmt.Errorf("%w: record at offset %d: %s", ErrCorruptLog, off, why)
		}
		if size-off < 4 {
			return off, true, nil
		}
		if _, err := io.ReadFull(br, lenBuf[:]); err != nil {
			return off, false, err
		}
		n := int64(binary.LittleEndian.Uint32(lenBuf[:]))
		if n < payloadFixed || n > payloadMax {
			if lenBuf == [4]byte{} && zeros(br) {
				return off, true, nil
			}
			return corrupt(fmt.Sprintf("payload length %d", n))
		}
		end := off + 4 + n + 4
		if end > size {
			return off, true, nil
		}
		p := payload[:n+4]
		if _, err := io.ReadFull(br, p); err != nil {
			return off, false, err
		}
		if crc32.Checksum(p[:n], castagnoli) != binary.LittleEndian.Uint32(p[n:]) {
			// The last record, or one where a zero tail begins, was cut
			// short by a crash.
			if end == size || zeros(br) {
				return off, true, nil
			}
			return corrupt("checksum mismatch")
		}
		rec, err := decodePayload(p[:n])
		if err != nil {
			return corrupt(err.Error())
		}
		if err := fn(rec); err != nil {
			return corrupt(err.Error())
		}
		off = end
	}
	return off, false, nil
}

// zeros reports whether everything left in r is zero bytes.
func zeros(r io.Reader) bool {
	var buf [4096]byte
	for {
		n, err := r.Read(buf[:])
		if !bytes.Equal(buf[:n], make([]byte, n)) {
			return false
		}
		if err != nil {
			return err == io.EOF
		}
	}
}
