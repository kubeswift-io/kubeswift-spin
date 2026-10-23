// Command manager runs the kubeswift-spin controller, or, with
// --mode=cache-manager, the compiled-cache manager (ADR 0014). Both run from
// the same image as separate Deployments with separate service accounts.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kubeswift-io/kubeswift-spin/internal/cachemanager"
	"github.com/kubeswift-io/kubeswift-spin/internal/capabilities"
	"github.com/kubeswift-io/kubeswift-spin/internal/compatibility"
	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/controller"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
	"github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
	"github.com/kubeswift-io/kubeswift-spin/internal/scheme"
	"github.com/kubeswift-io/kubeswift-spin/internal/translate"
	"github.com/kubeswift-io/kubeswift-spin/internal/version"
)

// Upper bounds for the sizing flags. Replica ordinals use at most four
// digits in sandbox names (see translate.SandboxName).
const (
	maxReplicasFlag = 1000
	maxVCPUFlag     = 256
)

type options struct {
	metricsAddr     string
	metricsSecure   bool
	probeAddr       string
	leaderElect     bool
	leaderElectNS   string
	runtimeImage    string
	defaultCPU      string
	defaultMemory   string
	minMemory       string
	maxMemory       string
	maxVCPUs        int
	maxReplicas     int
	watchNamespaces string
	capabilityTTL   time.Duration
	cacheSyncTO     time.Duration

	mode              string
	cacheRepository   string
	cacheNamespace    string
	cacheArchitecture string
	cacheRetention    time.Duration
	cachePullSecret   string
	cacheFetchTimeout time.Duration
	cacheRuntimes     string
	cacheMaxRecords   int
	compilerCPU       int
	compilerMemory    string
	compilerTimeout   time.Duration
	cacheMaxSize      string
	cacheMaxEntries   int64
	maxCompilers      int
	maxCompilersPerNS int
}

// Modes of the manager binary.
const (
	modeController   = "controller"
	modeCacheManager = "cache-manager"
)

