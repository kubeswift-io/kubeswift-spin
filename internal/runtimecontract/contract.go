// Package runtimecontract defines the interface between the controller, which
// builds the SwiftSandbox command line and environment, and the entrypoint in
// the kubeswift-spin runtime image, which prepares the guest and execs Spin.
//
// Both sides import this package so the paths and variable names cannot drift.
// Changing any value here changes the sandbox spec of every SpinApp and
// therefore rolls every instance, so treat it as a versioned contract.
package runtimecontract

import "regexp"

const (
	// EntrypointPath is the absolute path of the entrypoint binary in the
	// runtime image. The controller always sets it as the sandbox command so
	// warm-pool checkout (which requires a command) works.
	EntrypointPath = "/usr/local/bin/kubeswift-spin-entrypoint"

	// SpinPath is the absolute path of the pinned Spin binary.
	SpinPath = "/usr/local/bin/spin"

	// BaseDir is the writable root the entrypoint prepares inside the guest.
	// The sandbox rootfs is a read-only image with a tmpfs overlay, so this
	// directory lives in guest RAM and does not survive a restart.
	BaseDir = "/var/lib/kubeswift-spin"

	// StateDir is a writable directory for runtime-config stores that name
	// an explicit path, for example a "spin" key-value store with
	// path = "/var/lib/kubeswift-spin/state/kv.db". It is not passed as
	// `spin up --state-dir`, so Spin's default stores stay in memory.
	StateDir = BaseDir + "/state"

	// HomeDir is HOME for the Spin process. Spin resolves its registry cache
	// and Wasmtime cache under the XDG directories derived from it.
	HomeDir = BaseDir + "/home"

	// TmpDir is TMPDIR for the Spin process.
	TmpDir = BaseDir + "/tmp"

	// RuntimeConfigPath is where the entrypoint writes the runtime-config TOML
	// that the controller rendered, and the value passed to
	// `spin up --runtime-config-file`.
	RuntimeConfigPath = BaseDir + "/runtime-config.toml"

	// RuntimeConfigEnv carries the base64-encoded runtime-config TOML. The
	// controller only ever places non-secret values in it; see
	// docs/security-model.md.
	RuntimeConfigEnv = "KUBESWIFT_SPIN_RUNTIME_CONFIG_B64"

	// SecretDir is where KubeSwift secretFiles are written in the guest
	// (KubeSwift v0.16.0 and later). Files there belong to root with mode
	// 0400; the entrypoint reads them before dropping root and hands the
	// Spin user its own copies.
	SecretDir = "/run/kubeswift-spin" //nolint:gosec // a directory path, not a credential

	// SecretRuntimeConfigPath receives the runtime-config.toml key of the
	// Secret named by SpinApp spec.runtimeConfig.loadFromSecret.
	SecretRuntimeConfigPath = SecretDir + "/runtime-config.toml"

	// RuntimeConfigFileEnv names a runtime-config file delivered as a secret
	// file. It is mutually exclusive with RuntimeConfigEnv.
	RuntimeConfigFileEnv = "KUBESWIFT_SPIN_RUNTIME_CONFIG_FILE"

	// SecretValueEnvPrefix prefixes environment variables that carry Secret
	// values for runtime-config options (env valueFrom.secretKeyRef). The
	// rendered runtime config holds SecretPlaceholderPrefix+<variable name>
	// in place of each value; the entrypoint substitutes and removes the
	// variables before Spin starts.
	SecretValueEnvPrefix    = "KUBESWIFT_SPIN_SECRET_" //nolint:gosec // a variable name prefix, not a credential
	SecretPlaceholderPrefix = "kubeswift-spin-secret:" //nolint:gosec // a placeholder marker, not a credential

	// RegistryAuthDir receives the .dockerconfigjson key of each Secret in
	// SpinApp spec.imagePullSecrets, as <index>.json.
	RegistryAuthDir = SecretDir + "/registry-auth"

	// RegistryAuthFilesEnv lists those files, comma-separated. The entrypoint
	// merges them into HomeDir/.docker/config.json, where Spin's registry
	// client looks for credentials.
	RegistryAuthFilesEnv = "KUBESWIFT_SPIN_REGISTRY_AUTH_FILES"

	// HTTPPortName is the name of the exposed Spin HTTP port. Spin Operator's
	// SpinApp Service targets this name.
	HTTPPortName = "http-app"

	// ListenPort is the port Spin's HTTP trigger listens on inside the guest.
	// It is unprivileged so the Spin process does not need any capability
	// after the entrypoint drops root.
	ListenPort = 3000

	// CompileCommand is the entrypoint's compile mode (ADR 0014): it
	// compiles a Spin application into a Wasmtime cache and writes it, with
	// CompiledCacheMetadata, to CompileOutputDir, which KubeSwift exports
	// as an OCI artifact. It never serves requests.
	CompileCommand = "compile"

	// CompileOutputDir is the guest directory of the compiler sandbox's
	// KubeSwift output (spec.outputs[].path).
	CompileOutputDir = "/var/lib/kubeswift-spin-output"

	// CompileWorkDir is compile mode's writable scratch directory.
	CompileWorkDir = BaseDir + "/compile"

	// CompileListenAddress is the loopback listener of `spin up` in compile
	// mode. Nothing outside the guest can reach it, and nothing connects to
	// it except the entrypoint's TCP check that compilation finished.
	CompileListenAddress = "127.0.0.1:3999"

	// CompiledCacheMetadataFile is the metadata file at the root of a
	// compiled cache artifact; CompiledCacheEntries is the directory that
	// holds Wasmtime's cache layout (modules/wasmtime-<version>/<key>).
	CompiledCacheMetadataFile = "metadata.json"
	CompiledCacheEntries      = "cache"

	// CompiledCacheKind is the "kind" of compiled cache metadata.
	CompiledCacheKind = "kubeswift-spin.wasmtime-cache"

	// WasmtimeCacheVersion is the version directory of the Wasmtime cache
	// layout of the Spin in this runtime image. Compile mode refuses to
	// write another, and the entrypoint uses only a cache that names it.
	// Update it with Spin; hack/test-runtime-image.sh checks it.
	WasmtimeCacheVersion = "wasmtime-49.0.1"

	// CompiledCacheDir is where a replica's signed compiled cache is
	// mounted, read-only, when KubeSwift delivers it (ADR 0014).
	CompiledCacheDir = "/opt/kubeswift-spin-cache"

	// CompiledCacheConfigPath is the Wasmtime cache configuration the
	// entrypoint writes when it uses the mounted cache.
	CompiledCacheConfigPath = BaseDir + "/wasmtime-cache.toml"

	// The controller sets these on replicas that get a compiled cache: the
	// application, runtime image and architecture the cache must have been
	// built for. The entrypoint compares them with the cache metadata and
	// never passes them to Spin.
	CompiledCacheSourceEnv       = "KUBESWIFT_SPIN_CACHE_SOURCE"
	CompiledCacheRuntimeEnv      = "KUBESWIFT_SPIN_CACHE_RUNTIME"
	CompiledCacheArchitectureEnv = "KUBESWIFT_SPIN_CACHE_ARCHITECTURE"

	// WasmtimeCacheFileEnv and DisableWasmtimeCacheEnv are read by Spin.
	// Only the entrypoint sets them; values from anywhere else are dropped.
	WasmtimeCacheFileEnv    = "WASMTIME_CACHE_FILE"
	DisableWasmtimeCacheEnv = "DISABLE_WASMTIME_CACHE"

	// RunAsUID and RunAsGID are the unprivileged identity the entrypoint
	// switches to before exec'ing Spin. They match the distroless "nonroot"
	// user shipped in the runtime image.
	RunAsUID = 65532
	RunAsGID = 65532
)

