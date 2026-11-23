package main

import "golang.org/x/sys/unix"

// pinThread keeps the calling thread on cpu and returns a function that gives
// it back the CPUs it had. A pid of 0 names the calling thread alone, so no
// other thread of the process is touched, and restore must be called on that
// same thread. If pinThread fails, the thread's CPUs are as they were.
func pinThread(cpu int) (restore func() error, err error) {
	var before unix.CPUSet
	if err := unix.SchedGetaffinity(0, &before); err != nil {
		return nil, err
	}
	var set unix.CPUSet
	set.Set(cpu)
	if err := unix.SchedSetaffinity(0, &set); err != nil {
		return nil, err
	}
	return func() error { return unix.SchedSetaffinity(0, &before) }, nil
}
