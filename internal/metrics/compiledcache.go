package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Compiled-cache metrics (ADR 0014). Labels are fixed words; reasons outside
// the known set are counted as "Other".
var (
	CompiledCacheCompilations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeswift_spin_compiled_cache_compilations_total",
		Help: "Compiler sandboxes by result (started, succeeded, failed) and failure reason.",
	}, []string{"result", "reason"})
	CompiledCacheSignatures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeswift_spin_compiled_cache_signatures_total",
		Help: "Signing attempts of the cache signer by result (signed, rejected, failed).",
	}, []string{"result"})
	CompiledCacheLookups = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeswift_spin_compiled_cache_lookups_total",
		Help: "Replicas created for SpinApps that asked for a compiled cache, by result (hit: a signed cache was attached; miss: none yet; skipped: the SpinApp cannot have one).",
	}, []string{"result"})
	CompiledCacheAttachments = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeswift_spin_compiled_cache_attachments_total",
		Help: "Replicas that carried a compiled cache, by what KubeSwift did with it (mounted, skipped) and reason.",
	}, []string{"result", "reason"})
	CompiledCacheRecords = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubeswift_spin_compiled_cache_records",
		Help: "SpinCompiledCache records by Ready reason.",
	}, []string{"reason"})
)

func init() {
	crmetrics.Registry.MustRegister(CompiledCacheCompilations, CompiledCacheSignatures, CompiledCacheLookups,
		CompiledCacheAttachments, CompiledCacheRecords)
}

// knownReasons are the reasons counted under their own name: KubeSwift's
// sandbox, output and artifact reasons and kubeswift-spin's own.
var knownReasons = map[string]bool{
	"": true, "Other": true,
	// kubeswift-spin
	"CompilerDeleted": true, "CompilerReplaced": true, "CompilerRejected": true, "CompilerExited": true, "CompilerFailed": true,
	"CompileTimeout": true, "ExportInvalid": true, "SignerRejected": true, "NoConsumer": true,
	// KubeSwift outputs
	"OutputsNotSupported": true, "OutputExportFailed": true, "WorkloadFailed": true, "OutputDestinationDenied": true,
	"OutputDestinationAmbiguous": true, "OutputLimitExceedsPolicy": true, "OutputCredentialsMissing": true,
	"OutputDestinationRevoked": true, "OutputInvalidEntry": true, "OutputTooLarge": true, "OutputTooManyEntries": true,
	"OutputCollectionFailed": true, "RegistryUnauthorized": true, "RegistryUnavailable": true, "UploadTimeout": true,
	"OutputLost": true, "KernelUnsupported": true, "Timeout": true,
	// KubeSwift artifacts
	"Cached": true, "Fetched": true, "ResolveRefused": true, "FetchTimeout": true, "VerificationFailed": true,
	"ClaimMismatch": true, "PolicyDenied": true, "MaterializeFailed": true, "LauncherUnsupported": true,
	// Record Ready reasons
	"Pending": true, "Compiling": true, "CompileFailed": true, "Signing": true, "SignFailed": true, "Rejected": true,
	"NotCompiled": true, "Progressing": true, "PlacementFailed": true, "Ready": true, "NotTrusted": true,
}

// Reason returns r if it is a known reason and "Other" otherwise, so label
// cardinality stays bounded whatever a cluster component reports.
func Reason(r string) string {
	if knownReasons[r] {
		return r
	}
	return "Other"
}
