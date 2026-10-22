package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
)

// maxCacheMetadata bounds metadata.json; the signer refuses larger files.
const maxCacheMetadata = 1 << 20

// cacheFS is what the compiled cache decision reads: the mount table, the
// metadata file and the cache directory. Tests replace it.
type cacheFS struct {
	readFile readFileFunc
	lstat    func(string) (os.FileInfo, error)
}

var hostFS = cacheFS{readFile: os.ReadFile, lstat: os.Lstat}

// cacheExpectation is what the controller asked for: the application,
// runtime image and architecture the replica's cache must have been built
// for. All three are set or the replica has no cache.
type cacheExpectation struct {
	source, runtime, architecture string
}

func expectation(vars map[string]string) (cacheExpectation, bool) {
	e := cacheExpectation{
		source:       vars[runtimecontract.CompiledCacheSourceEnv],
		runtime:      vars[runtimecontract.CompiledCacheRuntimeEnv],
		architecture: vars[runtimecontract.CompiledCacheArchitectureEnv],
	}
	return e, e.source != "" && e.runtime != "" && e.architecture != ""
}

// compiledCacheConfig decides whether Spin uses the compiled cache mounted
// at dir (ADR 0014). It returns the Wasmtime cache configuration to write,
// or nil and the reason the cache is not used. The cache is used only when
// dir is a read-only mount point and its metadata names exactly the
// expected application, runtime image and architecture, and this image's
// Wasmtime cache version. Anything else starts Spin with its normal cache:
// the decision never fails the replica.
func compiledCacheConfig(fsys cacheFS, dir string, want cacheExpectation) ([]byte, string) {
	ro, mounted, err := mountState(fsys.readFile, dir)
	switch {
	case err != nil:
		return nil, "the mount table is unreadable"
	case !mounted:
		return nil, "no cache is mounted"
	case !ro:
		return nil, "the cache mount is writable"
	}
	raw, err := readBounded(fsys.readFile, filepath.Join(dir, runtimecontract.CompiledCacheMetadataFile), maxCacheMetadata)
	if err != nil {
		return nil, "the cache metadata is missing or too large"
	}
	var md runtimecontract.CacheMetadata
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&md); err != nil || dec.More() {
		return nil, "the cache metadata is not compiled cache metadata"
	}
	switch {
	case md.Kind != runtimecontract.CompiledCacheKind:
		return nil, "the cache metadata has another kind"
	case md.Source != want.source:
		return nil, "the cache was built for another application"
	case md.Runtime != want.runtime:
		return nil, "the cache was built by another runtime image"
	case md.Architecture != want.architecture || md.Architecture != runtime.GOARCH:
		return nil, "the cache was built for another architecture"
	case md.WasmtimeCache != runtimecontract.WasmtimeCacheVersion:
		return nil, "the cache has another Wasmtime cache version"
	case len(md.Entries) == 0:
		return nil, "the cache has no entries"
	}
	entries := filepath.Join(dir, runtimecontract.CompiledCacheEntries)
	if fi, err := fsys.lstat(filepath.Join(entries, "modules", md.WasmtimeCache)); err != nil || !fi.IsDir() {
		return nil, "the cache has no entry directory"
	}
	// Wasmtime 49 accepts only the [cache] table and refuses unknown keys.
	// A read-only directory serves hits; its statistics and cleanup writes
	// fail without effect, and misses compile as usual.
	return []byte(fmt.Sprintf("[cache]\ndirectory = %q\n", entries)), ""
}

// mountState reports whether dir is a mount point, and whether its topmost
// mount is read-only, from /proc/self/mountinfo. A directory that exists
// but is not a mount point is not a delivered cache: KubeSwift mounts
// nothing when it skips an optional artifact.
func mountState(readFile readFileFunc, dir string) (readOnly, mounted bool, err error) {
	raw, err := readFile("/proc/self/mountinfo")
	if err != nil {
		return false, false, err
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		// id parent major:minor root mountpoint options [optional...] - fstype source super-options
		f := strings.Fields(sc.Text())
		if len(f) < 6 || unescapeMount(f[4]) != dir {
			continue
		}
		mounted = true
		readOnly = false
		for _, o := range strings.Split(f[5], ",") {
			if o == "ro" {
				readOnly = true
			}
		}
	}
	return readOnly, mounted, sc.Err()
}

// unescapeMount undoes mountinfo's octal escapes of space, tab, newline
// and backslash.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}