func main() {
	var o options
	flag.StringVar(&o.metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to. Use 0 to disable.")
	flag.BoolVar(&o.metricsSecure, "metrics-secure", false, "Serve metrics over HTTPS and require an authorized bearer token (needs the tokenreviews and subjectaccessreviews create permissions).")
	flag.StringVar(&o.probeAddr, "health-probe-bind-address", ":8081", "Address the health and readiness probes bind to.")
	flag.BoolVar(&o.leaderElect, "leader-elect", false, "Enable leader election so only one replica reconciles.")
	flag.StringVar(&o.leaderElectNS, "leader-election-namespace", "", "Namespace of the leader election Lease. Defaults to the pod namespace.")
	flag.StringVar(&o.runtimeImage, "runtime-image", version.DefaultRuntimeImage, "Default runtime rootfs image for executors that do not set "+executor.AnnRuntimeImage+". Must include a registry and a tag or digest.")
	flag.StringVar(&o.defaultCPU, "default-cpu", "1", "CPU used when a SpinApp sets no cpu request or limit.")
	flag.StringVar(&o.defaultMemory, "default-memory", "512Mi", "Memory used when a SpinApp sets no memory request or limit.")
	flag.StringVar(&o.minMemory, "min-memory", "256Mi", "Smallest guest memory accepted per replica.")
	flag.StringVar(&o.maxMemory, "max-memory", "16Gi", "Largest guest memory accepted per replica.")
	flag.IntVar(&o.maxVCPUs, "max-vcpus", 8, "Largest vCPU count accepted per replica.")
	flag.IntVar(&o.maxReplicas, "max-replicas", 20, "Largest spec.replicas accepted per SpinApp.")
	flag.StringVar(&o.watchNamespaces, "watch-namespaces", "", "Comma-separated namespaces to watch. Empty watches all namespaces.")
	flag.DurationVar(&o.capabilityTTL, "capability-refresh", 5*time.Minute, "How often to re-read the SwiftSandbox OpenAPI schema for optional features.")
	flag.DurationVar(&o.cacheSyncTO, "cache-sync-timeout", 2*time.Minute, "Fail startup if informers do not sync within this time (for example when RBAC forbids a watch).")
	flag.StringVar(&o.mode, "mode", modeController, "What to run: controller (the SpinApp controller) or cache-manager (the compiled-cache manager).")
	flag.StringVar(&o.cacheRepository, "compiled-cache-repository", "", "Base repository of compiled Wasmtime caches (registry host and path). Empty disables compiled caches.")
	flag.StringVar(&o.cacheNamespace, "compiled-cache-namespace", os.Getenv("POD_NAMESPACE"), "Namespace of the SpinCompiledCache records. Defaults to the pod namespace.")
	flag.StringVar(&o.cacheArchitecture, "compiled-cache-architecture", "amd64", "Architecture of compiled caches for executors whose node selector does not name kubernetes.io/arch: amd64 or arm64.")
	flag.StringVar(&o.cacheRuntimes, "compiled-cache-runtimes", "", "Comma-separated runtime images, repository@sha256:<digest>, that compiled caches are built and signed for. Executors with another runtime image get no cache.")
	flag.IntVar(&o.cacheMaxRecords, "compiled-cache-max-records-per-namespace", 32, "Most SpinCompiledCache records the cache manager keeps for one namespace.")
	cs := compiledcache.DefaultCompilerSettings()
	flag.IntVar(&o.compilerCPU, "compiled-cache-compiler-cpu", int(cs.CPU), "vCPUs of a compiler sandbox.")
	flag.StringVar(&o.compilerMemory, "compiled-cache-compiler-memory", cs.Memory.String(), "Memory of a compiler sandbox.")
	flag.DurationVar(&o.compilerTimeout, "compiled-cache-compiler-timeout", cs.Timeout, "Run time limit of a compiler sandbox (spec.timeout), 1m to 1h.")
	flag.StringVar(&o.cacheMaxSize, "compiled-cache-max-size", cs.MaxSize.String(), "Largest compiled cache a compiler may export.")
	flag.Int64Var(&o.cacheMaxEntries, "compiled-cache-max-entries", cs.MaxEntries, "Most files and directories a compiled cache may hold.")
	flag.IntVar(&o.maxCompilers, "compiled-cache-max-compilers", 4, "Most compiler sandboxes running at once, cluster-wide.")
	flag.IntVar(&o.maxCompilersPerNS, "compiled-cache-max-compilers-per-namespace", 1, "Most compiler sandboxes running at once in one namespace.")
	flag.DurationVar(&o.cacheRetention, "compiled-cache-retention", 24*time.Hour, "How long the cache manager keeps a SpinCompiledCache that no SpinApp uses (the most recently replaced one of each SpinApp is always kept).")
	flag.StringVar(&o.cachePullSecret, "compiled-cache-pull-secret", "", "Name of a docker-registry Secret, in every enabled namespace, that reads that namespace's cache repository. Empty uses the runtime image pull Secret for replicas and anonymous reads for placements.")
	flag.DurationVar(&o.cacheFetchTimeout, "compiled-cache-fetch-timeout", compiledcache.DefaultColdFetchTimeout, "How long a replica that does not come from a warm pool may wait for its compiled cache before it starts without it (0s to 5m); 0s gives such replicas no cache. Warm-pool replicas never wait.")
	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("starting kubeswift-spin", "version", version.Version, "commit", version.Commit, "mode", o.mode)

	if err := run(o); err != nil {
		setupLog.Error(err, "kubeswift-spin cannot start")
		os.Exit(1)
	}
}

func run(o options) error {
	setupLog := ctrl.Log.WithName("setup")
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}

	defaults, policy, err := o.policy()
	if err != nil {
		return err
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("create clientset: %w", err)
	}
	if err := capabilities.CheckRequired(cs.Discovery()); err != nil {
		return err
	}
	poolsServed, err := capabilities.Served(cs.Discovery(), capabilities.Pools)
	if err != nil {
		return fmt.Errorf("discover %s: %w", capabilities.Pools.String(), err)
	}
	settings := compiledcache.Settings{Namespace: o.cacheNamespace, Repository: o.cacheRepository, Architecture: o.cacheArchitecture,
		Runtimes: splitList(o.cacheRuntimes)}
	if err := settings.Validate(); err != nil {
		return fmt.Errorf("--compiled-cache-repository, --compiled-cache-architecture, --compiled-cache-runtimes: %w", err)
	}
	delivery := compiledcache.Delivery{PullSecret: o.cachePullSecret, ColdFetchTimeout: o.cacheFetchTimeout}
	if err := delivery.Validate(); err != nil {
		return fmt.Errorf("--compiled-cache-fetch-timeout: %w", err)
	}
	if errs := validation.IsDNS1123Subdomain(o.cachePullSecret); o.cachePullSecret != "" && len(errs) > 0 {
		return fmt.Errorf("--compiled-cache-pull-secret %q is not a Secret name: %s", o.cachePullSecret, strings.Join(errs, "; "))
	}
	cachesServed := false
	if settings.Configured() {
		if cachesServed, err = capabilities.Served(cs.Discovery(), capabilities.CompiledCaches); err != nil {
			return fmt.Errorf("discover %s: %w", capabilities.CompiledCaches.String(), err)
		}
		if !cachesServed {
			return fmt.Errorf("--compiled-cache-repository is set, but the SpinCompiledCache API (%s) is not installed; install the chart with compiledCache.enabled=true", capabilities.CompiledCaches.String())
		}
	}
	namespaces := splitList(o.watchNamespaces)
	switch o.mode {
	case modeController:
		if err := checkPermissions(context.Background(), cs, namespaces, poolsServed, controllerBase(), controllerChecks(settings)); err != nil {
			return err
		}
	case modeCacheManager:
		if !settings.Configured() {
			return fmt.Errorf("--mode=%s needs --compiled-cache-repository and --compiled-cache-namespace", modeCacheManager)
		}
		placementsServed, err := capabilities.Served(cs.Discovery(), capabilities.Placements)
		if err != nil {
			return fmt.Errorf("discover %s: %w", capabilities.Placements.String(), err)
		}
		if !placementsServed {
			return fmt.Errorf("the SwiftArtifactPlacement API (%s) is not installed; compiled caches need KubeSwift v0.19.1 or later", capabilities.Placements.String())
		}
		if err := checkPermissions(context.Background(), cs, namespaces, poolsServed, cacheManagerBase(), cacheManagerChecks(settings)); err != nil {
			return err
		}
	default:
		return fmt.Errorf("--mode must be %s or %s, got %q", modeController, modeCacheManager, o.mode)
	}
	setupLog.Info("required APIs and permissions verified", "warmPools", poolsServed, "compiledCaches", settings.Configured())

	// Sandbox features shape every sandbox spec, so the controller does not
	// reconcile before it knows them.
	detector := &capabilities.OpenAPIDetector{Discovery: cs.Discovery(), TTL: o.capabilityTTL}
	features, err := detector.Sandbox(context.Background())
	if err != nil {
		return fmt.Errorf("detect SwiftSandbox features from the OpenAPI v3 schema of sandbox.kubeswift.io/v1alpha1: %w", err)
	}
	setupLog.Info("SwiftSandbox features detected", "exposure", features.Exposure(), "secrets", features.Secrets(), "egress", features.Egress,
		"readinessStartup", features.ReadinessStartup, "termination", features.Termination, "compiledCache", features.CompiledCache())
	if settings.Configured() && !features.CompiledCache() {
		setupLog.Info("compiled caches are configured, but the installed KubeSwift has no platform-verified optional artifacts or sandbox outputs (KubeSwift v0.19.1 or later); SpinApps run without them")
	}
	if features.Exposure() && !features.Termination {
		setupLog.Info("KubeSwift is older than v0.16.3 (no spec.termination): a deleted replica is not drained and loses its NetworkPolicy while it can still serve; upgrade KubeSwift to v0.16.3 or later")
	}

	recordNS := ""
	if settings.Configured() {
		recordNS = settings.Namespace
	}
	cacheOpts := controller.CacheOptions(namespaces, recordNS)
	if o.mode == modeCacheManager {
		// The cache manager only needs its compiler sandboxes, not the
		// SpinApps' replicas.
		cacheOpts.ByObject[&sandboxapi.SwiftSandbox{}] = cache.ByObject{Label: labels.SelectorFromSet(labels.Set{
			translate.LabelManagedBy: translate.ManagedByValue, compiledcache.LabelComponent: compiledcache.ComponentCompiler})}
		cacheOpts.ByObject[&sandboxapi.SwiftArtifactPlacement{}] = cache.ByObject{Label: labels.SelectorFromSet(labels.Set{
			translate.LabelManagedBy: translate.ManagedByValue, compiledcache.LabelComponent: compiledcache.ComponentPlacement})}
	}

	metricsOpts := metricsserver.Options{BindAddress: o.metricsAddr, SecureServing: o.metricsSecure}
	if o.metricsSecure {
		metricsOpts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                        scheme.New(),
		Cache:                         cacheOpts,
		Metrics:                       metricsOpts,
		HealthProbeBindAddress:        o.probeAddr,
		LeaderElection:                o.leaderElect,
		LeaderElectionID:              leaderElectionID(o.mode),
		LeaderElectionNamespace:       o.leaderElectNS,
		LeaderElectionReleaseOnCancel: true,
		Controller:                    config.Controller{CacheSyncTimeout: o.cacheSyncTO},
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	ctx := ctrl.SetupSignalHandler()
	if o.mode == modeCacheManager {
		if o.cacheMaxRecords < 1 || o.cacheMaxRecords > 1000 {
			return fmt.Errorf("--compiled-cache-max-records-per-namespace must be between 1 and 1000")
		}
		ns := &cachemanager.NamespaceReconciler{
			Client:     mgr.GetClient(),
			Defaults:   defaults,
			Options:    compatibility.Options{MaxReplicas: int32(o.maxReplicas), Resources: policy}, //nolint:gosec // bounded in policy()
			Settings:   settings,
			Delivery:   delivery,
			Detector:   detector,
			Retention:  o.cacheRetention,
			MaxRecords: o.cacheMaxRecords,
		}
		if err := ns.SetupWithManager(mgr); err != nil {
			return err
		}
		compiler, err := o.compilerSettings()
		if err != nil {
			return err
		}
		rec := &cachemanager.RecordReconciler{
			Client:                   mgr.GetClient(),
			APIReader:                mgr.GetAPIReader(),
			Settings:                 settings,
			Delivery:                 delivery,
			Compiler:                 compiler,
			Defaults:                 defaults,
			Recorder:                 mgr.GetEventRecorder("kubeswift-spin-cache-manager"),
			MaxCompilers:             o.maxCompilers,
			MaxCompilersPerNamespace: o.maxCompilersPerNS,
		}
		if err := rec.SetupWithManager(mgr); err != nil {
			return err
		}
	} else {
		r := &controller.SpinAppReconciler{
			Client:               mgr.GetClient(),
			Scheme:               mgr.GetScheme(),
			Recorder:             mgr.GetEventRecorder("kubeswift-spin"),
			Defaults:             defaults,
			Options:              compatibility.Options{MaxReplicas: int32(o.maxReplicas), Resources: policy}, //nolint:gosec // bounded in policy()
			Detector:             detector,
			PoolsServed:          poolsServed,
			CompiledCache:        settings,
			CompiledCachesServed: cachesServed,
			CacheDelivery:        delivery,
		}
		if err := r.SetupWithManager(ctx, mgr); err != nil {
			return err
		}
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	// Ready only once the informer caches have synced.
	if err := mgr.AddReadyzCheck("readyz", func(req *http.Request) error {
		if !mgr.GetCache().WaitForCacheSync(req.Context()) {
			return errors.New("informer caches not synced")
		}
		return nil
	}); err != nil {
		return err
	}
	return mgr.Start(ctx)
}

// compilerSettings validates the compiler sandbox flags.
func (o options) compilerSettings() (compiledcache.CompilerSettings, error) {
	var cs compiledcache.CompilerSettings
	if o.compilerCPU < 1 || o.compilerCPU > maxVCPUFlag {
		return cs, fmt.Errorf("--compiled-cache-compiler-cpu must be between 1 and %d", maxVCPUFlag)
	}
	cs.CPU = int32(o.compilerCPU)
	var err error
	if cs.Memory, err = resource.ParseQuantity(o.compilerMemory); err != nil || cs.Memory.Sign() <= 0 {
		return cs, fmt.Errorf("--compiled-cache-compiler-memory must be a positive quantity, got %q", o.compilerMemory)
	}
	if o.compilerTimeout < time.Minute || o.compilerTimeout > time.Hour {
		return cs, fmt.Errorf("--compiled-cache-compiler-timeout must be between 1m and 1h")
	}
	cs.Timeout = o.compilerTimeout
	if cs.MaxSize, err = resource.ParseQuantity(o.cacheMaxSize); err != nil || cs.MaxSize.Sign() <= 0 {
		return cs, fmt.Errorf("--compiled-cache-max-size must be a positive quantity, got %q", o.cacheMaxSize)
	}
	if o.cacheMaxEntries < 2 || o.cacheMaxEntries > 1<<20 {
		return cs, fmt.Errorf("--compiled-cache-max-entries must be between 2 and %d", 1<<20)
	}
	cs.MaxEntries = o.cacheMaxEntries
	if o.maxCompilers < 1 || o.maxCompilersPerNS < 1 || o.maxCompilersPerNS > o.maxCompilers {
		return cs, fmt.Errorf("--compiled-cache-max-compilers and --compiled-cache-max-compilers-per-namespace must be at least 1, the second at most the first")
	}
	return cs, nil
}

func (o options) policy() (executor.Defaults, translate.ResourcePolicy, error) {
	parse := func(flagName, v string) (resource.Quantity, error) {
		q, err := resource.ParseQuantity(v)
		if err != nil || q.Sign() <= 0 {
			return resource.Quantity{}, fmt.Errorf("--%s must be a positive quantity, got %q", flagName, v)
		}
		return q, nil
	}
	var d executor.Defaults
	var p translate.ResourcePolicy
	var err error
	if d.DefaultCPU, err = parse("default-cpu", o.defaultCPU); err != nil {
		return d, p, err
	}
	if d.DefaultMemory, err = parse("default-memory", o.defaultMemory); err != nil {
		return d, p, err
	}
	if p.MinMemory, err = parse("min-memory", o.minMemory); err != nil {
		return d, p, err
	}
	if p.MaxMemory, err = parse("max-memory", o.maxMemory); err != nil {
		return d, p, err
	}
	if o.maxVCPUs < 1 || o.maxVCPUs > maxVCPUFlag {
		return d, p, fmt.Errorf("--max-vcpus must be between 1 and %d", maxVCPUFlag)
	}
	if o.maxReplicas < 1 || o.maxReplicas > maxReplicasFlag {
		return d, p, fmt.Errorf("--max-replicas must be between 1 and %d", maxReplicasFlag)
	}
	p.MaxVCPU = int32(o.maxVCPUs)
	if o.runtimeImage != "" {
		if err := executor.ValidateRuntimeImage(o.runtimeImage); err != nil {
			return d, p, fmt.Errorf("--runtime-image: %w", err)
		}
	}
	d.RuntimeImage = o.runtimeImage
	return d, p, nil
}

type check struct {
	group, resource, subresource, verb string
}

// checkPermissions fails fast with an actionable message instead of letting
// a forbidden informer leave a healthy-looking process that does nothing.
func checkPermissions(ctx context.Context, cs kubernetes.Interface, namespaces []string, pools bool, checks []check, extra []namespacedCheck) error {
	if pools {
		checks = append(checks,
			check{"sandbox.kubeswift.io", "swiftsandboxpools", "", "list"},
			check{"sandbox.kubeswift.io", "swiftsandboxpools", "", "watch"})
	}
	scopes := namespaces
	if len(scopes) == 0 {
		scopes = []string{""}
	}
	type scoped struct {
		ns string
		c  check
	}
	var all []scoped
	for _, ns := range scopes {
		for _, c := range checks {
			all = append(all, scoped{ns, c})
		}
	}
	for _, e := range extra {
		if e.namespace != "" {
			all = append(all, scoped{e.namespace, e.check})
			continue
		}
		for _, ns := range scopes {
			all = append(all, scoped{ns, e.check})
		}
	}
	var denied []string
	{
		for _, sc := range all {
			ns, c := sc.ns, sc.c
			ssar := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: ns, Group: c.group, Resource: c.resource, Subresource: c.subresource, Verb: c.verb,
				},
			}}
			res, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, ssar, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("check permissions: %w", err)
			}
			if !res.Status.Allowed {
				r := c.resource
				if c.subresource != "" {
					r += "/" + c.subresource
				}
				scope := "all namespaces"
				if ns != "" {
					scope = "namespace " + ns
				}
				denied = append(denied, fmt.Sprintf("%s %s.%s in %s", c.verb, r, c.group, scope))
			}
		}
	}
	if len(denied) > 0 {
		return fmt.Errorf("the service account is missing permissions: %s (install the RBAC from charts/kubeswift-spin)", strings.Join(denied, ", "))
	}
	return nil
}

