package main

// Compile mode (ADR 0014): compile a Spin application into a Wasmtime cache
// inside an isolated compiler sandbox, without serving it.
//
// The entrypoint stays root only to prepare directories and to write the
// output; `spin registry pull` and `spin up` run as the unprivileged user
// with no_new_privs. `spin up` listens on a loopback address no one else can
// reach. Spin compiles every component before it opens its listener, so a
// TCP connection that the listener accepts proves the cache is complete; no
// request is ever sent to the application.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"

	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
)

// Compile mode limits.
const (
	compilePullTimeout  = 3 * time.Minute
	compileStartTimeout = 5 * time.Minute
	compileStopTimeout  = 10 * time.Second
	maxLockedApp        = 4 << 20
	maxCacheEntry       = 256 << 20
	maxCacheEntries     = 4096
	maxComponents       = 256
	// compilePlaceholder is the value of a required Spin variable that has no
	// value in the compiler sandbox (for example one backed by a Secret,
	// which the compiler never receives). Variables are not part of the
	// compiled code; a value is needed only because `spin up` refuses to
	// start without one. It is also a valid host name, for variables used in
	// allowed_outbound_hosts templates.
	compilePlaceholder = "kubeswift-spin-compile-placeholder"
)

type compileOpts struct {
	source, runtime, architecture string
}

var (
	pinnedRef   = regexp.MustCompile(`^[^@\s]+@sha256:[a-f0-9]{64}$`)
	spinVarName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	storeLabel  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	componentID = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// parseCompileArgs parses `compile --source=... --runtime=...
// --architecture=...`. Every flag is required, once.
func parseCompileArgs(args []string) (compileOpts, error) {
	var o compileOpts
	seen := map[string]bool{}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok || seen[k] {
			return o, fmt.Errorf("compile: unexpected argument %q", bounded(a))
		}
		seen[k] = true
		switch k {
		case "--source":
			o.source = v
		case "--runtime":
			o.runtime = v
		case "--architecture":
			o.architecture = v
		default:
			return o, fmt.Errorf("compile: unknown flag %q", bounded(k))
		}
	}
	switch {
	case !pinnedRef.MatchString(o.source):
		return o, fmt.Errorf("compile: --source must be repository@sha256:<digest>")
	case !pinnedRef.MatchString(o.runtime):
		return o, fmt.Errorf("compile: --runtime must be repository@sha256:<digest>")
	case o.architecture != "amd64" && o.architecture != "arm64":
		return o, fmt.Errorf("compile: --architecture must be amd64 or arm64")
	}
	return o, nil
}

func bounded(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}

// lockedApp is the part of Spin's locked application that compile mode
// reads.
type lockedApp struct {
	Variables map[string]struct {
		Default *string `json:"default"`
	} `json:"variables"`
	Triggers []struct {
		TriggerType   string          `json:"trigger_type"`
		TriggerConfig json.RawMessage `json:"trigger_config"`
	} `json:"triggers"`
	Components []struct {
		ID       string                     `json:"id"`
		Metadata map[string]json.RawMessage `json:"metadata"`
	} `json:"components"`
}

// compilePlan is what `spin up` needs to start the application without
// serving it: the components to load, placeholder values for required
// variables, and runtime configuration for every store label.
type compilePlan struct {
	components    []string // sorted IDs of HTTP trigger components
	filter        bool     // pass --component-id for each (the app has other triggers)
	placeholders  []string // SPIN_VARIABLE_* names that get compilePlaceholder
	runtimeConfig []byte
}

