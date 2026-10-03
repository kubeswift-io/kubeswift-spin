// Command manager runs the kubeswift-spin controller.
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
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kubeswift-io/kubeswift-spin/internal/capabilities"
	"github.com/kubeswift-io/kubeswift-spin/internal/compatibility"
	"github.com/kubeswift-io/kubeswift-spin/internal/controller"
	"github.com/kubeswift-io/kubeswift-spin/internal/executor"
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
}

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
	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("starting kubeswift-spin", "version", version.Version, "commit", version.Commit)

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
	namespaces := splitList(o.watchNamespaces)
	if err := checkPermissions(context.Background(), cs, namespaces, poolsServed); err != nil {
		return err
	}
	setupLog.Info("required APIs and permissions verified", "warmPools", poolsServed)

	cacheOpts := controller.CacheOptions(namespaces)

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
		LeaderElectionID:              "kubeswift-spin.spin.kubeswift.io",
		LeaderElectionNamespace:       o.leaderElectNS,
		LeaderElectionReleaseOnCancel: true,
		Controller:                    config.Controller{CacheSyncTimeout: o.cacheSyncTO},
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	ctx := ctrl.SetupSignalHandler()
	r := &controller.SpinAppReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Recorder:    mgr.GetEventRecorder("kubeswift-spin"),
		Defaults:    defaults,
		Options:     compatibility.Options{MaxReplicas: int32(o.maxReplicas), Resources: policy}, //nolint:gosec // bounded in policy()
		Detector:    &capabilities.OpenAPIDetector{Discovery: cs.Discovery(), TTL: o.capabilityTTL},
		PoolsServed: poolsServed,
	}
	if err := r.SetupWithManager(ctx, mgr); err != nil {
		return err
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
func checkPermissions(ctx context.Context, cs kubernetes.Interface, namespaces []string, pools bool) error {
	checks := []check{
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
	if pools {
		checks = append(checks,
			check{"sandbox.kubeswift.io", "swiftsandboxpools", "", "list"},
			check{"sandbox.kubeswift.io", "swiftsandboxpools", "", "watch"})
	}
	scopes := namespaces
	if len(scopes) == 0 {
		scopes = []string{""}
	}
	var denied []string
	for _, ns := range scopes {
		for _, c := range checks {
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
		return fmt.Errorf("the controller service account is missing permissions: %s (install the RBAC from charts/kubeswift-spin)", strings.Join(denied, ", "))
	}
	return nil
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
