// Command spin-entrypoint is PID 1 of the workload inside a kubeswift-spin
// sandbox guest. It prepares the writable directories Spin needs, writes the
// runtime-config file the controller rendered, drops root, and execs Spin.
//
// KubeSwift starts the sandbox workload as root and does not apply the OCI
// image user, so dropping privileges here is what keeps the Spin process
// unprivileged inside the guest. The entrypoint never parses or logs the
// contents of the runtime configuration.
package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
)

// maxRuntimeConfig bounds the decoded runtime configuration.
const maxRuntimeConfig = 64 << 10

type plan struct {
	argv          []string
	env           []string
	runtimeConfig []byte
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "kubeswift-spin-entrypoint: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	p, err := buildPlan(args, os.Environ())
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
	return syscall.Exec(runtimecontract.SpinPath, p.argv, p.env)
}

// buildPlan validates the arguments and computes the Spin argv and
// environment. It has no side effects.
func buildPlan(args, environ []string) (*plan, error) {
	if len(args) == 0 {
		return nil, errors.New("usage: kubeswift-spin-entrypoint up [spin up flags] | --version")
	}
	switch args[0] {
	case "up", "--version":
	default:
		return nil, fmt.Errorf("unsupported command %q: only `spin up` and `spin --version` are run by this image", args[0])
	}

	p := &plan{argv: append([]string{"spin"}, args...)}

	var encoded string
	var haveConfig bool
	env := make([]string, 0, len(environ)+6)
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case runtimecontract.RuntimeConfigEnv:
			encoded, haveConfig = v, true
			continue // never passed on to Spin
		case "HOME", "TMPDIR", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "PATH":
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

	flag := "--runtime-config-file=" + runtimecontract.RuntimeConfigPath
	wantsConfig := false
	for _, a := range args {
		if a == flag {
			wantsConfig = true
		} else if strings.HasPrefix(a, "--runtime-config-file") {
			return nil, fmt.Errorf("runtime config must be passed as %s", flag)
		}
	}
	switch {
	case haveConfig && !wantsConfig:
		return nil, fmt.Errorf("%s is set but %s is missing from the arguments", runtimecontract.RuntimeConfigEnv, flag)
	case wantsConfig && !haveConfig:
		return nil, fmt.Errorf("%s is requested but %s is not set", flag, runtimecontract.RuntimeConfigEnv)
	case haveConfig:
		if len(encoded) > base64.StdEncoding.EncodedLen(maxRuntimeConfig) {
			return nil, fmt.Errorf("%s exceeds %d bytes", runtimecontract.RuntimeConfigEnv, maxRuntimeConfig)
		}
		doc, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("%s is not valid base64", runtimecontract.RuntimeConfigEnv)
		}
		p.runtimeConfig = doc
	}
	return p, nil
}

// prepare creates the writable directories and the runtime-config file. As
// root it hands them to the unprivileged user; as any other user it requires
// that the directories are already writable.
func prepare(p *plan, asRoot bool) error {
	uid, gid := runtimecontract.RunAsUID, runtimecontract.RunAsGID
	for _, dir := range []string{
		runtimecontract.BaseDir,
		runtimecontract.StateDir,
		runtimecontract.HomeDir,
		runtimecontract.TmpDir,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if asRoot {
			if err := os.Chown(dir, uid, gid); err != nil {
				return fmt.Errorf("chown %s: %w", dir, err)
			}
		}
	}
	if p.runtimeConfig == nil {
		return nil
	}
	path := runtimecontract.RuntimeConfigPath
	if err := os.WriteFile(path, p.runtimeConfig, 0o600); err != nil {
		return fmt.Errorf("write runtime config: %w", err)
	}
	if asRoot {
		if err := os.Chown(path, uid, gid); err != nil {
			return fmt.Errorf("chown runtime config: %w", err)
		}
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
