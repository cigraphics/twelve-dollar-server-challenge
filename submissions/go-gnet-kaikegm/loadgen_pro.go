//go:build ignore

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type TokenUser struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

// Latency bucket boundaries in microseconds
var bucketLimits = []int64{
	500,      // 0.5ms
	1000,     // 1ms
	2000,     // 2ms
	5000,     // 5ms
	10000,    // 10ms
	20000,    // 20ms
	50000,    // 50ms
	100000,   // 100ms
	150000,   // 150ms
	200000,   // 200ms
	250000,   // 250ms
	300000,   // 300ms
	350000,   // 350ms
	400000,   // 400ms
	450000,   // 450ms
	500000,   // 500ms (Official p95 threshold)
	600000,   // 600ms
	750000,   // 750ms
	1000000,  // 1000ms (Official p99 threshold)
	1500000,  // 1.5s
	2000000,  // 2s
	3000000,  // 3s
	5000000,  // 5s
	10000000, // 10s
}

const numBuckets = 25 // 24 limits + 1 overflow bucket

type Histogram struct {
	counts [numBuckets]atomic.Uint64
	total  atomic.Uint64
	sumUs  atomic.Uint64
	maxUs  atomic.Int64
}

func (h *Histogram) Record(us int64) {
	h.total.Add(1)
	h.sumUs.Add(uint64(us))

	// Update max atomically
	for {
		cur := h.maxUs.Load()
		if us <= cur {
			break
		}
		if h.maxUs.CompareAndSwap(cur, us) {
			break
		}
	}

	for i, lim := range bucketLimits {
		if us <= lim {
			h.counts[i].Add(1)
			return
		}
	}
	h.counts[numBuckets-1].Add(1)
}

func (h *Histogram) Percentile(p float64) time.Duration {
	tot := h.total.Load()
	if tot == 0 {
		return 0
	}
	target := uint64(float64(tot) * p)
	if target == 0 {
		target = 1
	}
	var acc uint64
	for i := 0; i < len(bucketLimits); i++ {
		acc += h.counts[i].Load()
		if acc >= target {
			return time.Duration(bucketLimits[i]) * time.Microsecond
		}
	}
	return time.Duration(h.maxUs.Load()) * time.Microsecond
}

func (h *Histogram) Avg() time.Duration {
	tot := h.total.Load()
	if tot == 0 {
		return 0
	}
	return time.Duration(h.sumUs.Load()/tot) * time.Microsecond
}

func (h *Histogram) Max() time.Duration {
	return time.Duration(h.maxUs.Load()) * time.Microsecond
}

