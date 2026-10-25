package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
	"github.com/kubeswift-io/kubeswift-spin/internal/spinlayout"
)

// localAppTimeout bounds the verification and copy of a local application.
const localAppTimeout = 5 * time.Second

// useLocalApp starts Spin from the application the controller mounted at dir
// when it asked for one and every check passes (ADR 0015): the plan then runs
// `spin trigger http` on a verified copy below out, and Spin gets no registry
// credentials. Otherwise it logs why and leaves the plan exactly as it was,
// so Spin starts from the registry. It never fails the replica. supported is
// the `spin --version` line the converter reproduces; empty disables it.
func useLocalApp(p *plan, fsys cacheFS, dir, out, supported string) {
	if p.localDigest == "" {
		return
	}
	why := planLocalApp(p, fsys, dir, out, supported)
	if why != "" {
		fmt.Fprintf(os.Stderr, "kubeswift-spin-entrypoint: local application not used: %s\n", why)
		return
	}
	fmt.Fprintf(os.Stderr, "kubeswift-spin-entrypoint: local application used from %s\n", dir)
}

// planLocalApp rewrites the plan and returns "", or returns why not with
// the plan unchanged. Reasons are fixed strings or name a flag; they never
// carry a value.
func planLocalApp(p *plan, fsys cacheFS, dir, out, supported string) string {
	if supported == "" {
		return "this runtime image's Spin is not supported by the local runner"
	}
	trigger, from, why := localTriggerArgv(p.argv)
	if why != "" {
		return why
	}
	ro, mounted, err := mountState(fsys.readFile, dir)
	switch {
	case err != nil:
		return "the mount table is unreadable"
	case !mounted:
		return "no application is mounted"
	case !ro:
		return "the application mount is writable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), localAppTimeout)
	defer cancel()
	res, err := spinlayout.Prepare(ctx, dir, out, p.localDigest, from, spinlayout.DefaultLimits)
	if err != nil {
		var e *spinlayout.Error
		if errors.As(err, &e) {
			return string(e.Reason)
		}
		return "the application could not be prepared"
	}
	// Spin never holds registry credentials on the local path. prepare
	// wrote them for the registry path; remove them, or do not use the
	// local application.
	if err := os.Remove(runtimecontract.HomeDir + "/.docker/config.json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		spinlayout.Remove(out)
		return "the registry credentials could not be removed"
	}
	p.argv = trigger
	p.env = append(p.env,
		"SPIN_LOCKED_URL=file://"+res.LockPath,
		"SPIN_WORKING_DIR="+res.WorkingDir,
	)
	return ""
}

// localTriggerArgv returns the `spin trigger http` command line for a
// `spin up` plan, passing the trigger options as `spin up` passes them for an
// application with only HTTP triggers, and the --from reference; or why the
// plan is not supported (`--component-id` and any other option keep the
// registry path).
func localTriggerArgv(argv []string) (trigger []string, from, why string) {
	if len(argv) < 2 || argv[1] != "up" {
		return nil, "", "not a spin up command"
	}
	trigger = []string{"spin", "trigger", "http"}
	for _, a := range argv[2:] {
		flag, value, _ := strings.Cut(a, "=")
		switch flag {
		case "--from":
			from = value
		case "--listen", "--runtime-config-file":
			trigger = append(trigger, a)
		default:
			return nil, "", fmt.Sprintf("the option %s is not supported by the local runner", flag)
		}
	}
	if from == "" {
		return nil, "", "no --from reference"
	}
	return trigger, from, ""
}
