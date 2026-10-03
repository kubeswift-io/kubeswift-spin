// Package metrics defines the controller's Prometheus metrics. They are
// registered with the controller-runtime registry and served on the manager's
// metrics endpoint next to the standard controller_runtime_* metrics.
//
// Labels are restricted to small fixed sets. Namespaces, application names,
// UIDs, image references and error strings are never used as label values.
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// App states used by the kubeswift_spin_apps gauge.
const (
	StateAvailable   = "available"
	StateProgressing = "progressing"
	StateBlocked     = "blocked"
	StateUnavailable = "unavailable"
)

var (
	Reconciliations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeswift_spin_reconciliations_total",
		Help: "SpinApp reconciliations by result (success, error).",
	}, []string{"result"})

	ReconcileErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kubeswift_spin_reconcile_errors_total",
		Help: "SpinApp reconciliations that returned an error.",
	})

	Apps = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubeswift_spin_apps",
		Help: "SpinApps realized by kubeswift-spin, by state (available, progressing, blocked, unavailable).",
	}, []string{"state"})

	ReadyReplicas = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kubeswift_spin_ready_replicas",
		Help: "Sum of readyReplicas over all SpinApps realized by kubeswift-spin.",
	})

	DesiredReplicas = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kubeswift_spin_desired_replicas",
		Help: "Sum of desired replicas over all SpinApps realized by kubeswift-spin.",
	})

	SandboxCreations = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kubeswift_spin_sandbox_creations_total",
		Help: "SwiftSandboxes created.",
	})

	SandboxDeletions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeswift_spin_sandbox_deletions_total",
		Help: "SwiftSandboxes deleted, by reason (scale_down, rollout, failed, stale, executor_change).",
	}, []string{"reason"})

	SandboxFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kubeswift_spin_sandbox_failures_total",
		Help: "SwiftSandboxes observed in a terminal phase (Completed or Failed).",
	})

	UnsupportedConfiguration = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeswift_spin_unsupported_configuration_total",
		Help: "Times a SpinApp generation was found to use unsupported configuration, by SpinApp field.",
	}, []string{"field"})
)

func init() {
	crmetrics.Registry.MustRegister(Reconciliations, ReconcileErrors, Apps, ReadyReplicas, DesiredReplicas,
		SandboxCreations, SandboxDeletions, SandboxFailures, UnsupportedConfiguration)
}

type appState struct {
	state   string
	ready   int32
	desired int32
}

// Tracker aggregates per-app state into the gauges without exposing
// per-app labels.
type Tracker struct {
	mu   sync.Mutex
	apps map[string]appState
}

// NewTracker returns an empty Tracker.
func NewTracker() *Tracker { return &Tracker{apps: map[string]appState{}} }

// Set records the state of the app identified by key.
func (t *Tracker) Set(key, state string, ready, desired int32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.apps[key] = appState{state, ready, desired}
	t.publish()
}

// Forget removes an app that no longer exists or is no longer managed.
func (t *Tracker) Forget(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.apps[key]; !ok {
		return
	}
	delete(t.apps, key)
	t.publish()
}

func (t *Tracker) publish() {
	counts := map[string]float64{StateAvailable: 0, StateProgressing: 0, StateBlocked: 0, StateUnavailable: 0}
	var ready, desired int32
	for _, a := range t.apps {
		counts[a.state]++
		ready += a.ready
		desired += a.desired
	}
	for s, c := range counts {
		Apps.WithLabelValues(s).Set(c)
	}
	ReadyReplicas.Set(float64(ready))
	DesiredReplicas.Set(float64(desired))
}