// planCompile reads the locked application. env holds the sandbox
// environment, so variables the controller passed keep their values.
func planCompile(raw []byte, env map[string]string, stateDir string) (*compilePlan, error) {
	var app lockedApp
	if err := json.Unmarshal(raw, &app); err != nil {
		return nil, errors.New("the application's locked configuration is not valid JSON")
	}
	p := &compilePlan{}
	httpComponents := map[string]bool{}
	other := false
	for _, t := range app.Triggers {
		if t.TriggerType != "http" {
			other = true
			continue
		}
		var cfg struct {
			Component json.RawMessage `json:"component"`
		}
		if err := json.Unmarshal(t.TriggerConfig, &cfg); err != nil {
			return nil, errors.New("an HTTP trigger has an unreadable configuration")
		}
		var id string
		if err := json.Unmarshal(cfg.Component, &id); err != nil || id == "" {
			return nil, errors.New("an HTTP trigger does not name its component")
		}
		if len(id) > 128 || !componentID.MatchString(id) {
			return nil, fmt.Errorf("component ID %q is not a Spin component ID", bounded(id))
		}
		httpComponents[id] = true
		if len(httpComponents) > maxComponents {
			return nil, fmt.Errorf("more than %d HTTP components", maxComponents)
		}
	}
	if len(httpComponents) == 0 {
		return nil, errors.New("the application has no HTTP trigger; compiled caches are built for HTTP applications only")
	}
	for id := range httpComponents {
		p.components = append(p.components, id)
	}
	sort.Strings(p.components)
	p.filter = other

	for name, v := range app.Variables {
		if !spinVarName.MatchString(name) {
			return nil, fmt.Errorf("variable %q has an unexpected name", bounded(name))
		}
		if v.Default != nil {
			continue
		}
		envName := "SPIN_VARIABLE_" + strings.ToUpper(name)
		if _, ok := env[envName]; !ok {
			p.placeholders = append(p.placeholders, envName)
		}
	}
	sort.Strings(p.placeholders)

	kv, db := map[string]bool{}, map[string]bool{}
	for _, c := range app.Components {
		if !httpComponents[c.ID] {
			continue
		}
		for key, dst := range map[string]map[string]bool{"key_value_stores": kv, "databases": db} {
			raw, ok := c.Metadata[key]
			if !ok {
				continue
			}
			var labels []string
			if err := json.Unmarshal(raw, &labels); err != nil {
				return nil, fmt.Errorf("component %q: %s is not a list of labels", bounded(c.ID), key)
			}
			for _, l := range labels {
				if !storeLabel.MatchString(l) {
					return nil, fmt.Errorf("component %q: unexpected store label %q", bounded(c.ID), bounded(l))
				}
				if l != "default" {
					dst[l] = true
				}
			}
		}
	}
	doc := map[string]any{}
	add := func(table string, labels map[string]bool, prefix string) {
		if len(labels) == 0 {
			return
		}
		t := map[string]any{}
		names := make([]string, 0, len(labels))
		for l := range labels {
			names = append(names, l)
		}
		sort.Strings(names)
		for i, l := range names {
			t[l] = map[string]string{"type": "spin", "path": fmt.Sprintf("%s/%s-%d.db", stateDir, prefix, i)}
		}
		doc[table] = t
	}
	add("key_value_store", kv, "kv")
	add("sqlite_database", db, "sqlite")
	b, err := toml.Marshal(doc)
	if err != nil {
		return nil, errors.New("encode the compile runtime configuration")
	}
	p.runtimeConfig = b
	return p, nil
}

// findLockedApp returns the locked application that `spin registry pull`
// stored under dir. The cache directory is fresh, so it holds the one
// application pulled; if Spin stored it under several names, all copies must
// be identical.
func findLockedApp(dir string) ([]byte, error) {
	var found []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "config.json" {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the pulled application: %w", err)
	}
	if len(found) == 0 {
		return nil, errors.New("spin registry pull stored no locked application")
	}
	sort.Strings(found)
	var first []byte
	for i, f := range found {
		b, err := readLimited(f, maxLockedApp)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			first = b
		} else if string(b) != string(first) {
			return nil, errors.New("the registry cache holds more than one locked application")
		}
	}
	return first, nil
}

// readLimited reads a file the Spin user wrote. It never follows a
// symbolic link, and requires a regular file with one link (a hard link to a
// file the Spin user cannot read must not be copied by root), owned by the
// Spin user when compile mode runs as root.
func readLimited(path string, limit int64) ([]byte, error) {
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer func() { _ = f.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || (os.Geteuid() == 0 && st.Uid != runtimecontract.RunAsUID) {
		return nil, fmt.Errorf("%s is not a regular file of the Spin user with a single link", filepath.Base(path))
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), limit)
	}
	return b, nil
}

// collectEntries returns the Wasmtime version directory and the cache entry
// names under cacheDir/modules. Statistics, lock and temporary files are left
// out; anything else that is not a regular file fails.
func collectEntries(cacheDir string) (string, []string, error) {
	modules := filepath.Join(cacheDir, "modules")
	dirs, err := os.ReadDir(modules)
	if err != nil {
		return "", nil, fmt.Errorf("spin wrote no Wasmtime cache: %w", err)
	}
	var version string
	for _, d := range dirs {
		if !d.IsDir() || !runtimecontract.ValidWasmtimeCacheDir(d.Name()) {
			continue
		}
		if version != "" {
			return "", nil, errors.New("the Wasmtime cache has more than one version directory")
		}
		version = d.Name()
	}
	if version == "" {
		return "", nil, errors.New("the Wasmtime cache has no version directory")
	}
	files, err := os.ReadDir(filepath.Join(modules, version))
	if err != nil {
		return "", nil, err
	}
	var entries []string
	for _, f := range files {
		if !runtimecontract.ValidCacheEntryName(f.Name()) {
			continue // .stats, .cleanup.wip-*, temporary files
		}
		if !f.Type().IsRegular() {
			return "", nil, fmt.Errorf("cache entry %s is not a regular file", f.Name())
		}
		entries = append(entries, f.Name())
	}
	if len(entries) == 0 {
		return "", nil, errors.New("spin compiled no component into the Wasmtime cache")
	}
	if len(entries) > maxCacheEntries {
		return "", nil, fmt.Errorf("more than %d cache entries", maxCacheEntries)
	}
	sort.Strings(entries)
	return version, entries, nil
}

