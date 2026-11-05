package engine

// FailSyncs makes every fsync of the first symbol's log fail. Call it
// before the first request is sent.
func FailSyncs(e *Engine) {
	injectFault(e, &faultyLog{limit: -1, syncErr: errInjected})
}
