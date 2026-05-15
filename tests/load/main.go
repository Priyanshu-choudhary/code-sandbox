// Command load is a constant-concurrency load generator for goboxd.
//
// Usage:
//   go run ./tests/load -url http://localhost:8080/run \
//       -concurrency 50 -duration 30s -payload tests/load/payloads/python.json
//
// At the end it prints requests, throughput, error breakdown, and
// p50/p95/p99/max latency.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	var (
		url         = flag.String("url", "http://localhost:8080/run", "POST target URL")
		concurrency = flag.Int("concurrency", 10, "concurrent workers")
		duration    = flag.Duration("duration", 30*time.Second, "test duration")
		payload     = flag.String("payload", "", "request body file (required)")
		warmup      = flag.Duration("warmup", 2*time.Second, "warmup time excluded from stats")
	)
	flag.Parse()
	if *payload == "" {
		fmt.Fprintln(os.Stderr, "load: -payload required")
		os.Exit(2)
	}
	body, err := os.ReadFile(*payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load: read payload: %v\n", err)
		os.Exit(2)
	}

	type sample struct {
		dur    time.Duration
		status int
	}
	var (
		samples = make([]sample, 0, 65536)
		mu      sync.Mutex

		total       atomic.Int64
		http200     atomic.Int64
		http429     atomic.Int64
		http4xx     atomic.Int64
		http5xx     atomic.Int64
		netErr      atomic.Int64
	)

	client := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        *concurrency * 2,
			MaxIdleConnsPerHost: *concurrency * 2,
			MaxConnsPerHost:     *concurrency * 2,
			IdleConnTimeout:     30 * time.Second,
		},
	}

	startWall := time.Now()
	statsCutoff := startWall.Add(*warmup)
	deadline := startWall.Add(*warmup + *duration)

	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if time.Now().After(deadline) {
					return
				}
				req, _ := http.NewRequest("POST", *url, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				t0 := time.Now()
				resp, err := client.Do(req)
				d := time.Since(t0)
				total.Add(1)
				if err != nil {
					netErr.Add(1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()

				switch {
				case resp.StatusCode == 200:
					http200.Add(1)
				case resp.StatusCode == 429:
					http429.Add(1)
				case resp.StatusCode >= 500:
					http5xx.Add(1)
				default:
					http4xx.Add(1)
				}

				if t0.After(statsCutoff) {
					mu.Lock()
					samples = append(samples, sample{d, resp.StatusCode})
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	elapsed := time.Since(statsCutoff)
	if elapsed <= 0 {
		elapsed = time.Since(startWall)
	}

	durs := make([]time.Duration, len(samples))
	for i, s := range samples {
		durs[i] = s.dur
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })

	pct := func(p float64) time.Duration {
		if len(durs) == 0 {
			return 0
		}
		i := int(float64(len(durs)) * p)
		if i >= len(durs) {
			i = len(durs) - 1
		}
		return durs[i]
	}

	fmt.Println("=== load summary ===")
	fmt.Printf("url           : %s\n", *url)
	fmt.Printf("concurrency   : %d\n", *concurrency)
	fmt.Printf("duration      : %s (warmup %s excluded)\n", duration.String(), warmup.String())
	fmt.Printf("total reqs    : %d\n", total.Load())
	fmt.Printf("  200 ok      : %d\n", http200.Load())
	fmt.Printf("  429 over    : %d\n", http429.Load())
	fmt.Printf("  4xx other   : %d\n", http4xx.Load())
	fmt.Printf("  5xx error   : %d\n", http5xx.Load())
	fmt.Printf("  net error   : %d\n", netErr.Load())
	fmt.Printf("throughput    : %.1f req/s (window=%s, samples=%d)\n",
		float64(len(samples))/elapsed.Seconds(), elapsed.Truncate(time.Millisecond), len(samples))
	fmt.Printf("latency p50   : %s\n", pct(0.50))
	fmt.Printf("latency p95   : %s\n", pct(0.95))
	fmt.Printf("latency p99   : %s\n", pct(0.99))
	if len(durs) > 0 {
		fmt.Printf("latency max   : %s\n", durs[len(durs)-1])
	}
}
