# Go + gnet (`go-gnet-kaikegm`)

High-performance, single-threaded Go feed API built for **The $12 Server Challenge**, sustaining **175,000 concurrent virtual users at `0.000%` error rate** (`5 ms` `p95`, `150 ms` `p99`, `173.07 MiB` RSS), **177,000 concurrent virtual users** within the `<1.0%` threshold (`0.894%` error rate, `10 ms` `p95`, `175.96 MiB` RSS), and **35,577 req/s** max throughput.

| | |
|---|---|
| **Language & Compiler** | Go 1.27.2 (built with Profile-Guided Optimization `default.pgo`, `-trimpath -ldflags="-s -w"`) |
| **Networking** | [`gnet`](https://github.com/panjf2000/gnet) v2.10.0 single-threaded `epoll` event loop (`LockOSThread(true)`, `TCP_NODELAY`) |
| **HTTP Engine** | Custom zero-allocation HTTP/1.1 keep-alive parser and response serializer on `:80` |
| **SQLite Driver** | In-tree CGO binding to pinned SQLite 3.53.4 amalgamation (`-O3 -march=native -flto -DNDEBUG`, `SQLITE_THREADSAFE=2`, `SQLITE_USE_ALLOCA=1`) |
| **JSON Serialization** | Direct C row-to-JSON serializer (`q_feed`, `q_post`) with vectorized span `memcpy` into Go response buffer in a single CGO call (~40% faster `/feed`) |
| **Authentication** | Zero-allocation HS256 JWT validator (`0 B/op`, `0 allocs/op`, ~500 ns/op, Rule 5 compliant) |
| **Nginx or Direct** | **Direct on `:80`**: handles **177,000+ keep-alive connections** with **~175 MiB RAM**, saving 15–20% Nginx CPU overhead |

All Go module dependencies are pinned in `go.mod` and `go.sum`. The SQLite 3.53.4 amalgamation is pinned and SHA-256 verified in `build.sh`.

## Running it

```bash
sudo bash install.sh   # installs build-essential and Go 1.27.2 toolchain
bash build.sh          # downloads pinned SQLite 3.53.4 amalgamation and compiles bin/server with PGO + LTO
SQLITE_PATH=... JWT_SECRET=... HOST=0.0.0.0 PORT=80 bash start.sh
```

## Key Architectural Optimizations

1. **Single-Threaded `epoll` Event Loop (`gnet`) Locked to OS Thread (`:80` Direct, No Nginx)**
   Standard `net/http` spawns one goroutine per active TCP connection. Under 64,000 to 177,000 persistent keep-alive connections, goroutine stack overhead and scheduler context switches consume hundreds of megabytes and saturate a 1-vCPU core. `gnet` multiplexes all connections on a single non-blocking `epoll` loop locked to the OS thread (`GOMAXPROCS(1)`, `LockOSThread(true)`), bypassing Nginx to save 15–20% of the single vCPU.

2. **Direct C Row-to-JSON Serialization (`q_feed`, `q_post`) with Vectorized Span `memcpy` — Single CGO Crossing**
   Instead of scanning rows across CGO boundaries into Go structs or paying for SQLite's `json_object`/`group_concat` dynamic allocations and CTE materialization (`MATERIALIZE`), `/feed` and `/posts/:id` step index-ordered `CROSS JOIN` queries in C and serialize the exact JSON byte stream directly into the Go HTTP response buffer in a single CGO call (cutting `/feed` CPU time by ~40%). Clean ASCII spans in `body` and `author` are copied in bulk via `memcpy`.

3. **In-Tree Lock-Free SQLite 3.53.4 (`EXCLUSIVE` + 1 GiB `mmap` + WAL Page Cache)**
   Compiled in-tree with `-O3 -march=native -flto -DNDEBUG -fomit-frame-pointer -finline-functions -DSQLITE_THREADSAFE=2 -DSQLITE_DEFAULT_MEMSTATUS=0 -DSQLITE_USE_ALLOCA=1 -DSQLITE_OMIT_PROGRESS_CALLBACK=1 -DSQLITE_OMIT_SHARED_CACHE=1`. Configured with `PRAGMA locking_mode=EXCLUSIVE` (zero `-shm` lock syscalls), `journal_mode=WAL`, `synchronous=NORMAL` (Rule 6), `mmap_size=1073741824` (1 GiB zero-copy memory-mapped reads), `cache_size=-16384` (keeping hot WAL index pages in RAM), and `wal_autocheckpoint=10000`.

4. **Zero-Allocation HS256 JWT Verification (Rule 5 Compliant)**
   Verifies every incoming token signature from scratch (`0 B/op`, `0 allocs/op`, **~500 ns/op**) using stack/static scratch buffers, a pre-initialized single-threaded HMAC-SHA256 state (`jwtMAC`), and a custom zero-allocation JSON property scanner that handles arbitrary key ordering and escaped strings.

5. **Profile-Guided Optimization (`default.pgo`) & Runtime Tuning**
   Built with a CPU profile (`default.pgo`) captured under realistic load so the Go compiler optimizes hot-path inlining and branch layout (`OnTraffic`, `parseRequest`, `verifyJWT`, `dbFeed`), paired with `debug.SetGCPercent(400)` and `debug.SetMemoryLimit(1200 << 20)`.

## Benchmark Verification

### Test Hardware & Environment

All local benchmarks were executed on a single laptop where **both the server and the 177,000-VU load generator ran simultaneously on the same machine** over loopback:

| Component | Specification |
| :--- | :--- |
| **Host Machine** | Lenovo IdeaPad 3 15ITL6 (82MD) |
| **CPU** | 11th Gen Intel Core i5-1135G7 @ 4.20 GHz (**Server strictly pinned to 1 OS thread** via `GOMAXPROCS(1)` + `LockOSThread(true)`) |
| **OS & Kernel** | CachyOS x86_64 — Linux `7.2.9-2-cachyos` |
| **Memory & Storage** | 7.54 GiB RAM (`btrfs` SSD) — **Server RSS peaked at 173–178 MiB** (`SetMemoryLimit(1200 MiB)`) |
| **Load Generator** | [`loadgen_pro.go`](./loadgen_pro.go) — Multi-IP raw-TCP Go load generator binding across 25 loopback IPs (`127.0.0.1`–`127.0.0.25`) |

- **Official Functional Test Suite (`test/run.sh`):** **42/42 checks pass** (100% correctness).
- **Official `k6` User Loop Simulation (`loadgen_pro.go`, 3–8 s / 5–15 s think time, 15% like, 2% post):**

| Concurrent VUs | Total Requests | Throughput | Error Rate | Max RAM (RSS) | Latency `p95` *(<500ms)* | Latency `p99` *(<1000ms)* | Verdict |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **50,000 VUs** | 183,868 | 3,215 req/s | **0.000%** | **65.00 MiB** | **500 µs** | **1 ms** | PASS |
| **100,000 VUs** *(C100k)* | 396,538 | 6,403 req/s | **0.000%** | **84.00 MiB** | **2 ms** | **10 ms** | PASS |
| **130,000 VUs** | 555,976 | 8,194 req/s | **0.000%** | **101.02 MiB** | **5 ms** | **20 ms** | PASS |
| **160,000 VUs** | 739,977 | 10,227 req/s | **0.000%** | **111.86 MiB** | **5 ms** | **99 ms** *(Max)* | PASS |
| **165,000 VUs** | 719,794 | 9,444 req/s | **0.000%** | **108.04 MiB** | **5 ms** | **50 ms** | PASS |
| **170,000 VUs** | 775,733 | 9,929 req/s | **0.000%** | **147.72 MiB** | **10 ms** | **150 ms** | PASS |
| **175,000 VUs** *(Clean Peak)* | **809,984** | **10,233 req/s** | **0.000%** | **173.07 MiB** | **5 ms** | **150 ms** | **PASS (0 Errors, 88% CPU)** |
| **176,000 VUs** | 835,555 | 10,073 req/s | **0.404%** | **178.85 MiB** | **5 ms** | **150 ms** | PASS |
| **177,000 VUs** *(Max Ceiling)* | **849,498** | **10,280 req/s** | **0.894%** | **175.96 MiB** | **10 ms** | **150 ms** | **PASS (<1.0% Error)** |
| **180,000 VUs** | 849,515 | 10,217 req/s | 1.706% | 184.23 MiB | 5 ms | 100 ms | FAIL *(Linux `tcp_max_syn_backlog=512` limit)* |

- **Max Throughput Stress (`-fast=true`, 5–20 ms jitter, full request mix):**

| Concurrent VUs | Avg Throughput | Total Requests | Error Rate | Max RAM (RSS) | Latency `p50` |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **1,000 VUs** | **35,577 req/s** | 536,135 | **0.000%** | 28.29 MiB | 13.7 ms |
| **5,000 VUs** | **33,616 req/s** | 510,846 | **0.000%** | 46.29 MiB | 131.3 ms |
| **25,000 VUs** | **30,609 req/s** | 484,826 | **0.000%** | 78.72 MiB | 760.0 ms |
| **80,000 VUs** | **20,399 req/s** | 375,585 | **0.000%** | 121.89 MiB | 3.52 s |

## Load Testing Guide (`loadgen_pro.go`)

Standard single-IP clients hit the Linux ephemeral port ceiling (~28k–64k sockets) and `net/http` goroutine overhead before saturating the server. The standalone load generator in [`loadgen_pro.go`](./loadgen_pro.go) (tagged with `//go:build ignore` so it does not interfere with `build.sh`) binds across **25 loopback IPs (`127.0.0.1`–`127.0.0.25`)** using raw TCP sockets and reproduces the exact user flow from `bench/load.js`.

### 1. Build the Server and Load Generator

Run from the root of the repository:

```bash
# Build the server binary (bin/server)
bash submissions/go-gnet-kaikegm/build.sh

# Compile the standalone load generator
go build -o /tmp/loadgen_pro ./submissions/go-gnet-kaikegm/loadgen_pro.go
```

### 2. Start the Server on a Clean Copy of `seed/feed.db`

```bash
ulimit -n 500000
rm -f /tmp/bench_feed.db*
cp seed/feed.db /tmp/bench_feed.db

JWT_SECRET=twelve-dollar-challenge HOST=0.0.0.0 PORT=3000 SQLITE_PATH=/tmp/bench_feed.db \
  ./submissions/go-gnet-kaikegm/bin/server &
SERVER_PID=$!
```

### 3. Run the Benchmark

- **Official `k6` User Loop Simulation (e.g., 175,000 Concurrent VUs):**
  ```bash
  /tmp/loadgen_pro -users 175000 -rampup 36s -duration 10s -tokens seed/tokens.json -pid $SERVER_PID
  ```

- **Max Throughput Stress Mode (`-fast=true`, 5–20 ms jitter):**
  ```bash
  /tmp/loadgen_pro -users 1000 -rampup 5s -duration 15s -fast=true -tokens seed/tokens.json -pid $SERVER_PID
  ```

### Available CLI Flags (`loadgen_pro`)

| Flag | Default | Description |
| :--- | :--- | :--- |
| `-users` | `1000` | Number of concurrent virtual users (persistent TCP connections) |
| `-rampup` | `10s` | Gradual ramp-up window to establish all connections smoothly |
| `-duration` | `20s` | Steady-state measurement plateau after ramp-up |
| `-pid` | `0` | Server process PID to sample peak `VmRSS` (MiB) and CPU (%) from `/proc` |
| `-tokens` | `seed/tokens.json` | Path to the 20,000 JWT test tokens generated by `seed/make-seed.sh` |
| `-target` | `127.0.0.1:3000` | Target `host:port` of the running server |
| `-fast` | `false` | `false` = official `k6` think times (3–8 s / 5–15 s); `true` = max RPS stress mode (5–20 ms) |

## License

MIT, under the repo's [license](../../LICENSE).
