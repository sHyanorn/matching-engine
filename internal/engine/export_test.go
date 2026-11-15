package engine

import "github.com/sHyanorn/matching-engine/internal/book"

// FailSyncs makes every fsync of the first symbol's log fail. Call it
// before the first request is sent.
func FailSyncs(e *Engine) {
	injectFault(e, &faultyLog{limit: -1, syncErr: errInjected})
}

// LogPayload is the payload of cmd's command log record, with no client_id.
func LogPayload(cmd book.Command) []byte {
	rec := appendRecord(nil, logRecord{cmd: cmd})
	return rec[4 : len(rec)-4]
}