// writeOutput copies the entries into out and writes the metadata.
func writeOutput(out, cacheDir string, meta runtimecontract.CacheMetadata) error {
	dst := filepath.Join(out, runtimecontract.CompiledCacheEntries, "modules", meta.WasmtimeCache)
	if err := os.MkdirAll(dst, 0o755); err != nil { //nolint:gosec // exported as a read-only artifact
		return err
	}
	for _, e := range meta.Entries {
		b, err := readLimited(filepath.Join(cacheDir, "modules", meta.WasmtimeCache, e), maxCacheEntry)
		if err != nil {
			return fmt.Errorf("read cache entry %s: %w", e, err)
		}
		if err := os.WriteFile(filepath.Join(dst, e), b, 0o644); err != nil { //nolint:gosec // exported as a read-only artifact
			return fmt.Errorf("write cache entry %s: %w", e, err)
		}
	}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, runtimecontract.CompiledCacheMetadataFile), append(b, '\n'), 0o644) //nolint:gosec // exported as a read-only artifact
}

// checkEmptyDir requires an existing, empty directory.
func checkEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("the output directory %s is missing (is spec.outputs set?): %w", dir, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("the output directory %s is not empty", dir)
	}
	return nil
}

// runCompile is compile mode. It returns an error for any failure; the
// compiler sandbox then exits non-zero and KubeSwift exports nothing.
func runCompile(args, environ []string) error {
	o, err := parseCompileArgs(args)
	if err != nil {
		return err
	}
	if runtime.GOARCH != o.architecture {
		return fmt.Errorf("compile: this runtime is %s, the cache is requested for %s", runtime.GOARCH, o.architecture)
	}
	if os.Geteuid() != 0 {
		return errors.New("compile: the entrypoint must start as root to prepare the guest")
	}
	if err := checkEmptyDir(runtimecontract.CompileOutputDir); err != nil {
		return err
	}

	// The registry credentials and environment are handled as in `up` mode.
	p, err := buildPlan([]string{"up"}, environ, os.ReadFile)
	if err != nil {
		return err
	}
	// Directories the Spin user writes are its own; the directories that
	// hold them, and every file root writes after Spin ran, are root's, so
	// code running as the Spin user cannot redirect root's writes.
	work := runtimecontract.CompileWorkDir
	registry, wasmtime, state := work+"/registry", work+"/wasmtime", work+"/state"
	for _, d := range []struct {
		path  string
		owned bool
	}{
		{runtimecontract.BaseDir, false}, {work, false},
		{runtimecontract.HomeDir, true}, {runtimecontract.TmpDir, true},
		{registry, true}, {wasmtime, true}, {state, true},
	} {
		mode := os.FileMode(0o755)
		if d.owned {
			mode = 0o700
		}
		if err := os.MkdirAll(d.path, mode); err != nil {
			return err
		}
		if err := os.Chmod(d.path, mode); err != nil {
			return err
		}
		if d.owned {
			if err := chown(d.path, true); err != nil {
				return err
			}
		}
	}
	if p.dockerConfig != nil {
		if err := os.MkdirAll(runtimecontract.HomeDir+"/.docker", 0o700); err != nil {
			return err
		}
		if err := chown(runtimecontract.HomeDir+"/.docker", true); err != nil {
			return err
		}
		if err := writePrivate(runtimecontract.HomeDir+"/.docker/config.json", p.dockerConfig, true); err != nil {
			return fmt.Errorf("write registry credentials: %w", err)
		}
	}
	cacheConfig := work + "/wasmtime-cache.toml"
	if err := writeRootFile(cacheConfig, []byte(fmt.Sprintf("[cache]\ndirectory = %q\n", wasmtime))); err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}

	env := map[string]string{}
	for _, kv := range p.env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	// Spin must use exactly the cache this mode collects.
	delete(env, "DISABLE_WASMTIME_CACHE")
	env["WASMTIME_CACHE_FILE"] = cacheConfig

	spinVer := spinVersion(env)
	fmt.Fprintf(os.Stderr, "kubeswift-spin-entrypoint: compile: pulling %s\n", o.source)
	pull := spinCommand(env, "registry", "pull", "--cache-dir="+registry, o.source)
	if err := runBounded(pull, compilePullTimeout); err != nil {
		return fmt.Errorf("compile: spin registry pull: %w", err)
	}
	if err := reapSpinUser(); err != nil {
		return err
	}
	raw, err := findLockedApp(registry)
	if err != nil {
		return fmt.Errorf("compile: %w", err)
	}
	plan, err := planCompile(raw, env, state)
	if err != nil {
		return fmt.Errorf("compile: %w", err)
	}
	for _, name := range plan.placeholders {
		env[name] = compilePlaceholder
	}
	rc := work + "/runtime-config.toml"
	if err := writeRootFile(rc, plan.runtimeConfig); err != nil {
		return err
	}
	upArgs := []string{"up", "--from=" + o.source, "--cache-dir=" + registry,
		"--listen=" + runtimecontract.CompileListenAddress, "--runtime-config-file=" + rc}
	if plan.filter {
		for _, c := range plan.components {
			upArgs = append(upArgs, "--component-id="+c)
		}
	}
	fmt.Fprintf(os.Stderr, "kubeswift-spin-entrypoint: compile: compiling %d components (%d placeholder variables)\n",
		len(plan.components), len(plan.placeholders))
	if err := compileUntilListening(spinCommand(env, upArgs...)); err != nil {
		return fmt.Errorf("compile: %w", err)
	}
	// No process of the Spin user may run while root reads what it wrote.
	if err := reapSpinUser(); err != nil {
		return err
	}

	version, entries, err := collectEntries(wasmtime)
	if err != nil {
		return fmt.Errorf("compile: %w", err)
	}
	meta := runtimecontract.CacheMetadata{
		Kind:          runtimecontract.CompiledCacheKind,
		Source:        o.source,
		Runtime:       o.runtime,
		Architecture:  o.architecture,
		Spin:          spinVer,
		WasmtimeCache: version,
		Entries:       entries,
		Components:    plan.components,
	}
	if err := writeOutput(runtimecontract.CompileOutputDir, wasmtime, meta); err != nil {
		return fmt.Errorf("compile: write the output: %w", err)
	}
	fmt.Fprintf(os.Stderr, "kubeswift-spin-entrypoint: compile: wrote %d cache entries (%s) for %d components\n",
		len(entries), version, len(plan.components))
	return nil
}

