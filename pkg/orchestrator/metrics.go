package orchestrator

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// PipelineMetrics tracks Prometheus counters and histograms for the RAG orchestrator pipeline.
type PipelineMetrics struct {
	mu           sync.Mutex
	durations    map[string]*metricBucketTracker
	contextBytes *metricBucketTracker
	successTotal atomic.Uint64
	errorTotal   atomic.Uint64
}

type metricBucketTracker struct {
	count atomic.Uint64
	sum   atomic.Uint64 // stored in micro-units (micro-seconds or bytes)
}

func (t *metricBucketTracker) observe(val float64, scale float64) {
	t.count.Add(1)
	scaled := uint64(val * scale)
	t.sum.Add(scaled)
}

func (t *metricBucketTracker) snapshot(scale float64) (uint64, float64) {
	cnt := t.count.Load()
	rawSum := t.sum.Load()
	return cnt, float64(rawSum) / scale
}

// NewPipelineMetrics initializes an empty PipelineMetrics collector.
func NewPipelineMetrics() *PipelineMetrics {
	return &PipelineMetrics{
		durations:    make(map[string]*metricBucketTracker),
		contextBytes: &metricBucketTracker{},
	}
}

// ObservePipelineDuration records an execution duration partitioned by mode and dialect.
func (m *PipelineMetrics) ObservePipelineDuration(mode ExecutionMode, dialect string, durationSeconds float64, success bool) {
	if m == nil {
		return
	}
	if success {
		m.successTotal.Add(1)
	} else {
		m.errorTotal.Add(1)
	}

	key := string(mode) + "|" + dialect
	m.mu.Lock()
	tracker, exists := m.durations[key]
	if !exists {
		tracker = &metricBucketTracker{}
		m.durations[key] = tracker
	}
	m.mu.Unlock()

	tracker.observe(durationSeconds, 1e6) // scale to micro-seconds
}

// ObserveContextBytes records the assembled context size in bytes.
func (m *PipelineMetrics) ObserveContextBytes(bytesCount int) {
	if m == nil {
		return
	}
	m.contextBytes.observe(float64(bytesCount), 1.0)
}

// MetricsExposition formats Prometheus text exposition for the orchestrator pipeline metrics.
func (m *PipelineMetrics) MetricsExposition() string {
	if m == nil {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("# HELP rag_orchestrator_requests_total Total RAG pipeline requests executed\n")
	sb.WriteString("# TYPE rag_orchestrator_requests_total counter\n")
	sb.WriteString(fmt.Sprintf("rag_orchestrator_requests_total{status=\"success\"} %d\n", m.successTotal.Load()))
	sb.WriteString(fmt.Sprintf("rag_orchestrator_requests_total{status=\"error\"} %d\n", m.errorTotal.Load()))

	sb.WriteString("# HELP rag_orchestrator_context_bytes Assembled document chunk context payload size in bytes\n")
	sb.WriteString("# TYPE rag_orchestrator_context_bytes histogram\n")
	ctxCnt, ctxSum := m.contextBytes.snapshot(1.0)
	sb.WriteString(fmt.Sprintf("rag_orchestrator_context_bytes_count %d\n", ctxCnt))
	sb.WriteString(fmt.Sprintf("rag_orchestrator_context_bytes_sum %.0f\n", ctxSum))

	sb.WriteString("# HELP rag_orchestrator_pipeline_duration_seconds End-to-end RAG orchestrator pipeline execution latency\n")
	sb.WriteString("# TYPE rag_orchestrator_pipeline_duration_seconds histogram\n")

	m.mu.Lock()
	keys := make([]string, 0, len(m.durations))
	for k := range m.durations {
		keys = append(keys, k)
	}
	m.mu.Unlock()

	for _, k := range keys {
		parts := strings.SplitN(k, "|", 2)
		mode := parts[0]
		dialect := parts[1]
		m.mu.Lock()
		tracker := m.durations[k]
		m.mu.Unlock()
		if tracker == nil {
			continue
		}
		cnt, sum := tracker.snapshot(1e6)
		sb.WriteString(fmt.Sprintf("rag_orchestrator_pipeline_duration_seconds_count{mode=%q,dialect=%q} %d\n", mode, dialect, cnt))
		sb.WriteString(fmt.Sprintf("rag_orchestrator_pipeline_duration_seconds_sum{mode=%q,dialect=%q} %.6f\n", mode, dialect, sum))
	}

	return sb.String()
}
