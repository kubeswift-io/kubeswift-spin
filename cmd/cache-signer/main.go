// Command cache-signer is the kubeswift-spin cache signer (ADR 0014): the
// only component that holds the compiled-cache signing key. It checks each
// SpinCompiledCache's compiler sandbox and exported artifact, then signs the
// artifact's digest with the record's claims.
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

	"github.com/google/go-containerregistry/pkg/authn"
	"k8s.io/apimachinery/pkg/api/resource"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/scheme"
	"github.com/kubeswift-io/kubeswift-spin/internal/signer"
	"github.com/kubeswift-io/kubeswift-spin/internal/version"
)

func main() {
	var (
		metricsAddr, probeAddr, leaderNS     string
		metricsSecure, leaderElect           bool
		namespace, repository, arch, runtime string
		key, cosignBin, signingConfig        string
		maxSize                              string
		maxEntries                           int
		cosignTimeout                        time.Duration
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to. Use 0 to disable.")
	flag.BoolVar(&metricsSecure, "metrics-secure", false, "Serve metrics over HTTPS and require an authorized bearer token.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health and readiness probes bind to.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election so only one replica signs.")
	flag.StringVar(&leaderNS, "leader-election-namespace", "", "Namespace of the leader election Lease. Defaults to the pod namespace.")
	flag.StringVar(&namespace, "compiled-cache-namespace", os.Getenv("POD_NAMESPACE"), "Namespace of the SpinCompiledCache records.")
	flag.StringVar(&repository, "compiled-cache-repository", "", "Base repository of compiled caches (registry host and path).")
	flag.StringVar(&arch, "compiled-cache-architecture", "amd64", "Default architecture of compiled caches.")
	flag.StringVar(&runtime, "compiled-cache-runtimes", "", "Comma-separated runtime images, repository@sha256:<digest>, the signer signs caches for.")
	flag.StringVar(&key, "key", "", "Signing key: a file holding a cosign private key (password in COSIGN_PASSWORD) or a KMS reference cosign understands.")
	flag.StringVar(&cosignBin, "cosign", "/usr/local/bin/cosign", "The cosign binary.")
	flag.StringVar(&signingConfig, "signing-config", "/etc/kubeswift-spin-signer/signing-config.json", "Sigstore signing configuration without a transparency log.")
	flag.StringVar(&maxSize, "compiled-cache-max-size", "512Mi", "Largest compiled cache the signer reads.")
	flag.IntVar(&maxEntries, "compiled-cache-max-entries", 10000, "Most files and directories a compiled cache may hold.")
	flag.DurationVar(&cosignTimeout, "cosign-timeout", 2*time.Minute, "Time limit of one cosign call.")
	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("starting kubeswift-spin cache signer", "version", version.Version, "commit", version.Commit)

	settings := compiledcache.Settings{Namespace: namespace, Repository: repository, Architecture: arch, Runtimes: splitList(runtime)}
	err := run(settings, key, cosignBin, signingConfig, maxSize, maxEntries, cosignTimeout,
		metricsAddr, metricsSecure, probeAddr, leaderElect, leaderNS)
	if err != nil {
		setupLog.Error(err, "the cache signer cannot start")
		os.Exit(1)
	}
}

func run(settings compiledcache.Settings, key, cosignBin, signingConfig, maxSize string, maxEntries int, cosignTimeout time.Duration,
	metricsAddr string, metricsSecure bool, probeAddr string, leaderElect bool, leaderNS string) error {
	if !settings.Configured() {
		return errors.New("--compiled-cache-repository and --compiled-cache-namespace are required")
	}
	if err := settings.Validate(); err != nil {
		return err
	}
	if key == "" {
		return errors.New("--key is required")
	}
	if err := signer.CheckSigningConfig(signingConfig); err != nil {
		return err
	}
	size, err := resource.ParseQuantity(maxSize)
	if err != nil || size.Sign() <= 0 {
		return fmt.Errorf("--compiled-cache-max-size must be a positive quantity, got %q", maxSize)
	}
	if maxEntries < 2 {
		return errors.New("--compiled-cache-max-entries must be at least 2")
	}

	cosign := &signer.Cosign{Binary: cosignBin, Key: key, SigningConfig: signingConfig, Timeout: cosignTimeout, Env: os.Environ()}
	keyID, err := cosign.KeyID(context.Background())
	if err != nil {
		return fmt.Errorf("read the signing key's public key: %w", err)
	}
	ctrl.Log.WithName("setup").Info("signing key loaded", "keyID", keyID)

	metricsOpts := metricsserver.Options{BindAddress: metricsAddr, SecureServing: metricsSecure}
	if metricsSecure {
		metricsOpts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme.New(),
		Cache: cache.Options{
			DefaultTransform: cache.TransformStripManagedFields(),
			ByObject: map[client.Object]cache.ByObject{
				&cachev1alpha1.SpinCompiledCache{}: {Namespaces: map[string]cache.Config{settings.Namespace: {}}},
			},
		},
		Metrics:                       metricsOpts,
		HealthProbeBindAddress:        probeAddr,
		LeaderElection:                leaderElect,
		LeaderElectionID:              "kubeswift-spin-cache-signer.spin.kubeswift.io",
		LeaderElectionNamespace:       leaderNS,
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	r := &signer.Reconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Settings:  settings,
		Fetcher:   &signer.Fetcher{Keychain: authn.DefaultKeychain, Limits: signer.Limits{MaxSize: size.Value(), MaxEntries: maxEntries}},
		Signer:    cosign,
		Identity:  "kubeswift-spin-cache-signer/" + version.Version,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", func(req *http.Request) error {
		if !mgr.GetCache().WaitForCacheSync(req.Context()) {
			return errors.New("informer caches not synced")
		}
		return nil
	}); err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
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
