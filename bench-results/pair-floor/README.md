# Floor between two rented Pods

Round trip of OE1 Pings between two Runpod Secure Cloud `cpu5c` Pods in one
data centre, the engine's OE1 listener on one Pod and `loadgen -oe` on the
other (D043). Commit d15d870ad4bdb7a9b16438ff1019df531bb87994, 2026-11-21,
09:24 to 09:30 UTC.

- Pods: 8 vCPUs and 16 GB each, EU-RO-1, image `golang:1.27.4`.
- Command: `scripts/pair.sh engine` in one Pod and `scripts/pair.sh client`
  in the other. The client ran
  `loadgen -oe ENGINE_ADDR -rate 10000 -warmup 10s -duration 60s` five times
  against the engine's public TCP mapping of port 9000.
- Files: `run-N.txt` and `run-N.json` are loadgen's report of run N.
  `env-client.txt` and `env-engine.txt` keep the lines of each Pod's
  environment report that the floor needs.

## Results

Round trip from a Ping's due time to its Pong, and send lag, in µs. Every
run was valid: 10,000 Pings a second sent, 600,000 of 600,000 answered, send
lag p99.9 at most 10 µs.

| Run | p50 | p99 | p99.9 | p99.99 | max | Send lag p99.9 |
|---|---|---|---|---|---|---|
| 1 | 58.6 | 71.3 | 103.5 | 314.6 | 3,039.3 | 7.0 |
| 2 | 56.5 | 67.0 | 79.1 | 132.5 | 3,248.0 | 6.6 |
| 3 | 56.3 | 65.2 | 76.9 | 867.8 | 2,791.3 | 6.8 |
| 4 | 64.2 | 76.0 | 97.6 | 828.7 | 3,327.2 | 8.2 |
| 5 | 60.2 | 71.4 | 100.8 | 179.4 | 2,694.9 | 7.7 |

The p99 of each run is at most 1.22 times its p50. The longest round trip of
each run is 2.7 to 3.3 ms.

## The two hosts

| | Engine | Client |
|---|---|---|
| Machine ID | `weiltxeslcjv` | `3n7hlaf04ogy` |
| CPU | AMD Ryzen Threadripper 7960X 24-Cores | AMD EPYC 9575F 64-Core |
| Kernel | 6.8.0-138-generic | 6.8.0-147-generic |
| Kernel boot ID | 0c5b271b-d9a8-4129-b2da-12365681f13e | ee42b13b-f1be-4021-96a1-cb05ed8b266d |
| Clock source | tsc | tsc |
| `time.Now` then `time.Since`, 1,000,000 samples | p50 20 ns, p99 21 ns, max 221.1 µs | p50 20 ns, p99 30 ns, max 31.08 µs |
| `setsockopt(SO_BUSY_POLL, 50 µs)` | ok | ok |

Each Pod was allowed 8 CPUs. The two Pods ran on different machines.

## Checks of the route

- A 2-vCPU Pod with a 300 s deadline, to which no delete was sent, read 404
  from the API 473 s after its create.
- The engine Pod's port 9000 had a public TCP mapping 38 s after its create.
- The client's `results.tgz` (13,430 bytes, SHA-256
  `59f6668d2182eee44a51fd101a9ba73f995ddf3d68c3ea26e56af9cbf8435cf4`) was
  fetched through Runpod's HTTP proxy and has the SHA-256 that the client
  logged and that of the base64 copy in its log. The archive is not kept
  here; the run files and `env-client.txt` come from it, and `env-engine.txt`
  from the engine's log.
- Both Pods read back 404 after their deletion.

## Limits

This is one pair at one time of day, so it is one sample of the floor. The
two hosts have different CPUs. The traffic crosses the data centre over the
engine's public address and the host's port mapping, and the floor includes
that path.
