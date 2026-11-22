//go:build !linux

package spin

import (
	"errors"
	"net"
	"runtime"
	"strings"
	"testing"
)

// Off Linux no pool can be made, and the parts of one made by hand do
// nothing: a pin fails and gives its core back, and no connection polls.
func TestUnsupported(t *testing.T) {
	if _, err := New(nil); !errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), runtime.GOOS) {
		t.Errorf("New = %v, want an unsupported error that names %s", err, runtime.GOOS)
	}
	p := NewPool([]int{0}, nil)
	l := p.Reserve("worker")
	if l.Pin() || p.Free() != 1 {
		t.Errorf("a pin worked, or the core was not given back: %d free", p.Free())
	}
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	if p.Wrap(a) != a {
		t.Error("Wrap wrapped a connection")
	}
}