// namespacedCheck is an extra permission check, in one namespace or, with
// an empty namespace, in every watched scope.
type namespacedCheck struct {
	namespace string
	check     check
}

// controllerBase are the SpinApp controller's permissions in every watched
// scope.
func controllerBase() []check {
	return []check{
		{"core.spinkube.dev", "spinapps", "", "list"},
		{"core.spinkube.dev", "spinapps", "", "watch"},
		{"core.spinkube.dev", "spinapps", "status", "patch"},
		// blockOwnerDeletion on sandbox owner references needs this when
		// the OwnerReferencesPermissionEnforcement admission plugin is on.
		{"core.spinkube.dev", "spinapps", "finalizers", "update"},
		{"events.k8s.io", "events", "", "create"},
		{"core.spinkube.dev", "spinappexecutors", "", "list"},
		{"core.spinkube.dev", "spinappexecutors", "", "watch"},
		{"sandbox.kubeswift.io", "swiftsandboxes", "", "list"},
		{"sandbox.kubeswift.io", "swiftsandboxes", "", "watch"},
		{"sandbox.kubeswift.io", "swiftsandboxes", "", "create"},
		{"sandbox.kubeswift.io", "swiftsandboxes", "", "delete"},
	}
}

// cacheManagerBase are the cache manager's permissions in every watched
// scope: it reads SpinApps and executors and never writes them.
func cacheManagerBase() []check {
	return []check{
		{"core.spinkube.dev", "spinapps", "", "list"},
		{"core.spinkube.dev", "spinapps", "", "watch"},
		{"core.spinkube.dev", "spinappexecutors", "", "list"},
		{"core.spinkube.dev", "spinappexecutors", "", "watch"},
		// Compiler sandboxes, in the namespaces of the SpinApps they compile.
		{"sandbox.kubeswift.io", "swiftsandboxes", "", "list"},
		{"sandbox.kubeswift.io", "swiftsandboxes", "", "watch"},
		{"sandbox.kubeswift.io", "swiftsandboxes", "", "create"},
		{"sandbox.kubeswift.io", "swiftsandboxes", "", "delete"},
		// Placements on the warm pools of consumer SpinApps.
		{"sandbox.kubeswift.io", "swiftartifactplacements", "", "list"},
		{"sandbox.kubeswift.io", "swiftartifactplacements", "", "watch"},
		{"sandbox.kubeswift.io", "swiftartifactplacements", "", "create"},
		{"sandbox.kubeswift.io", "swiftartifactplacements", "", "update"},
		{"sandbox.kubeswift.io", "swiftartifactplacements", "", "delete"},
	}
}

