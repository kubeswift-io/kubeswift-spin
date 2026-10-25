package spinlayout

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	ocilayout "github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// TestGoldenAgainstSpin pushes each example application to a loopback
// registry, writes the OCI layout the way KubeSwift writes it, and compares
// the converter's locked application and files with what the pinned `spin
// up --from` writes for the same digest. It needs the Spin binary
// (KSS_GOLDEN_SPIN) and the examples built (make example-test or make
// examples); `make spinlayout-golden` runs it.
func TestGoldenAgainstSpin(t *testing.T) {
	spin := os.Getenv("KSS_GOLDEN_SPIN")
	if spin == "" {
		t.Skip("KSS_GOLDEN_SPIN is not set; run make spinlayout-golden")
	}
	srv := httptest.NewServer(withLocation(registry.New(registry.Logger(log.New(io.Discard, "", 0)))))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	home := t.TempDir()

	apps := []struct {
		name, manifest string
		env            []string
		// spinFails: Spin 4.2.5 cannot run the pushed artifact; the
		// converter must refuse it too.
		spinFails bool
	}{
		{"hello-http", "../../examples/hello-http/spin.toml", nil, false},
		{"multi-component", "../../examples/multi-component/spin.toml", nil, false},
		{"request-info", "../../examples/request-info/spin.toml", nil, false},
		{"outbound-http", "../../examples/outbound-http/spin.toml", nil, false},
		{"key-value", "../../examples/key-value/spin.toml", nil, false},
		{"serverless-ai", "../../examples/serverless-ai/spin.toml", nil, false},
		{"mcp", "../../examples/experimental/mcp/spin.toml", nil, false},
		{"files", "testdata/files-app/spin.toml", nil, false},
		// Spin 4.2.5 pushes archive layers with a truncated gzip stream and
		// cannot pull them back ("failed to iterate over archive").
		{"files-archive", "testdata/files-app/spin.toml", []string{"SPIN_OCI_ARCHIVE_LAYERS=1"}, true},
	}
	for _, a := range apps {
		t.Run(a.name, func(t *testing.T) {
			ref := push(t, spin, home, host, a.name, a.manifest, a.env)
			digest := ref[strings.LastIndexByte(ref, '@')+1:]
			dir := writeLayout(t, ref)
			if keep := os.Getenv("KSS_GOLDEN_KEEP"); keep != "" {
				_ = exec.Command("cp", "-r", dir, filepath.Join(keep, a.name)).Run()
			}

			out := filepath.Join(t.TempDir(), "app")
			res, err := Prepare(context.Background(), dir, out, digest, ref, DefaultLimits)
			if a.spinFails {
				if err == nil {
					removeTree(out)
					t.Fatal("converter accepted an artifact Spin cannot run")
				}
				if spinUpFails(t, spin, home, ref) {
					return
				}
				t.Fatal("spin up ran an artifact the fixture says it cannot; update the fixture")
			}
			if err != nil {
				t.Fatalf("converter refused: %v", err)
			}
			defer removeTree(out)
			got := normalize(t, res.LockPath)

			want := normalize(t, spinUpLock(t, spin, home, ref))
			if !reflect.DeepEqual(got, want) {
				g, _ := json.MarshalIndent(got, "", " ")
				w, _ := json.MarshalIndent(want, "", " ")
				t.Fatalf("converter differs from spin up\nconverter: %s\nspin up: %s", g, w)
			}
		})
	}
}

// withLocation adds the Location header the distribution specification
// requires on a 201 for a blob or manifest, which the in-memory registry
// leaves out for monolithic uploads and Spin's OCI client needs.
func withLocation(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&locationWriter{ResponseWriter: w, r: r}, r)
	})
}

type locationWriter struct {
	http.ResponseWriter
	r *http.Request
}

func (l *locationWriter) WriteHeader(code int) {
	if code == http.StatusCreated && l.Header().Get("Location") == "" {
		p := l.r.URL.Path
		if d := l.r.URL.Query().Get("digest"); d != "" && strings.Contains(p, "/blobs/uploads") {
			p = p[:strings.Index(p, "/blobs/uploads")] + "/blobs/" + d
		}
		l.Header().Set("Location", p)
	}
	l.ResponseWriter.WriteHeader(code)
}

