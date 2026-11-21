// Package oe is OE1, the binary order entry protocol of docs/API.md (D041):
// its frames and their encoding.
//
// Every frame is length u16 | type u8 | body, little-endian, where length
// counts the type byte and the body and each type has one body size. Each
// frame type is a struct with an Append method that encodes it, and, if
// it has a body, a Decode function that reads one. [Next] splits frames
// off a byte stream. Encoding appends to a caller-owned buffer and
// decoding reads in place, so neither allocates, except the name of a
// [Symbol].
package oe

import (
	"errors"
	"fmt"
)

// Type is a frame's type byte.
type Type byte

// The frame types. Sent by the client: Login, NewOrder, Cancel, Ping,
// Heartbeat and Logout; by the server: LoginAccepted, LoginRejected,
// Symbol, Event, Done, Pong, Heartbeat and EndOfSession.
const (
	TypeLogin         Type = 'L'
	TypeNewOrder      Type = 'N'
	TypeCancel        Type = 'C'
	TypePing          Type = 'P'
	TypeHeartbeat     Type = 'H'
	TypeLogout        Type = 'O'
	TypeLoginAccepted Type = 'A'
	TypeLoginRejected Type = 'J'
	TypeSymbol        Type = 'Y'
	TypeEvent         Type = 'E'
	TypeDone          Type = 'D'
	TypePong          Type = 'Q'
	TypeEndOfSession  Type = 'Z'
)

// HeaderSize is the length and type that precede every body.
const HeaderSize = 3

// bodySize returns the body length of frames of type t, and false for an
// unknown type.
func bodySize(t Type) (int, bool) {
	switch t {
	case TypeLogin:
		return 18, true
	case TypeNewOrder, TypePing:
		return 28, true // a Ping is the size of a NewOrder
	case TypeCancel:
		return 16, true
	case TypeLoginAccepted:
		return 24, true
	case TypeLoginRejected:
		return 1, true
	case TypeSymbol:
		return 1 + SymbolNameSize, true
	case TypeEvent:
		return 16 + 64, true
	case TypeDone, TypePong:
		return 27, true // a Pong is the size of a Done
	case TypeHeartbeat, TypeLogout, TypeEndOfSession:
		return 0, true
	}
	return 0, false
}

// ErrFrame reports a frame of an unknown type, or whose length is not the
// one its type has. A byte stream cannot be resynchronized after it.
var ErrFrame = errors.New("oe: malformed frame")

// Next splits the first frame off b. It returns the frame's type, its body
// and its length on the wire, or n == 0 if b does not hold the whole frame
// yet. The header is checked as soon as it is in, so a malformed frame is
// reported before its body arrives.
func Next(b []byte) (t Type, body []byte, n int, err error) {
	if len(b) < HeaderSize {
		return 0, nil, 0, nil
	}
	length := int(le.Uint16(b))
	t = Type(b[2])
	size, ok := bodySize(t)
	if !ok || length != 1+size {
		return 0, nil, 0, fmt.Errorf("%w: type %q, length %d", ErrFrame, byte(t), length)
	}
	n = HeaderSize + size
	if len(b) < n {
		return 0, nil, 0, nil
	}
	return t, b[HeaderSize:n], n, nil
}

// header appends the length and type of a frame of type t to dst.
func header(dst []byte, t Type) []byte {
	size, _ := bodySize(t)
	dst = le.AppendUint16(dst, uint16(1+size))
	return append(dst, byte(t))
}
