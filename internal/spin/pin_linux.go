package spin

import "golang.org/x/sys/unix"

// allowedCPUs lists the CPUs this process may run on.
func allowedCPUs() ([]int, error) {
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err != nil {
		return nil, err
	}
	var cpus []int
	for cpu := range len(set) * 64 {
		if set.IsSet(cpu) {
			cpus = append(cpus, cpu)
		}
	}
	return cpus, nil
}

// pinThread keeps the calling thread on cpu.
func pinThread(cpu int) error {
	var set unix.CPUSet
	set.Set(cpu)
	return unix.SchedSetaffinity(0, &set)
}