// controllerChecks are the controller's compiled-cache permissions: it only
// reads records.
func controllerChecks(s compiledcache.Settings) []namespacedCheck {
	if !s.Configured() {
		return nil
	}
	return []namespacedCheck{
		{s.Namespace, check{"spin.kubeswift.io", "spincompiledcaches", "", "list"}},
		{s.Namespace, check{"spin.kubeswift.io", "spincompiledcaches", "", "watch"}},
		{"", check{"", "namespaces", "", "list"}},
		{"", check{"", "namespaces", "", "watch"}},
	}
}

// cacheManagerChecks are the cache manager's permissions.
func cacheManagerChecks(s compiledcache.Settings) []namespacedCheck {
	out := []namespacedCheck{}
	for _, verb := range []string{"list", "watch", "create", "delete"} {
		out = append(out, namespacedCheck{s.Namespace, check{"spin.kubeswift.io", "spincompiledcaches", "", verb}})
	}
	out = append(out, namespacedCheck{s.Namespace, check{"spin.kubeswift.io", "spincompiledcaches", "status", "patch"}},
		namespacedCheck{"", check{"", "namespaces", "", "list"}},
		namespacedCheck{"", check{"", "namespaces", "", "watch"}},
		namespacedCheck{s.Namespace, check{"events.k8s.io", "events", "", "create"}})
	return out
}

func leaderElectionID(mode string) string {
	if mode == modeCacheManager {
		return "kubeswift-spin-cache-manager.spin.kubeswift.io"
	}
	return "kubeswift-spin.spin.kubeswift.io"
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
