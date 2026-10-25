// Package capabilities discovers which upstream APIs and features the cluster
// serves. kubeswift-spin uses discovery rather than assuming a KubeSwift or
// SpinKube version, and fails at startup when a required API is missing.
package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// Required lists the API resources kubeswift-spin cannot run without.
var Required = []schema.GroupVersionResource{
	{Group: "core.spinkube.dev", Version: "v1alpha1", Resource: "spinapps"},
	{Group: "core.spinkube.dev", Version: "v1alpha1", Resource: "spinapps/status"},
	{Group: "core.spinkube.dev", Version: "v1alpha1", Resource: "spinappexecutors"},
	{Group: "sandbox.kubeswift.io", Version: "v1alpha1", Resource: "swiftsandboxes"},
}

// Pools is the optional warm-pool API.
var Pools = schema.GroupVersionResource{Group: "sandbox.kubeswift.io", Version: "v1alpha1", Resource: "swiftsandboxpools"}

// Placements is the SwiftArtifactPlacement API (KubeSwift v0.17.0), which
// prepares artifacts on the nodes of a warm pool. The cache manager
// requires it and refuses to start without it; compiled caches need
// KubeSwift v0.19.3 anyway, for outputs, platform verification and the
// optional-artifact fallback (docs/compiled-cache.md#requirements).
var Placements = schema.GroupVersionResource{Group: "sandbox.kubeswift.io", Version: "v1alpha1", Resource: "swiftartifactplacements"}

// CompiledCaches is kubeswift-spin's own SpinCompiledCache API, installed by
// the chart only when compiled caches are enabled.
var CompiledCaches = schema.GroupVersionResource{Group: "spin.kubeswift.io", Version: "v1alpha1", Resource: "spincompiledcaches"}

func installHint(group string) string {
	switch group {
	case "core.spinkube.dev":
		return "install the Spin Operator CRDs (tested: spin-operator v0.6.1, see docs/compatibility.md)"
	case "sandbox.kubeswift.io":
		return "install KubeSwift with the sandbox CRDs (tested: KubeSwift v0.16.0; v0.15.1 runs without exposure and secrets; see docs/compatibility.md)"
	}
	return "see docs/compatibility.md"
}

// CheckRequired verifies that every required resource is served. The error
// names each missing resource and how to install it.
func CheckRequired(dc discovery.DiscoveryInterface) error {
	var missing []string
	served := map[string]map[string]bool{}
	for _, gvr := range Required {
		gv := gvr.GroupVersion().String()
		if _, ok := served[gv]; !ok {
			served[gv] = map[string]bool{}
			list, err := dc.ServerResourcesForGroupVersion(gv)
			if err != nil {
				missing = append(missing, fmt.Sprintf("API group version %s is not served (%v): %s", gv, err, installHint(gvr.Group)))
				served[gv] = nil
				continue
			}
			for _, r := range list.APIResources {
				served[gv][r.Name] = true
			}
		}
		if served[gv] == nil {
			continue
		}
		if !served[gv][gvr.Resource] {
			missing = append(missing, fmt.Sprintf("resource %s in %s is not served: %s", gvr.Resource, gv, installHint(gvr.Group)))
		}
	}
	if len(missing) > 0 {
		return errors.New("required APIs are unavailable: " + strings.Join(missing, "; "))
	}
	return nil
}

// Served reports whether a single resource is served.
func Served(dc discovery.DiscoveryInterface, gvr schema.GroupVersionResource) (bool, error) {
	list, err := dc.ServerResourcesForGroupVersion(gvr.GroupVersion().String())
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, r := range list.APIResources {
		if r.Name == gvr.Resource {
			return true, nil
		}
	}
	return false, nil
}

// Sandbox describes optional SwiftSandbox features relevant to Spin. All of
// them first appear in KubeSwift v0.16.0.
type Sandbox struct {
	// Ports is true when spec.network.ports exists (inbound port exposure).
	Ports bool
	// ReadinessProbe is true when spec.readinessProbe exists.
	ReadinessProbe bool
	// PodMetadata is true when spec.podMetadata exists, letting a client
	// label the launcher pod so a Service can select it.
	PodMetadata bool
	// SecretFiles is true when spec.secretFiles exists. The same release
	// honors env[].valueFrom.secretKeyRef, which the schema cannot show.
	SecretFiles bool
	// Egress is true when spec.network.egress exists (restricted-mode
	// allowlist).
	Egress bool
	// ReadinessStartup is true when spec.readinessStartup exists: the
	// readiness probe can run at a sub-second interval until its first pass.
	ReadinessStartup bool
	// Termination is true when spec.termination exists (KubeSwift v0.16.3):
	// a deleted sandbox leaves Service endpoints, drains, stops its microVM
	// and keeps its NetworkPolicy until the launcher has stopped.
	// kubeswift-spin does not set the field; it only reports whether the
	// installed KubeSwift has this behavior.
	Termination bool
	// TrustedArtifacts is true when spec.artifacts[] has optional, claims
	// and requirePlatformVerification (KubeSwift v0.18.0): an artifact can
	// be skipped instead of failing the sandbox, and must carry a signature
	// a platform SwiftArtifactVerificationPolicy accepts with given claims.
	TrustedArtifacts bool
	// Outputs is true when spec.outputs exists (KubeSwift v0.19.0): a
	// sandbox can export a directory as an OCI artifact without registry
	// credentials in the guest.
	Outputs bool
	// LocalArtifacts is true when spec.artifacts[] has optional,
	// fetchTimeout, layout and pullSecretRef: a replica can mount its own
	// application as an optional OCI layout, read with its pull Secret
	// (ADR 0015).
	LocalArtifacts bool
}