// writeRootFile creates a file root owns, in a directory root owns, mode
// 0644 (Spin reads it), failing if anything is already there.
func writeRootFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o644) //nolint:gosec // compile-time path
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// reapCommand is the hidden subcommand that kills every process of the Spin
// user. It runs as that user, so it can signal nothing else.
const reapCommand = "compile-reap"

// reapSpinUser kills every process of the Spin user, including any that left
// Spin's process group, so none runs while root reads its files.
func reapSpinUser() error {
	cmd := exec.Command("/proc/self/exe", reapCommand)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: runtimecontract.RunAsUID, Gid: runtimecontract.RunAsGID, Groups: []uint32{}},
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("stop the Spin user's processes: %w", err)
	}
	return nil
}

// reap is reapCommand: as a non-root user, kill every other process of that
// user.
func reap() error {
	if os.Geteuid() == 0 {
		return errors.New("refusing to reap as root")
	}
	if err := syscall.Kill(-1, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// spinCommand runs Spin as the unprivileged user in its own process group.
func spinCommand(env map[string]string, args ...string) *exec.Cmd {
	cmd := exec.Command(runtimecontract.SpinPath, args...) //nolint:gosec // fixed binary, validated arguments
	cmd.Env = make([]string, 0, len(env))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	sort.Strings(cmd.Env)
	cmd.Dir = runtimecontract.CompileWorkDir
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:    true,
		Credential: &syscall.Credential{Uid: runtimecontract.RunAsUID, Gid: runtimecontract.RunAsGID, Groups: []uint32{}},
		Pdeathsig:  syscall.SIGKILL,
	}
	return cmd
}

// runBounded runs cmd to completion within d.
func runBounded(cmd *exec.Cmd, d time.Duration) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return fmt.Errorf("did not finish within %s", d)
	}
}

// compileUntilListening starts `spin up` and stops it once its loopback
// listener accepts a connection, which Spin opens only after it compiled
// every component.
func compileUntilListening(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start spin up: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stop := func() error {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
		select {
		case <-done:
			return nil
		case <-time.After(compileStopTimeout):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
			return nil
		}
	}
	deadline := time.Now().Add(compileStartTimeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			return fmt.Errorf("spin up exited before it compiled the application: %w", err)
		default:
		}
		c, err := net.DialTimeout("tcp", runtimecontract.CompileListenAddress, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return stop()
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = stop()
	return fmt.Errorf("spin up did not finish compiling within %s", compileStartTimeout)
}

// spinVersion returns the first line of `spin --version`.
func spinVersion(env map[string]string) string {
	cmd := spinCommand(env, "--version")
	var out strings.Builder
	cmd.Stdout = &out
	if err := runBounded(cmd, 30*time.Second); err != nil {
		return "unknown"
	}
	line, _, _ := strings.Cut(strings.TrimSpace(out.String()), "\n")
	return line
}
