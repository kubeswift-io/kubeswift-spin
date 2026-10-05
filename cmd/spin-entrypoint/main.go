// Command spin-entrypoint is PID 1 of the workload inside a kubeswift-spin
// sandbox guest. It prepares the writable directories Spin needs, writes the
// runtime-config file and registry credentials the controller asked for,
// drops root, and execs Spin.
//
// KubeSwift starts the sandbox workload as root and does not apply the OCI
// image user, so dropping privileges here is what keeps the Spin process
// unprivileged inside the guest. Secret files KubeSwift delivers are owned by
// root with mode 0400; they are read here, before the drop, and the Spin user
// gets its own copies. The entrypoint never logs or echoes their contents.
package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"

	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
)

// maxRuntimeConfig bounds the runtime configuration; maxAuthFile bounds each
// registry credential file.
const (
	maxRuntimeConfig = 64 << 10
	maxAuthFile      = 64 << 10
)

type plan struct {
	argv          []string
	env           []string
	runtimeConfig []byte
	dockerConfig  []byte
}

type readFileFunc func(string) ([]byte, error)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "kubeswift-spin-entrypoint: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	p, err := buildPlan(args, os.Environ(), os.ReadFile)
	if err != nil {
		return err
	}
	if err := prepare(p, os.Geteuid() == 0); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := dropPrivileges(runtimecontract.RunAsUID, runtimecontract.RunAsGID); err != nil {
			return err
		}
	}
	// No process started by Spin may regain privileges through setuid
	// binaries or file capabilities.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	// The binary path is a constant and the arguments are the `spin up`
	// flags the controller built; buildPlan rejects any other command.
	return syscall.Exec(runtimecontract.SpinPath, p.argv, p.env) //nolint:gosec // fixed binary, validated arguments
}

// internalEnv reports whether an environment variable is part of the
// entrypoint contract and must not reach Spin.
func internalEnv(k string) bool {
	switch k {
	case runtimecontract.RuntimeConfigEnv, runtimecontract.RuntimeConfigFileEnv, runtimecontract.RegistryAuthFilesEnv:
		return true
	}
	return strings.HasPrefix(k, runtimecontract.SecretValueEnvPrefix)
}

// buildPlan validates the arguments and computes the Spin argv, environment,
// runtime configuration and registry credentials. It only reads files.
func buildPlan(args, environ []string, readFile readFileFunc) (*plan, error) {
	if len(args) == 0 {
		return nil, errors.New("usage: kubeswift-spin-entrypoint up [spin up flags] | --version")
	}
	switch args[0] {
	case "up", "--version":
	default:
		return nil, fmt.Errorf("unsupported command %q: only `spin up` and `spin --version` are run by this image", args[0])
	}

	p := &plan{argv: append([]string{"spin"}, args...)}

	vars := map[string]string{}
	env := make([]string, 0, len(environ)+6)
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		if internalEnv(k) {
			vars[k] = v
			continue // never passed on to Spin
		}
		switch k {
		case "HOME", "TMPDIR", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "PATH", "DOCKER_CONFIG":
			continue // set below
		}
		env = append(env, kv)
	}
	home := runtimecontract.HomeDir
	env = append(env,
		"HOME="+home,
		"TMPDIR="+runtimecontract.TmpDir,
		"XDG_CACHE_HOME="+home+"/.cache",
		"XDG_CONFIG_HOME="+home+"/.config",
		"XDG_DATA_HOME="+home+"/.local/share",
		"PATH=/usr/local/bin",
	)
	p.env = env

	if err := planRuntimeConfig(p, args, vars, readFile); err != nil {
		return nil, err
	}
	if err := planRegistryAuth(p, vars, readFile); err != nil {
		return nil, err
	}
	return p, nil
}

func planRuntimeConfig(p *plan, args []string, vars map[string]string, readFile readFileFunc) error {
	flag := "--runtime-config-file=" + runtimecontract.RuntimeConfigPath
	wantsConfig := false
	for _, a := range args {
		if a == flag {
			wantsConfig = true
		} else if strings.HasPrefix(a, "--runtime-config-file") {
			return fmt.Errorf("runtime config must be passed as %s", flag)
		}
	}
	encoded, haveInline := vars[runtimecontract.RuntimeConfigEnv]
	file, haveFile := vars[runtimecontract.RuntimeConfigFileEnv]
	switch {
	case haveInline && haveFile:
		return fmt.Errorf("%s and %s are mutually exclusive", runtimecontract.RuntimeConfigEnv, runtimecontract.RuntimeConfigFileEnv)
	case (haveInline || haveFile) && !wantsConfig:
		return fmt.Errorf("a runtime config is set but %s is missing from the arguments", flag)
	case wantsConfig && !haveInline && !haveFile:
		return fmt.Errorf("%s is requested but neither %s nor %s is set", flag, runtimecontract.RuntimeConfigEnv, runtimecontract.RuntimeConfigFileEnv)
	case haveFile:
		if file != runtimecontract.SecretRuntimeConfigPath {
			return fmt.Errorf("%s must be %s", runtimecontract.RuntimeConfigFileEnv, runtimecontract.SecretRuntimeConfigPath)
		}
		doc, err := readBounded(readFile, file, maxRuntimeConfig)
		if err != nil {
			return fmt.Errorf("read the runtime config secret file: %w", err)
		}
		p.runtimeConfig = doc
	case haveInline:
		if len(encoded) > base64.StdEncoding.EncodedLen(maxRuntimeConfig) {
			return fmt.Errorf("%s exceeds %d bytes", runtimecontract.RuntimeConfigEnv, maxRuntimeConfig)
		}
		doc, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return fmt.Errorf("%s is not valid base64", runtimecontract.RuntimeConfigEnv)
		}
		doc, err = substituteSecrets(doc, vars)
		if err != nil {
			return err
		}
		p.runtimeConfig = doc
	}
	return nil
}

