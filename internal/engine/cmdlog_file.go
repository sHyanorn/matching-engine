package engine

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// logFile is what a cmdLog needs from its open file once replay is done.
// Tests substitute failing implementations.
type logFile interface {
	io.Writer
	Sync() error
	Close() error
}

// cmdLog is the writing end of one symbol's command log. It appends each
// batch's records and has them fsynced in one of two ways, fixed when the
// log is opened:
//
//   - every batch (Config.SyncInterval < 0): write fsyncs before it
//     returns and reports a failed write or fsync, since the durability
//     promised for the batch is then unknown (D017, D025);
//   - at intervals (SyncInterval > 0): a helper goroutine runs the fsyncs,
//     one at most an interval after a write, and write never waits for
//     them (D031).
//
// The first failed write or fsync stops the log: nothing is written after
// it, since its durability could not be promised (D017). Except for err,
// the methods are for the market's goroutine only.
type cmdLog struct {
	symbol     string
	file       logFile
	everyBatch bool
	interval   time.Duration // between fsyncs, unless everyBatch
	logger     *slog.Logger
	onSync     func(symbol string, took time.Duration)

	dirty    bool                  // written since the last fsync was started or asked for
	lastSync time.Time             // when it was
	failed   error                 // the first failure (wraps ErrLogFailed)
	failure  atomic.Pointer[error] // failed, for other goroutines

	// The helper: wake has it call file.Sync and onSync, so neither may
	// change after start; it reports a failure in helperErr, which the
	// log takes up, and closes stopped when it ends.
	ticker    *time.Ticker
	wake      chan struct{}
	stopped   chan struct{}
	helperErr atomic.Pointer[error]
}

// createLog creates a log holding only its header, unless path exists, and
// reports whether it did, even if it then returns an error. The header
// goes to a temporary file that is synced and then renamed into place, so
// a crash never leaves a log with a partial header.
func createLog(path, symbol string, index int) (bool, error) {
	if _, err := os.Stat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	tmp := path + ".tmp"
	err := writeFileSync(tmp, appendHeader(nil, symbol, index))
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp) // best effort; err is the one to report
		return false, err
	}
	return true, syncDir(filepath.Dir(path))
}

// writeFileSync writes data to a new or truncated file and fsyncs it.
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// openLog opens symbol's log at path, checks its header and hands every
// record to apply, in order. A torn last record is truncated away. The
// log it returns appends after the last good record.
func openLog(path, symbol string, index int, cfg Config, apply func(logRecord) error) (*cmdLog, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if err := replayLog(f, symbol, index, cfg.Logger, apply); err != nil {
		_ = f.Close() // the replay error is the one to report
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cmdLog{
		symbol:     symbol,
		file:       f,
		everyBatch: cfg.SyncInterval < 0,
		interval:   cfg.SyncInterval,
		logger:     cfg.Logger,
		onSync:     cfg.OnSync,
		lastSync:   time.Now(),
	}, nil
}

// replayLog reads f from its header to its end and leaves it positioned
// for appending.
func replayLog(f *os.File, symbol string, index int, logger *slog.Logger, apply func(logRecord) error) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	start, err := readHeader(f, symbol, index)
	if err != nil {
		return err
	}
	good, torn, err := readRecords(f, start, info.Size(), apply)
	if err != nil {
		return err
	}
	if torn {
		if err := f.Truncate(good); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		logger.Warn("dropped a torn tail from the command log", "symbol", symbol, "bytes", info.Size()-good)
	}
	// Writes go on at the end, after the last good record.
	_, err = f.Seek(0, io.SeekEnd)
	return err
}

// start begins the interval fsyncs and returns the channel that says when
// one may be due, on which the owner calls tick. If every batch is
// fsynced it starts nothing and returns nil.
func (l *cmdLog) start() <-chan time.Time {
	if l.everyBatch {
		return nil
	}
	l.ticker = time.NewTicker(l.interval)
	l.wake, l.stopped = make(chan struct{}, 1), make(chan struct{})
	go l.syncLoop(l.wake)
	return l.ticker.C
}

// write appends one batch's records: buf holds them back to back and
// ends[i] is where record i ends. It returns how many of them are wholly
// in the log, which after a failed or short write is not all of them. If
// every batch is fsynced it also returns the failure, of the write or of
// the fsync, that leaves the durability of those records unknown.
func (l *cmdLog) write(buf []byte, ends []int) (logged int, unsynced error) {
	l.takeHelperErr()
	if len(buf) == 0 || l.failed != nil {
		return 0, nil
	}
	n, err := l.file.Write(buf)
	for logged < len(ends) && ends[logged] <= n {
		logged++
	}
	if n > 0 {
		l.dirty = true
	}
	switch {
	case err != nil:
		l.fail("write", err)
	case l.everyBatch || time.Since(l.lastSync) >= l.interval:
		l.sync()
	}
	if l.everyBatch {
		return logged, l.failed
	}
	return logged, nil
}

// tick has pending writes fsynced; the owner calls it every interval.
func (l *cmdLog) tick() {
	l.takeHelperErr()
	if l.dirty {
		l.sync()
	}
}

// sync has the log flushed to stable storage: by the helper if it runs,
// at once otherwise. A failure is kept in failed.
func (l *cmdLog) sync() {
	l.dirty = false
	l.lastSync = time.Now()
	if l.wake != nil {
		select {
		case l.wake <- struct{}{}:
		default: // an fsync is already due to start, and covers these writes
		}
		return
	}
	if err := l.fsync(); err != nil {
		l.fail("fsync", err)
	}
}

// syncLoop is the helper: it runs an fsync for each wake, until wake is
// closed.
func (l *cmdLog) syncLoop(wake <-chan struct{}) {
	defer close(l.stopped)
	for range wake {
		if err := l.fsync(); err != nil {
			l.helperErr.CompareAndSwap(nil, &err)
		}
	}
}

// takeHelperErr takes up a failed fsync of the helper.
func (l *cmdLog) takeHelperErr() {
	if err := l.helperErr.Load(); err != nil {
		l.fail("fsync", *err)
	}
}

// fsync flushes the log and reports how long it took.
func (l *cmdLog) fsync() error {
	start := time.Now()
	err := l.file.Sync()
	if l.onSync != nil {
		l.onSync(l.symbol, time.Since(start))
	}
	return err
}

// fail records the first failed write or fsync.
func (l *cmdLog) fail(op string, err error) {
	if l.failed != nil {
		return
	}
	l.failed = fmt.Errorf("%w: %s: %s: %w", ErrLogFailed, l.symbol, op, err)
	failed := l.failed
	l.failure.Store(&failed)
	l.logger.Error("command log failed; the symbol accepts no more commands", "symbol", l.symbol, "err", err)
}

// err returns the failure that stopped the log, or nil while it works. It
// is safe to call from any goroutine.
func (l *cmdLog) err() error {
	if err := l.failure.Load(); err != nil {
		return *err
	}
	return nil
}

// close stops the helper once its fsyncs are done, flushes what was
// written since and closes the file. It returns the log's failure, if
// any, with that of the close.
func (l *cmdLog) close() error {
	if l.wake != nil {
		l.ticker.Stop()
		close(l.wake)
		<-l.stopped
		l.wake = nil
		l.takeHelperErr()
	}
	if l.dirty {
		l.sync()
	}
	return errors.Join(l.failed, l.file.Close())
}
