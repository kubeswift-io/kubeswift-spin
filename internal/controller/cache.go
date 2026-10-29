package controller

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"

	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/localapp"
	"github.com/kubeswift-io/kubeswift-spin/internal/translate"
)

// CacheOptions returns the informer cache configuration. Only SwiftSandboxes
// created by kubeswift-spin are cached, which bounds memory use on clusters
// with many unrelated sandboxes, and managedFields, which the controller
// never reads (it does not use server-side apply), are dropped from every
// cached object. An empty namespaces list watches all namespaces.
// SpinCompiledCache records are watched only in recordNamespace (the
// kubeswift-spin namespace), and only when it is set.
func CacheOptions(namespaces []string, recordNamespace string) cache.Options {
	opts := cache.Options{
		DefaultTransform: cache.TransformStripManagedFields(),
		ByObject: map[client.Object]cache.ByObject{
			&sandboxv1alpha1.SwiftSandbox{}: {Label: labels.SelectorFromSet(labels.Set{translate.LabelManagedBy: translate.ManagedByValue})},
		},
	}
	if recordNamespace != "" {
		opts.ByObject[&cachev1alpha1.SpinCompiledCache{}] = cache.ByObject{Namespaces: map[string]cache.Config{recordNamespace: {}}}
	}
	if len(namespaces) > 0 {
		opts.DefaultNamespaces = map[string]cache.Config{}
		for _, ns := range namespaces {
			opts.DefaultNamespaces[ns] = cache.Config{}
		}
	}
	return opts
}

// PlacementSelector selects the SwiftArtifactPlacements the controller
// caches: application placements, which it reads while local applications
// are enabled and deletes while they are disabled, and compiled-cache
// placements while compiled caches are disabled (PlacementCleanup).
func PlacementSelector(cleanup PlacementCleanup) labels.Selector {
	components := []string{localapp.ComponentPlacement}
	if cleanup.CompiledCaches {
		components = append(components, compiledcache.ComponentPlacement)
	}
	sel, err := labels.Parse(fmt.Sprintf("%s=%s,%s in (%s)", translate.LabelManagedBy, translate.ManagedByValue,
		compiledcache.LabelComponent, strings.Join(components, ",")))
	if err != nil {
		panic(err) // constant label keys and values
	}
	return sel
}
