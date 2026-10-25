package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Local application metrics (ADR 0015). Labels are fixed words; reasons
// outside the known set are counted as "Other". No namespace, name, digest
// or registry is ever a label value.
var (
	LocalApplicationLookups = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeswift_spin_local_application_lookups_total",
		Help: "Replicas created for SpinApps that asked for a local application, by result (attached: the application artifact was attached; unplaced: a warm replica whose pool's placement is not prepared; cold: a replica without a warm pool; held: a failure hold is active; ineligible: the SpinApp cannot have one).",
	}, []string{"result"})
	LocalApplicationAttachments = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeswift_spin_local_application_attachments_total",
		Help: "Replicas that carried the application artifact, by what KubeSwift did with it (mounted, skipped) and reason.",
	}, []string{"result", "reason"})
	LocalApplicationPlacements = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubeswift_spin_local_application_placements",
		Help: "Application placements the cache manager keeps, by state (prepared, preparing, retained).",
	}, []string{"state"})
)

func init() {
	crmetrics.Registry.MustRegister(LocalApplicationLookups, LocalApplicationAttachments, LocalApplicationPlacements)
	for _, r := range []string{"NotOnNode", "PullSecretInvalid", "RegistryUnavailable", "ResolvePending"} {
		knownReasons[r] = true
	}
}