// CacheMetadata is the metadata.json of a compiled cache artifact. Compile
// mode writes it, the cache signer checks it before signing, and the
// entrypoint checks it before using a mounted cache. It has no timestamps,
// so compiling the same application with the same runtime gives the same
// file.
type CacheMetadata struct {
	// Kind is CompiledCacheKind.
	Kind string `json:"kind"`
	// Source is the application, repository@sha256:...
	Source string `json:"source"`
	// Runtime is the runtime image that compiled it, repository@sha256:...
	Runtime string `json:"runtime"`
	// Architecture is the GOARCH of the compiler: amd64 or arm64.
	Architecture string `json:"architecture"`
	// Spin is the first line of `spin --version`.
	Spin string `json:"spin"`
	// WasmtimeCache is the directory of Wasmtime's cache layout that holds
	// the entries, for example wasmtime-49.0.1.
	WasmtimeCache string `json:"wasmtimeCache"`
	// Entries are the cache entry file names, sorted.
	Entries []string `json:"entries"`
	// Components are the IDs of the compiled components, sorted.
	Components []string `json:"components"`
}

var (
	cacheEntryName = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	wasmtimeDir    = regexp.MustCompile(`^wasmtime-[0-9]+\.[0-9]+\.[0-9]+$`)
)

// ValidCacheEntryName reports whether name is a Wasmtime cache entry: the
// unpadded base64url SHA-256 that Wasmtime uses as the file name.
func ValidCacheEntryName(name string) bool { return cacheEntryName.MatchString(name) }

// ValidWasmtimeCacheDir reports whether name is a version directory of
// Wasmtime's cache layout.
func ValidWasmtimeCacheDir(name string) bool { return wasmtimeDir.MatchString(name) }