// substituteSecrets replaces runtime-config option values of the form
// SecretPlaceholderPrefix+<variable> with the value of that environment
// variable, which KubeSwift filled from a Secret. The document is parsed and
// re-encoded, so any value is quoted correctly.
func substituteSecrets(doc []byte, vars map[string]string) ([]byte, error) {
	if !strings.Contains(string(doc), runtimecontract.SecretPlaceholderPrefix) {
		return doc, nil
	}
	var tree map[string]any
	if err := toml.Unmarshal(doc, &tree); err != nil {
		return nil, errors.New("the runtime config is not valid TOML")
	}
	var walk func(v any) (any, error)
	walk = func(v any) (any, error) {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				nv, err := walk(child)
				if err != nil {
					return nil, err
				}
				t[k] = nv
			}
			return t, nil
		case string:
			name, ok := strings.CutPrefix(t, runtimecontract.SecretPlaceholderPrefix)
			if !ok {
				return t, nil
			}
			if !strings.HasPrefix(name, runtimecontract.SecretValueEnvPrefix) {
				return nil, fmt.Errorf("runtime config placeholder %q does not name a %s variable", name, runtimecontract.SecretValueEnvPrefix)
			}
			value, ok := vars[name]
			if !ok {
				return nil, fmt.Errorf("runtime config needs %s, which is not set", name)
			}
			return value, nil
		default:
			return v, nil
		}
	}
	if _, err := walk(tree); err != nil {
		return nil, err
	}
	out, err := toml.Marshal(tree)
	if err != nil {
		return nil, errors.New("re-encode the runtime config")
	}
	return out, nil
}

// planRegistryAuth merges the Docker config files of the SpinApp's
// imagePullSecrets into one config for the Spin user.
func planRegistryAuth(p *plan, vars map[string]string, readFile readFileFunc) error {
	list, ok := vars[runtimecontract.RegistryAuthFilesEnv]
	if !ok || list == "" {
		return nil
	}
	auths := map[string]json.RawMessage{}
	for _, path := range strings.Split(list, ",") {
		clean := filepath.Clean(path)
		if filepath.Dir(clean) != runtimecontract.RegistryAuthDir || !strings.HasSuffix(clean, ".json") {
			return fmt.Errorf("registry credential file %q is not in %s", path, runtimecontract.RegistryAuthDir)
		}
		raw, err := readBounded(readFile, clean, maxAuthFile)
		if err != nil {
			return fmt.Errorf("read registry credential file %s: %w", clean, err)
		}
		var cfg struct {
			Auths map[string]json.RawMessage `json:"auths"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil || cfg.Auths == nil {
			return fmt.Errorf("registry credential file %s is not a Docker config with an auths section (is the Secret of type kubernetes.io/dockerconfigjson?)", clean)
		}
		for host, a := range cfg.Auths {
			auths[host] = a // later Secrets win, as for imagePullSecrets
		}
	}
	out, err := json.Marshal(map[string]any{"auths": auths})
	if err != nil {
		return errors.New("encode the merged registry credentials")
	}
	p.dockerConfig = out
	return nil
}

func readBounded(readFile readFileFunc, path string, limit int) ([]byte, error) {
	b, err := readFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	return b, nil
}

// prepare creates the writable directories and writes the files the Spin
// user needs. As root it hands them to that user; as any other user it
// requires that the directories are already writable.
func prepare(p *plan, asRoot bool) error {
	dirs := []string{
		runtimecontract.BaseDir,
		runtimecontract.StateDir,
		runtimecontract.HomeDir,
		runtimecontract.TmpDir,
	}
	if p.dockerConfig != nil {
		dirs = append(dirs, runtimecontract.HomeDir+"/.docker")
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := chown(dir, asRoot); err != nil {
			return err
		}
	}
	if p.runtimeConfig != nil {
		if err := writePrivate(runtimecontract.RuntimeConfigPath, p.runtimeConfig, asRoot); err != nil {
			return fmt.Errorf("write runtime config: %w", err)
		}
	}
	if p.dockerConfig != nil {
		if err := writePrivate(runtimecontract.HomeDir+"/.docker/config.json", p.dockerConfig, asRoot); err != nil {
			return fmt.Errorf("write registry credentials: %w", err)
		}
	}
	return nil
}

func writePrivate(path string, data []byte, asRoot bool) error {
	if err := os.WriteFile(path, data, 0o600); err != nil { //nolint:gosec // paths are compile-time constants
		return err
	}
	return chown(path, asRoot)
}

func chown(path string, asRoot bool) error {
	if !asRoot {
		return nil
	}
	if err := os.Chown(path, runtimecontract.RunAsUID, runtimecontract.RunAsGID); err != nil {
		return fmt.Errorf("chown %s: %w", path, err)
	}
	return nil
}

func dropPrivileges(uid, gid int) error {
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("clear supplementary groups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid %d: %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid %d: %w", uid, err)
	}
	if os.Geteuid() != uid || os.Getegid() != gid {
		return fmt.Errorf("privilege drop did not take effect (euid %d, egid %d)", os.Geteuid(), os.Getegid())
	}
	return nil
}
