package controller

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/engine"
)

// Metrics are the controller's Prometheus metrics. A nil *Metrics records
// nothing, so tests need not register any.
type Metrics struct {
	operations *prometheus.CounterVec
	durations  *prometheus.HistogramVec
	stepTimes  *prometheus.HistogramVec
	stale      prometheus.Counter
}

// NewMetrics registers the controller's metrics with reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		operations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "heimdall_agent_operations_total",
			Help: "Engine operations by type and result.",
		}, []string{"type", "result"}),
		durations: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "heimdall_agent_operation_duration_seconds",
			Help:    "Duration of engine operations (time to ready for Apply).",
			Buckets: []float64{5, 15, 30, 60, 120, 180, 300, 600, 1200, 1800},
		}, []string{"type", "result"}),
		stepTimes: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "heimdall_agent_step_duration_seconds",
			Help:    "Duration of successful engine steps, by step.",
			Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600},
		}, []string{"step"}),
		stale: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "heimdall_agent_stale_results_total",
			Help: "Results of superseded operations that were discarded (generation fencing).",
		}),
	}
	reg.MustRegister(m.operations, m.durations, m.stepTimes, m.stale)
	return m
}

func (m *Metrics) operation(t v1alpha1.OperationType, result v1alpha1.OperationResult, d time.Duration) {
	if m == nil {
		return
	}
	m.operations.WithLabelValues(string(t), string(result)).Inc()
	m.durations.WithLabelValues(string(t), string(result)).Observe(d.Seconds())
}

func (m *Metrics) steps(evs []engine.Event) {
	if m == nil {
		return
	}
	for _, ev := range evs {
		if ev.State == "succeeded" {
			m.stepTimes.WithLabelValues(ev.Stage).Observe(ev.Duration.Seconds())
		}
	}
}

func (m *Metrics) staleResult() {
	if m == nil {
		return
	}
	m.stale.Inc()
}
