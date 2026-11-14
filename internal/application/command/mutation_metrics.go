package command

import (
	"sync"
	"sync/atomic"
)

// MutationMetricsRecorder tracks control plane write latencies and rollback counts for Prometheus exposition.
type MutationMetricsRecorder struct {
	mu            sync.RWMutex
	counts        map[string]uint64
	durationSums  map[string]float64
	rollbackCount atomic.Uint64
}

// NewMutationMetricsRecorder creates a new MutationMetricsRecorder.
func NewMutationMetricsRecorder() *MutationMetricsRecorder {
	return &MutationMetricsRecorder{
		counts:       make(map[string]uint64),
		durationSums: make(map[string]float64),
	}
}

// Observe records a control plane write operation latency in seconds.
func (m *MutationMetricsRecorder) Observe(operation, status string, seconds float64) {
	if m == nil {
		return
	}
	key := operation + "|" + status
	m.mu.Lock()
	m.counts[key]++
	m.durationSums[key] += seconds
	m.mu.Unlock()
}

// RecordRollback increments the counter of semantic tag pointer rollbacks (ElevatedPointerRollbacks alert).
func (m *MutationMetricsRecorder) RecordRollback() {
	if m == nil {
		return
	}
	m.rollbackCount.Add(1)
}

// RollbackCount returns the total number of semantic tag pointer rollbacks recorded.
func (m *MutationMetricsRecorder) RollbackCount() uint64 {
	if m == nil {
		return 0
	}
	return m.rollbackCount.Load()
}

// Snapshot returns copies of the recorded counts and duration sums.
func (m *MutationMetricsRecorder) Snapshot() (map[string]uint64, map[string]float64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c := make(map[string]uint64, len(m.counts))
	s := make(map[string]float64, len(m.durationSums))
	for k, v := range m.counts {
		c[k] = v
	}
	for k, v := range m.durationSums {
		s[k] = v
	}
	return c, s
}