func push(t *testing.T, spin, home, host, repo, manifest string, env []string) string {
	t.Helper()
	cmd := exec.Command(spin, "registry", "push", "--insecure", "-f", manifest, host+"/"+repo+":golden")
	cmd.Env = append([]string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("spin registry push: %v\n%s", err, out)
	}
	// The registry's answer for the tag, not Spin's output: the test
	// registry's Location header carries no digest.
	r, err := name.ParseReference(host+"/"+repo+":golden", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	d, err := remote.Head(r)
	if err != nil {
		t.Fatalf("resolve pushed tag: %v", err)
	}
	return host + "/" + repo + "@" + d.Digest.String()
}

// writeLayout writes ref as an OCI image layout, as KubeSwift's
// materializer does (layout.go, MaterializeLayout).
func writeLayout(t *testing.T, ref string) string {
	t.Helper()
	r, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := remote.Get(r)
	if err != nil {
		t.Fatal(err)
	}
	img, err := desc.Image()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "layout")
	lp, err := ocilayout.Write(dir, empty.Index)
	if err != nil {
		t.Fatal(err)
	}
	if err := lp.AppendImage(img, ocilayout.WithAnnotations(map[string]string{"org.opencontainers.image.ref.name": r.String()})); err != nil {
		t.Fatal(err)
	}
	return dir
}

// spinUpLock runs the pinned `spin up --from` until it has written its
// locked application, stops it, and returns the lock's path.
func spinUpLock(t *testing.T, spin, home, ref string) string {
	t.Helper()
	tmp := t.TempDir()
	cmd := exec.Command(spin, "up", "--from", ref, "--insecure", "--temp", tmp,
		"--cache-dir", filepath.Join(t.TempDir(), "cache"), "--listen", "127.0.0.1:0")
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "RUST_LOG=" + os.Getenv("RUST_LOG")}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()
	lock := filepath.Join(tmp, "spin.lock")
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if b, err := os.ReadFile(lock); err == nil && json.Valid(b) {
			// The assets below tmp and the Wasm in the cache directory stay
			// after the process stops; normalize reads them by path.
			return lock
		}
	}
	t.Fatalf("spin up wrote no lock:\n%s", logs.String())
	return ""
}

// spinUpFails reports whether `spin up --from ref` exits with an error
// before writing a lock.
func spinUpFails(t *testing.T, spin, home, ref string) bool {
	t.Helper()
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, spin, "up", "--from", ref, "--insecure", "--temp", tmp,
		"--cache-dir", filepath.Join(t.TempDir(), "cache"), "--listen", "127.0.0.1:0")
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	err := cmd.Run()
	_, statErr := os.Stat(filepath.Join(tmp, "spin.lock"))
	return err != nil && ctx.Err() == nil && os.IsNotExist(statErr)
}

// normalize replaces every file:// source with what it points to: a Wasm
// source with the digest of its bytes, a files mount with the digests of
// every file below it by relative path. Everything else is compared as is.
func normalize(t *testing.T, lockPath string) any {
	t.Helper()
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	var lock map[string]any
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatal(err)
	}
	cs, _ := lock["components"].([]any)
	for _, cv := range cs {
		c := cv.(map[string]any)
		if src, ok := c["source"].(map[string]any); ok {
			src["source"] = fileDigest(t, strings.TrimPrefix(src["source"].(string), "file://"))
		}
		if fs, ok := c["files"].([]any); ok {
			for _, fv := range fs {
				f := fv.(map[string]any)
				f["source"] = treeDigests(t, strings.TrimPrefix(f["source"].(string), "file://"))
			}
		}
	}
	return lock
}

func fileDigest(t *testing.T, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func treeDigests(t *testing.T, dir string) map[string]any {
	m := map[string]any{}
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
		if fi.Mode().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			m[rel] = fileDigest(t, p)
		}
		return nil
	})
	return m
}
