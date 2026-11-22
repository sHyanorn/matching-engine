# Session resume

Benchmarks behind D045, on the VM and with the method of `../oe-orders/`
(same `run.sh`; ten alternating rounds of 200,000 commands each, benchstat).
`before` is the commit that serves orders and cancels, `after` the commit that
resumes sessions.

`BenchmarkOrders` (a real engine, loopback, 256 commands of the SPEC §9 flow
in a write and their replies read before the next write),
`benchstat-oe-orders.txt`: 4.90 µs per command before, 4.96 µs after (p=0.39),
0 allocations in both. Bytes per command rise from 139 to 158: the buffer of
held output, which grows once to at most 1.5 MB.

`BenchmarkPing`, `benchstat-oe-ping.txt`: 395.6 ns before, 395.8 ns after
(p=0.97), 0 allocations in both.

`BenchmarkResume` (`summary-resume.txt`): a Login that resumes a session with
a full 1 MB of held output, from its oldest frame, takes the server 47.2 µs
(± 9%) before the first byte is written: finding the frame and queueing
LoginAccepted and 1 MB behind it. The time to send the bytes is not in it.