// LocalApplication reports whether the installed KubeSwift has every
// sandbox feature local applications need (ADR 0015); the placement API is
// checked separately.
func (s Sandbox) LocalApplication() bool { return s.LocalArtifacts }

// CompiledCache reports whether the installed KubeSwift has every sandbox
// feature the trusted compiled cache needs (ADR 0014).
func (s Sandbox) CompiledCache() bool { return s.TrustedArtifacts && s.Outputs }

// Exposure reports whether every feature needed to put sandboxes behind a
// Service with readiness is present.
func (s Sandbox) Exposure() bool { return s.Ports && s.ReadinessProbe && s.PodMetadata }

// Secrets reports whether Secret values can be delivered to the guest
// without being written to any object.
func (s Sandbox) Secrets() bool { return s.SecretFiles }

// SandboxDetector reports Sandbox features, refreshing at most every TTL.
type SandboxDetector interface {
	Sandbox(ctx context.Context) (Sandbox, error)
}

// OpenAPIDetector inspects the published OpenAPI v3 schema of the
// sandbox.kubeswift.io group. Reading the schema needs no RBAC beyond the
// default system:discovery role, so no CRD read permission is required.
type OpenAPIDetector struct {
	Discovery discovery.DiscoveryInterface
	TTL       time.Duration

	mu      sync.Mutex
	cached  Sandbox
	ok      bool
	lastErr error
	fetched time.Time
}

const sandboxSchemaName = "io.kubeswift.sandbox.v1alpha1.SwiftSandbox"

// Sandbox returns the detected features.
// Failures are cached for the TTL too, so a cluster without OpenAPI v3 does
// not cost a discovery round trip on every reconcile.
//
// The features only change on a successful read. A failed read returns the
// last successful result: features feed the sandbox spec, so flipping them
// off on a transient error would replace every replica.
func (d *OpenAPIDetector) Sandbox(_ context.Context) (Sandbox, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.fetched.IsZero() && time.Since(d.fetched) < d.TTL {
		return d.cached, d.lastErr
	}
	s, err := d.fetch()
	d.fetched, d.lastErr = time.Now(), err
	if err == nil {
		d.cached, d.ok = s, true
	}
	return d.cached, err
}

// Ready reports whether at least one detection has succeeded.
func (d *OpenAPIDetector) Ready() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ok
}

func (d *OpenAPIDetector) fetch() (Sandbox, error) {
	paths, err := d.Discovery.OpenAPIV3().Paths()
	if err != nil {
		return Sandbox{}, fmt.Errorf("read OpenAPI v3 paths: %w", err)
	}
	gv, ok := paths["apis/sandbox.kubeswift.io/v1alpha1"]
	if !ok {
		return Sandbox{}, fmt.Errorf("OpenAPI v3 schema for sandbox.kubeswift.io/v1alpha1 is not published")
	}
	raw, err := gv.Schema("application/json")
	if err != nil {
		return Sandbox{}, fmt.Errorf("read sandbox.kubeswift.io OpenAPI schema: %w", err)
	}
	return ParseSandboxSchema(raw)
}

type schemaNode struct {
	Properties map[string]schemaNode `json:"properties"`
	Items      *schemaNode           `json:"items"`
}

// has reports whether the node has every named property.
func (n schemaNode) has(names ...string) bool {
	for _, name := range names {
		if _, ok := n.Properties[name]; !ok {
			return false
		}
	}
	return true
}

// ParseSandboxSchema extracts Sandbox features from an OpenAPI v3 document.
func ParseSandboxSchema(raw []byte) (Sandbox, error) {
	var doc struct {
		Components struct {
			Schemas map[string]schemaNode `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Sandbox{}, fmt.Errorf("parse sandbox.kubeswift.io OpenAPI schema: %w", err)
	}
	root, ok := doc.Components.Schemas[sandboxSchemaName]
	if !ok {
		return Sandbox{}, fmt.Errorf("schema %s not found in sandbox.kubeswift.io OpenAPI document", sandboxSchemaName)
	}
	spec := root.Properties["spec"]
	network := spec.Properties["network"]
	_, ports := network.Properties["ports"]
	_, egress := network.Properties["egress"]
	_, probe := spec.Properties["readinessProbe"]
	_, podMeta := spec.Properties["podMetadata"]
	_, secretFiles := spec.Properties["secretFiles"]
	_, startup := spec.Properties["readinessStartup"]
	_, termination := spec.Properties["termination"]
	trusted, local := false, false
	if a := spec.Properties["artifacts"].Items; a != nil {
		trusted = a.has("optional", "fetchTimeout", "claims", "requirePlatformVerification")
		local = a.has("optional", "fetchTimeout", "layout", "pullSecretRef")
	}
	outputs := false
	if o := spec.Properties["outputs"].Items; o != nil {
		outputs = o.has("name", "path", "repository", "maxSize", "artifactType")
	}
	return Sandbox{Ports: ports, ReadinessProbe: probe, PodMetadata: podMeta, SecretFiles: secretFiles, Egress: egress,
		ReadinessStartup: startup, Termination: termination, TrustedArtifacts: trusted, Outputs: outputs, LocalArtifacts: local}, nil
}

// Static is a SandboxDetector returning fixed features, for tests and for
// clusters where OpenAPI v3 is disabled.
type Static Sandbox

// Sandbox returns the fixed features.
func (s Static) Sandbox(context.Context) (Sandbox, error) { return Sandbox(s), nil }