func main() {
	usersFlag := flag.Int("users", 1000, "Number of concurrent virtual users (VUs)")
	durationFlag := flag.Duration("duration", 20*time.Second, "Steady-state plateau test duration")
	rampUpFlag := flag.Duration("rampup", 10*time.Second, "Ramp-up duration to connect VUs gradually")
	serverPIDFlag := flag.Int("pid", 0, "Server PID to monitor peak RSS memory and CPU usage")
	tokensPathFlag := flag.String("tokens", "seed/tokens.json", "Path to seed/tokens.json")
	fastModeFlag := flag.Bool("fast", false, "High-throughput stress mode (5-20ms jitter); default false (3-8s official k6 think time)")
	hostPortFlag := flag.String("target", "127.0.0.1:3000", "Target server host:port")
	flag.Parse()

	// 1. Load test tokens
	tokensPath := *tokensPathFlag
	if _, err := os.Stat(tokensPath); err != nil {
		alt := filepath.Join("..", "..", tokensPath)
		if _, err2 := os.Stat(alt); err2 == nil {
			tokensPath = alt
		} else {
			tokensPath = filepath.Join("..", tokensPath)
		}
	}
	tokensBytes, err := os.ReadFile(tokensPath)
	if err != nil {
		fmt.Printf("Error loading tokens from %s: %v\n", tokensPath, err)
		os.Exit(1)
	}
	var tokens []TokenUser
	if err := json.Unmarshal(tokensBytes, &tokens); err != nil || len(tokens) == 0 {
		fmt.Printf("Error parsing tokens: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Loaded %d seed tokens.\n", len(tokens))

	// 2. Prepare 25 local loopback IPs (supports up to ~700,000 concurrent sockets)
	numIPs := 25
	localIPs := make([]net.IP, numIPs)
	for i := 0; i < numIPs; i++ {
		localIPs[i] = net.IPv4(127, 0, 0, byte(i+1))
	}

	// Atomic histograms and counters
	var (
		overallHist  Histogram
		feedHist     Histogram
		postHist     Histogram
		likeHist     Histogram
		createHist   Histogram
		totalReqs    atomic.Uint64
		errReqs      atomic.Uint64
		statusCounts [600]atomic.Uint64
	)

	// 3. Server process monitor (RSS memory and CPU usage)
	stopMonitor := make(chan struct{})
	var maxRSSKb int64
	var peakCPU float64
	if *serverPIDFlag > 0 {
		go func(pid int) {
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			var prevUTime, prevSTime uint64
			var prevTime time.Time
			for {
				select {
				case <-stopMonitor:
					return
				case t := <-ticker.C:
					// Sample VmRSS from /proc/<pid>/status
					data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
					if err == nil {
						for _, line := range strings.Split(string(data), "\n") {
							if strings.HasPrefix(line, "VmRSS:") {
								f := strings.Fields(line)
								if len(f) >= 2 {
									rss, _ := strconv.ParseInt(f[1], 10, 64)
									if rss > atomic.LoadInt64(&maxRSSKb) {
										atomic.StoreInt64(&maxRSSKb, rss)
									}
								}
							}
						}
					}
					// Sample CPU ticks from /proc/<pid>/stat
					statData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
					if err == nil {
						f := strings.Fields(string(statData))
						if len(f) > 15 {
							utime, _ := strconv.ParseUint(f[13], 10, 64)
							stime, _ := strconv.ParseUint(f[14], 10, 64)
							if !prevTime.IsZero() {
								deltaSec := t.Sub(prevTime).Seconds()
								deltaTicks := float64((utime + stime) - (prevUTime + prevSTime))
								cpuPct := (deltaTicks / 100.0) / deltaSec * 100.0
								if cpuPct > peakCPU {
									peakCPU = cpuPct
								}
							}
							prevUTime, prevSTime = utime, stime
							prevTime = t
						}
					}
				}
			}
		}(*serverPIDFlag)
	}

	targetAddr := *hostPortFlag
	numVUs := *usersFlag
	rampUp := *rampUpFlag
	steadyDuration := *durationFlag

	fmt.Printf("\nStarting benchmark with %d concurrent VUs...\n", numVUs)
	fmt.Printf("Connection ramp-up: %v (~%.0f VUs/s)\n", rampUp, float64(numVUs)/rampUp.Seconds())
	fmt.Printf("Steady-state plateau: %v\n", steadyDuration)
	if *fastModeFlag {
		fmt.Println("Mode: High-Throughput Stress (5-20ms jitter)")
	} else {
		fmt.Println("Mode: Official k6 User Loop Simulation (3-8s / 5-15s think time)")
	}

	startTestTime := time.Now()
	testDeadline := startTestTime.Add(rampUp + steadyDuration)

	var wg sync.WaitGroup
	var activeSockets atomic.Int64

	batchSize := numVUs / int(rampUp/(10*time.Millisecond))
	if batchSize < 1 {
		batchSize = 1
	}
	if batchSize > 250 {
		batchSize = 250
	}
	batchDelay := rampUp / time.Duration(numVUs/batchSize+1)
	if batchDelay < 5*time.Millisecond {
		batchDelay = 5 * time.Millisecond
	}

	for i := 0; i < numVUs; i++ {
		vuID := i + 1
		localIP := localIPs[i%numIPs]
		user := tokens[i%len(tokens)]

		wg.Add(1)
		go func(id int, lip net.IP, u TokenUser) {
			defer wg.Done()

			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)))

			dialer := &net.Dialer{
				LocalAddr: &net.TCPAddr{IP: lip},
				Timeout:   10 * time.Second,
				KeepAlive: 65 * time.Second,
			}

			var conn net.Conn
			var reader *bufio.Reader
			var writer *bufio.Writer

			connect := func() bool {
				if conn != nil {
					conn.Close()
					activeSockets.Add(-1)
					conn = nil
				}
				c, err := dialer.Dial("tcp", targetAddr)
				if err != nil {
					errReqs.Add(1)
					statusCounts[0].Add(1)
					return false
				}
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.SetNoDelay(true)
					_ = tc.SetKeepAlive(true)
					_ = tc.SetKeepAlivePeriod(65 * time.Second)
				}
				conn = c
				reader = bufio.NewReaderSize(c, 16384)
				writer = bufio.NewWriterSize(c, 4096)
				activeSockets.Add(1)
				return true
			}

			defer func() {
				if conn != nil {
					conn.Close()
					activeSockets.Add(-1)
				}
			}()

			// Execute raw HTTP/1.1 request over the persistent TCP connection
			doReq := func(method, path string, headers map[string]string, body []byte) (int, []byte, time.Duration) {
				for attempt := 0; attempt < 2; attempt++ {
					if conn == nil {
						if !connect() {
							return 0, nil, 0
						}
					}

					t0 := time.Now()
					_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

					// Format HTTP/1.1 request
					writer.WriteString(method)
					writer.WriteByte(' ')
					writer.WriteString(path)
					writer.WriteString(" HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: keep-alive\r\n")
					for k, v := range headers {
						writer.WriteString(k)
						writer.WriteString(": ")
						writer.WriteString(v)
						writer.WriteString("\r\n")
					}
					if len(body) > 0 {
						writer.WriteString("Content-Length: ")
						writer.WriteString(strconv.Itoa(len(body)))
						writer.WriteString("\r\n\r\n")
						writer.Write(body)
					} else {
						writer.WriteString("\r\n")
					}
					if err := writer.Flush(); err != nil {
						conn.Close()
						conn = nil
						activeSockets.Add(-1)
						continue
					}

					// Read status line
					statusLine, err := reader.ReadString('\n')
					if err != nil {
						conn.Close()
						conn = nil
						activeSockets.Add(-1)
						continue
					}

					// Parse status: HTTP/1.1 200 OK
					parts := strings.SplitN(statusLine, " ", 3)
					if len(parts) < 2 {
						conn.Close()
						conn = nil
						activeSockets.Add(-1)
						continue
					}
					statusCode, _ := strconv.Atoi(parts[1])

					// Read response headers
					contentLen := 0
					isClose := false
					for {
						hLine, err := reader.ReadString('\n')
						if err != nil || hLine == "\r\n" || hLine == "\n" {
							break
						}
						colon := strings.IndexByte(hLine, ':')
						if colon > 0 {
							k := strings.ToLower(strings.TrimSpace(hLine[:colon]))
							v := strings.TrimSpace(hLine[colon+1:])
							if k == "content-length" {
								contentLen, _ = strconv.Atoi(v)
							} else if k == "connection" && strings.EqualFold(v, "close") {
								isClose = true
							}
						}
					}

					// Read response body
					var respBody []byte
					if contentLen > 0 {
						respBody = make([]byte, contentLen)
						_, _ = io.ReadFull(reader, respBody)
					}

					lat := time.Since(t0)
					if isClose {
						conn.Close()
						conn = nil
						activeSockets.Add(-1)
					}

					return statusCode, respBody, lat
				}
				return 0, nil, 0
			}

			// Initial stagger
			if !*fastModeFlag {
				time.Sleep(time.Duration(rng.Intn(3000)) * time.Millisecond)
			}

			authHdr := map[string]string{
				"Authorization": "Bearer " + u.Token,
			}

			for time.Now().Before(testDeadline) {
				// 1. GET /feed
				code, body, lat := doReq("GET", "/feed", nil, nil)
				totalReqs.Add(1)
				if code >= 0 && code < 600 {
					statusCounts[code].Add(1)
				}
				if code == 200 {
					us := lat.Microseconds()
					overallHist.Record(us)
					feedHist.Record(us)
				} else {
					errReqs.Add(1)
					time.Sleep(50 * time.Millisecond)
					continue
				}

				// Pick a post ID from the feed
				postID := int64(1)
				var feed struct {
					Posts []struct {
						ID int64 `json:"id"`
					} `json:"posts"`
				}
				if json.Unmarshal(body, &feed) == nil && len(feed.Posts) > 0 {
					postID = feed.Posts[rng.Intn(len(feed.Posts))].ID
				}

				// Think time
				if *fastModeFlag {
					time.Sleep(time.Duration(5+rng.Intn(15)) * time.Millisecond)
				} else {
					time.Sleep(time.Duration(3000+rng.Intn(4000)) * time.Millisecond)
				}

				// 2. GET /posts/:id
				code, _, lat = doReq("GET", fmt.Sprintf("/posts/%d", postID), nil, nil)
				totalReqs.Add(1)
				if code >= 0 && code < 600 {
					statusCounts[code].Add(1)
				}
				if code == 200 {
					us := lat.Microseconds()
					overallHist.Record(us)
					postHist.Record(us)
				} else {
					errReqs.Add(1)
				}

				// 3. 15% chance: POST /posts/:id/like
				if rng.Float64() < 0.15 {
					if *fastModeFlag {
						time.Sleep(time.Duration(3+rng.Intn(10)) * time.Millisecond)
					} else {
						time.Sleep(time.Duration(3000+rng.Intn(5000)) * time.Millisecond)
					}

					code, _, lat = doReq("POST", fmt.Sprintf("/posts/%d/like", postID), authHdr, nil)
					totalReqs.Add(1)
					if code >= 0 && code < 600 {
						statusCounts[code].Add(1)
					}
					if code == 200 || code == 201 {
						us := lat.Microseconds()
						overallHist.Record(us)
						likeHist.Record(us)
					} else {
						errReqs.Add(1)
					}
				}

				// 4. 2% chance: POST /posts
				if rng.Float64() < 0.02 {
					if *fastModeFlag {
						time.Sleep(time.Duration(3+rng.Intn(10)) * time.Millisecond)
					} else {
						time.Sleep(time.Duration(3000+rng.Intn(5000)) * time.Millisecond)
					}

					postPayload := []byte(fmt.Sprintf(`{"body":"%s says hi at %s (VU %d)"}`, u.Username, time.Now().Format(time.RFC3339Nano), id))
					headers := map[string]string{
						"Authorization": "Bearer " + u.Token,
						"Content-Type":  "application/json",
					}
					code, _, lat = doReq("POST", "/posts", headers, postPayload)
					totalReqs.Add(1)
					if code >= 0 && code < 600 {
						statusCounts[code].Add(1)
					}
					if code == 201 {
						us := lat.Microseconds()
						overallHist.Record(us)
						createHist.Record(us)
					} else {
						errReqs.Add(1)
					}
				}

				// End-of-loop think time
				if *fastModeFlag {
					time.Sleep(time.Duration(5+rng.Intn(20)) * time.Millisecond)
				} else {
					time.Sleep(time.Duration(5000+rng.Intn(10000)) * time.Millisecond)
				}
			}
		}(vuID, localIP, user)

		if (i+1)%batchSize == 0 {
			time.Sleep(batchDelay)
		}
	}

	wg.Wait()
	totalDuration := time.Since(startTestTime)
	close(stopMonitor)

	// Print benchmark report
	tot := totalReqs.Load()
	errs := errReqs.Load()
	errPct := 0.0
	if tot > 0 {
		errPct = float64(errs) / float64(tot) * 100.0
	}
	p95 := overallHist.Percentile(0.95)
	p99 := overallHist.Percentile(0.99)
	passed := p95 < 500*time.Millisecond && p99 < 1000*time.Millisecond && errPct < 1.0

	fmt.Println("\n========================================================")
	fmt.Println("BENCHMARK RESULTS SUMMARY")
	fmt.Println("========================================================")
	fmt.Printf("Target Concurrent VUs:  %d\n", numVUs)
	fmt.Printf("Total Elapsed Time:     %.2f s\n", totalDuration.Seconds())
	fmt.Printf("Total Requests:         %d\n", tot)
	fmt.Printf("Average Throughput:     %.2f req/s\n", float64(tot)/totalDuration.Seconds())
	fmt.Printf("Failed Requests:        %d (%.3f%%)\n", errs, errPct)

	if *serverPIDFlag > 0 {
		fmt.Printf("Peak Server Memory RSS: %.2f MiB\n", float64(atomic.LoadInt64(&maxRSSKb))/1024.0)
		fmt.Printf("Peak Server CPU Usage:  %.1f%%\n", peakCPU)
	}

	fmt.Println("\n--- OVERALL LATENCY DISTRIBUTION ---")
	fmt.Printf("Average: %v\n", overallHist.Avg())
	fmt.Printf("p50:     %v\n", overallHist.Percentile(0.50))
	fmt.Printf("p90:     %v\n", overallHist.Percentile(0.90))
	fmt.Printf("p95:     %v\n", p95)
	fmt.Printf("p99:     %v\n", p99)
	fmt.Printf("Max:     %v\n", overallHist.Max())

	fmt.Println("\n--- LATENCY BY ENDPOINT ---")
	printEp := func(name string, h *Histogram) {
		fmt.Printf("[%s] Count: %d | Avg: %v | p95: %v | p99: %v\n",
			name, h.total.Load(), h.Avg(), h.Percentile(0.95), h.Percentile(0.99))
	}
	printEp("GET /feed", &feedHist)
	printEp("GET /posts/:id", &postHist)
	printEp("POST /like", &likeHist)
	printEp("POST /posts", &createHist)

	fmt.Println("\n--- HTTP STATUS CODES ---")
	for code := 0; code < 600; code++ {
		c := statusCounts[code].Load()
		if c > 0 {
			fmt.Printf("HTTP %d: %d\n", code, c)
		}
	}

	fmt.Println("\n--- OFFICIAL CHALLENGE THRESHOLDS ---")
	fmt.Printf("Criterion 1: p95 < 500 ms    -> %v [%s]\n", p95, statusTag(p95 < 500*time.Millisecond))
	fmt.Printf("Criterion 2: p99 < 1000 ms   -> %v [%s]\n", p99, statusTag(p99 < 1000*time.Millisecond))
	fmt.Printf("Criterion 3: Errors < 1.0%%   -> %.3f%% [%s]\n", errPct, statusTag(errPct < 1.0))

	if passed {
		fmt.Println("\nVERDICT: PASS! All official challenge criteria met.")
	} else {
		fmt.Println("\nVERDICT: FAIL! One or more official thresholds exceeded.")
	}
	fmt.Println("========================================================")
}

func statusTag(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}