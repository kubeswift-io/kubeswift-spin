package controller

import (
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"

	"github.com/kubeswift-io/kubeswift-spin/internal/translate"
)

// CacheOptions returns the informer cache configuration. Only SwiftSandboxes
// created by kubeswift-spin are cached, which bounds memory use on clusters
// with many unrelated sandboxes. An empty namespaces list watches all
// namespaces.
func CacheOptions(namespaces []string) cache.Options {
	opts := cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&sandboxv1alpha1.SwiftSandbox{}: {Label: labels.SelectorFromSet(labels.Set{translate.LabelManagedBy: translate.ManagedByValue})},
		},
	}
	if len(namespaces) > 0 {
		opts.DefaultNamespaces = map[string]cache.Config{}
		for _, ns := range namespaces {
			opts.DefaultNamespaces[ns] = cache.Config{}
		}
	}
	return opts
}
