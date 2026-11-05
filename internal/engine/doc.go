// Package engine runs one order book per symbol behind a concurrent API.
//
// Each symbol has a goroutine that owns its book and handles requests from
// a bounded queue one at a time, so the matching core needs no locks and
// its results are deterministic (D003, SPEC §4). For every command it:
//
//  1. stamps the command with the time;
//  2. applies it to the book;
//  3. hands the events back to the caller.
//
// Order IDs come from one engine-wide counter, assigned before the command
// is validated (D007), and carry their symbol's index so a cancel or an
// order lookup is routed from the ID alone (see ids.go).
package engine
