# Counting OE1 commands

Benchmarks behind D047, on the VM and with the method of `../oe-orders/`
(same `run.sh`; twenty alternating rounds, benchstat). `before` is a610825,
which does not count OE1 commands; `after` counts each one in the counters of
an `api.Server`, wired as `cmd/engine` wires them. Each binary is built
from its commit with `go test -c -o oe.VARIANT.test ./internal/oe` and run
with `PREFIX=oe.` and `BENCHTIME=200000x`, or `2000000x` for the Pings.

`BenchmarkOrders` (a real engine, loopback, 256 commands of the SPEC §9 flow
in a write and their replies read before the next write; 200,000 commands a
round), `benchstat-oe-orders.txt`: 5.43 µs per command before, 5.41 µs after
(p=0.97), 158 bytes and 0 allocations per command in both.

`BenchmarkPing` (2,000,000 Pings a round), `benchstat-oe-ping.txt`: 408.0 ns
before, 401.4 ns after (p=0.43), 0 allocations in both. A Ping is not
counted, so this is the control.

`BenchmarkCountOrder` in `internal/api` (`raw/count.txt`: the ten results of
one `go test` run with `-count=10`; `summary-count.txt`: their summary):
counting one order with two trades takes 25.5 ns (± 11%) and no allocation,
about 0.5% of a command, which the ± 3% of `BenchmarkOrders` cannot show.
