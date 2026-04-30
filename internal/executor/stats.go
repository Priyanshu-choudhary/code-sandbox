package executor

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Stats tracks request counters and a sliding-window latency ring used by
// the /metrics endpoint. All operations are safe for concurrent use.
type Stats struct {
	total     atomic.Int64
	rejected  atomic.Int64
	inFlight  atomic.Int64

	mu      sync.Mutex
	ring    []time.Duration // recent durations, ring buffer
	ringIdx int             // next write position
	ringSz  int             // logical length (<= len(ring))
}

const ringCap = 1024

func NewStats() *Stats {
	return &Stats{ring: make([]time.Duration, ringCap)}
}

func (s *Stats) IncInFlight() { s.total.Add(1); s.inFlight.Add(1) }
func (s *Stats) DecInFlight() { s.inFlight.Add(-1) }
func (s *Stats) IncRejected() { s.rejected.Add(1) }

func (s *Stats) Observe(d time.Duration) {
	s.mu.Lock()
	s.ring[s.ringIdx] = d
	s.ringIdx = (s.ringIdx + 1) % ringCap
	if s.ringSz < ringCap {
		s.ringSz++
	}
	s.mu.Unlock()
}

// StatsSnapshot is the JSON shape returned by /metrics.
type StatsSnapshot struct {
	Total           int64 `json:"total"`
	Rejected        int64 `json:"rejected"`
	InFlight        int64 `json:"in_flight"`
	MaxConcurrency  int   `json:"max_concurrency"`
	QueueDepth      int   `json:"queue_depth"`
	Samples         int   `json:"samples"`
	P50MS           int64 `json:"p50_ms"`
	P95MS           int64 `json:"p95_ms"`
	P99MS           int64 `json:"p99_ms"`
	MaxMS           int64 `json:"max_ms"`
}

func (s *Stats) Snapshot(maxConc, queue int) StatsSnapshot {
	s.mu.Lock()
	n := s.ringSz
	cp := make([]time.Duration, n)
	copy(cp, s.ring[:n])
	s.mu.Unlock()

	out := StatsSnapshot{
		Total:          s.total.Load(),
		Rejected:       s.rejected.Load(),
		InFlight:       s.inFlight.Load(),
		MaxConcurrency: maxConc,
		QueueDepth:     queue,
		Samples:        n,
	}
	if n == 0 {
		return out
	}
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	out.P50MS = cp[pIdx(n, 0.50)].Milliseconds()
	out.P95MS = cp[pIdx(n, 0.95)].Milliseconds()
	out.P99MS = cp[pIdx(n, 0.99)].Milliseconds()
	out.MaxMS = cp[n-1].Milliseconds()
	return out
}

func pIdx(n int, p float64) int {
	i := int(float64(n) * p)
	if i >= n {
		i = n - 1
	}
	return i
}
