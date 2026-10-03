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

func installHint(group string) string {
	switch group {
	case "core.spinkube.dev":
		return "install the Spin Operator CRDs (tested: spin-operator v0.6.1, see docs/compatibility.md)"
	case "sandbox.kubeswift.io":
		return "install KubeSwift with the sandbox CRDs (tested: KubeSwift v0.15.1, see docs/compatibility.md)"
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

// Sandbox describes optional SwiftSandbox features relevant to Spin.
type Sandbox struct {
	// Ports is true when spec.network.ports exists (inbound port exposure).
	Ports bool
	// ReadinessProbe is true when spec.readinessProbe exists.
	ReadinessProbe bool
	// PodMetadata is true when spec.podMetadata exists, letting a client
	// label the launcher pod so a Service can select it.
	PodMetadata bool
}

// Exposure reports whether every feature needed to put sandboxes behind a
// Service with readiness is present. The field names follow the proposal in
// docs/upstream/kubeswift-sandbox-service-exposure.md.
func (s Sandbox) Exposure() bool { return s.Ports && s.ReadinessProbe && s.PodMetadata }

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
	lastErr error
	fetched time.Time
}

const sandboxSchemaName = "io.kubeswift.sandbox.v1alpha1.SwiftSandbox"

// Sandbox returns the detected features.
// Failures are cached for the TTL too, so a cluster without OpenAPI v3 does
// not cost a discovery round trip on every reconcile.
func (d *OpenAPIDetector) Sandbox(_ context.Context) (Sandbox, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.fetched.IsZero() && time.Since(d.fetched) < d.TTL {
		return d.cached, d.lastErr
	}
	s, err := d.fetch()
	d.fetched, d.lastErr = time.Now(), err
	if err == nil {
		d.cached = s
	}
	return d.cached, err
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
	_, ports := spec.Properties["network"].Properties["ports"]
	_, probe := spec.Properties["readinessProbe"]
	_, podMeta := spec.Properties["podMetadata"]
	return Sandbox{Ports: ports, ReadinessProbe: probe, PodMetadata: podMeta}, nil
}

// Static is a SandboxDetector returning fixed features, for tests and for
// clusters where OpenAPI v3 is disabled.
type Static Sandbox

// Sandbox returns the fixed features.
func (s Static) Sandbox(context.Context) (Sandbox, error) { return Sandbox(s), nil }
