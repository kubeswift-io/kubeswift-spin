// Package runtimecontract defines the interface between the controller, which
// builds the SwiftSandbox command line and environment, and the entrypoint in
// the kubeswift-spin runtime image, which prepares the guest and execs Spin.
//
// Both sides import this package so the paths and variable names cannot drift.
// Changing any value here changes the sandbox spec of every SpinApp and
// therefore rolls every instance, so treat it as a versioned contract.
package runtimecontract

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

	// RunAsUID and RunAsGID are the unprivileged identity the entrypoint
	// switches to before exec'ing Spin. They match the distroless "nonroot"
	// user shipped in the runtime image.
	RunAsUID = 65532
	RunAsGID = 65532
)
