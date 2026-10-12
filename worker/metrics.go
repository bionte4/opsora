package main

import (
	"devops-allinone/shared/store"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	durationBuckets = prometheus.ExponentialBuckets(1, 2, 13) // 1s to ~68m

	buildsFinished = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "opsora_builds_finished_total",
		Help: "Builds that reached a final status.",
	}, []string{"status"})

	buildDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "opsora_build_duration_seconds",
		Help:    "Time from claim to final status for one build attempt.",
		Buckets: durationBuckets,
	}, []string{"status"})

	stepDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "opsora_step_duration_seconds",
		Help:    "Pipeline step duration, including image pulls.",
		Buckets: durationBuckets,
	}, []string{"status"})

	buildsRunning = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "opsora_builds_running",
		Help: "Builds this worker is executing.",
	})

	buildsRequeued = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "opsora_builds_requeued_total",
		Help: "Builds returned to the queue, by reason.",
	}, []string{"reason"})
)

// Series must exist at zero before the first event, otherwise rate() and
// increase() miss that event entirely.
func init() {
	for _, s := range []string{store.StatusSuccess, store.StatusFailed} {
		buildsFinished.WithLabelValues(s)
		buildDuration.WithLabelValues(s)
	}
	for _, s := range []string{store.StepSuccess, store.StepFailed, store.StepTimedOut} {
		stepDuration.WithLabelValues(s)
	}
	for _, r := range []string{"worker_lost", "shutdown", "unclaimed"} {
		buildsRequeued.WithLabelValues(r)
	}
}
