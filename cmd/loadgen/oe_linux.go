package main

import (
	"errors"
	"io"
	"net"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

// spareCPUs lists the CPUs this process may run on, highest first, or
// none if there are fewer than two to give the pacer and the reader.
func spareCPUs() cpus {
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err != nil {
		return nil
	}
	var c cpus
	for cpu := len(set)*64 - 1; cpu >= 0; cpu-- {
		if set.IsSet(cpu) {
			c = append(c, cpu)
		}
	}
	if len(c) < 2 {
		return nil
	}
	return c
}

// pin keeps the calling thread on cpu, if cpu is one, and reports whether
// it did; the caller has locked its goroutine to the thread. If the system
// refuses, the thread stays where it may run.
func pin(cpu int) bool {
	if cpu < 0 {
		return false
	}
	var set unix.CPUSet
	set.Set(cpu)
	return unix.SchedSetaffinity(0, &set) == nil
}

// spinRead returns a read function that polls c's socket until data, the
// end of the stream or stop, instead of waiting to be woken, and false if
// c has no socket, as a test's pipe has not.
func spinRead(c net.Conn, stop *atomic.Bool) (func([]byte) (int, error), bool) {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return nil, false
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return nil, false
	}
	s := &spinner{rc: rc, stop: stop}
	s.poll = s.pollFD // made once: a method value passed per read would allocate
	return s.read, true
}

type spinner struct {
	rc   syscall.RawConn
	stop *atomic.Bool
	poll func(fd uintptr) bool

	buf []byte
	n   int
	err error
}

func (s *spinner) pollFD(fd uintptr) bool {
	for {
		s.n, s.err = unix.Read(int(fd), s.buf)
		if !errors.Is(s.err, unix.EAGAIN) || s.stop.Load() {
			return true
		}
	}
}

func (s *spinner) read(b []byte) (int, error) {
	s.buf = b
	if err := s.rc.Read(s.poll); err != nil {
		return 0, err
	}
	switch {
	case errors.Is(s.err, unix.EAGAIN):
		return 0, errStopped
	case s.err != nil:
		return 0, s.err
	case s.n == 0:
		return 0, io.EOF
	}
	return s.n, nil
}
