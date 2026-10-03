// Package version holds build information injected with -ldflags.
package version

var (
	// Version is the kubeswift-spin release, for example "v0.1.0".
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = "unknown"
	// DefaultRuntimeImage is the runtime rootfs image a release build uses
	// when --runtime-image is not set. Release builds set it to the matching
	// ghcr.io/kubeswift-io/kubeswift-spin-runtime tag; development builds
	// leave it empty so a runtime image must be configured explicitly.
	DefaultRuntimeImage = ""
)
