// Package spinlayout turns a read-only OCI image layout of a Spin
// application into the resolved locked application that `spin trigger`
// runs, exactly as Spin's own OCI loader and `spin up` would for the same
// artifact (ADR 0015). Every blob it uses is verified by digest and copied,
// while it is hashed, into an output tree that Spin then reads; Spin never
// reads the layout itself. Any doubt is an error with a fixed reason: the
// caller then starts Spin from the registry instead.
//
// The package is pure apart from the two directories it is given; it runs
// in the runtime image's entrypoint.
package spinlayout

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

// Media types, as Spin 4.2.5 defines them (crates/oci/src/client.rs).
const (
	MediaLockedApp   = "application/vnd.fermyon.spin.application.v1+config"
	MediaArchive     = "application/vnd.wasm.content.bundle.v1.tar+gzip"
	mediaWasm        = "application/vnd.wasm.content.layer.v1+wasm"
	mediaWasmWKG     = "application/wasm"
	mediaOCIManifest = "application/vnd.oci.image.manifest.v1+json"

	// OriginScheme prefixes metadata.origin for an application loaded from
	// a registry (crates/oci/src/lib.rs).
	OriginScheme = "vnd.fermyon.origin-oci:"
)

// Limits bound everything read and written.
type Limits struct {
	Manifest int64 // index.json and the manifest
	Lock     int64 // the locked application
	Blob     int64 // any one blob or archive member
	Total    int64 // all bytes written
	Files    int   // files written, archive members included
}

// DefaultLimits are the limits of ADR 0015.
var DefaultLimits = Limits{Manifest: 4 << 20, Lock: 16 << 20, Blob: 512 << 20, Total: 2 << 30, Files: 10000}

// Reason is why a layout was not used. Reasons are fixed strings: they name
// the check, never a value from the layout.
type Reason string

// Reasons.
const (
	ReasonDigest    Reason = "the expected digest is not a sha256 digest"
	ReasonReference Reason = "the application reference is not pinned to the expected digest or has no registry host"
	ReasonLayout    Reason = "the OCI layout is missing or malformed"
	ReasonManifest  Reason = "the manifest is missing, malformed or does not match the digest"
	ReasonLock      Reason = "the locked application is missing, ambiguous or malformed"
	ReasonBareWasm  Reason = "the artifact is a bare Wasm package"
	ReasonTriggers  Reason = "the application uses trigger types other than http"
	ReasonComponent Reason = "a component is not supported by the local runner"
	ReasonBlob      Reason = "a blob is missing, of the wrong kind or does not match its digest"
	ReasonFile      Reason = "a static file is not supported or has an unsafe path"
	ReasonLimit     Reason = "the application exceeds a size or count limit"
	ReasonSpace     Reason = "the guest has too little free memory for the copy"
	ReasonDeadline  Reason = "the preparation took too long"
	ReasonOutput    Reason = "the output could not be written"
)

// Error is a refusal; Reason is safe to log.
type Error struct{ Reason Reason }

func (e *Error) Error() string { return string(e.Reason) }

func refuse(r Reason) error { return &Error{Reason: r} }

// Result is a prepared application.
type Result struct {
	// LockPath is the resolved locked application (SPIN_LOCKED_URL is
	// file:// plus this path).
	LockPath string
	// WorkingDir is SPIN_WORKING_DIR.
	WorkingDir string
}

var (
	digestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	// A component id or path element Spin's is_safe_to_join accepts and
	// that is a single name: no separators, not "." or "..", no NUL.
	nameRE = regexp.MustCompile(`^[^/\x00]+$`)
)

type desc struct {
	mediaType string
	digest    string
	size      int64
}

func parseDesc(v any) (desc, error) {
	m, err := object(v, "mediaType", "digest", "size", "annotations", "urls", "platform", "artifactType", "data")
	if err != nil {
		return desc{}, err
	}
	mt, _, err := str(m, "mediaType")
	if err != nil {
		return desc{}, err
	}
	d, ok, err := str(m, "digest")
	if err != nil || !ok || !digestRE.MatchString(d) {
		return desc{}, errors.New("bad digest")
	}
	size, ok, err := integer(m, "size")
	if err != nil || !ok {
		return desc{}, errors.New("bad size")
	}
	return desc{mediaType: mt, digest: d, size: size}, nil
}

// prep holds one preparation's state.
type prep struct {
	ctx     context.Context
	in      int
	out     *outRoot
	lim     Limits
	written int64
	files   int
	layers  map[string]desc   // manifest layers by digest
	copied  map[string]string // digest -> path below out of a verified copy
	members map[string]string // archive member digest -> path below out
}

// Prepare verifies the OCI image layout at mount for the manifest digest
// want, writes the resolved locked application and the content it uses into
// out (which must not exist), and returns where they are. from is the
// `spin up --from` reference; it must end in "@" + want. On any error the
// output is removed.
func Prepare(ctx context.Context, mount, out, want, from string, lim Limits) (*Result, error) {
	if !digestRE.MatchString(want) {
		return nil, refuse(ReasonDigest)
	}
	if !strings.HasSuffix(from, "@"+want) || !hasRegistryHost(from) {
		return nil, refuse(ReasonReference)
	}
	in, err := openDir(mount)
	if err != nil {
		return nil, refuse(ReasonLayout)
	}
	defer func() { _ = unix.Close(in) }()
	o, err := newOutRoot(out)
	if err != nil {
		return nil, refuse(ReasonOutput)
	}
	p := &prep{ctx: ctx, in: in, out: o, lim: lim, copied: map[string]string{}, members: map[string]string{}}
	res, err := p.run(from, want)
	o.close()
	if err != nil {
		removeTree(out)
		if ctx.Err() != nil {
			return nil, refuse(ReasonDeadline)
		}
		return nil, err
	}
	return res, nil
}

// hasRegistryHost reports whether the first path element names a registry
// (contains "." or ":" or is "localhost"), so that Spin prints the reference
// unchanged in metadata.origin.
func hasRegistryHost(ref string) bool {
	host, _, ok := strings.Cut(ref, "/")
	return ok && (strings.ContainsAny(host, ".:") || host == "localhost")
}

func (p *prep) run(from, want string) (*Result, error) {
	if err := p.checkLayout(want); err != nil {
		return nil, err
	}
	mb, err := p.readBlob(want, p.lim.Manifest)
	if err != nil {
		return nil, refuse(ReasonManifest)
	}
	cfg, layers, err := parseManifest(mb)
	if err != nil {
		return nil, refuse(ReasonManifest)
	}
	p.layers = map[string]desc{}
	var total int64
	var lockDesc *desc
	for i := range layers {
		l := layers[i]
		total += l.size
		if l.mediaType == MediaLockedApp {
			if lockDesc != nil {
				return nil, refuse(ReasonLock)
			}
			lockDesc = &layers[i]
		}
		p.layers[l.digest] = l
	}
	if total > p.lim.Total {
		return nil, refuse(ReasonLimit)
	}
	if err := p.checkSpace(total); err != nil {
		return nil, err
	}
	if lockDesc == nil {
		lockDesc = &cfg
	}
	lb, err := p.readBlob(lockDesc.digest, p.lim.Lock)
	if err != nil || int64(len(lb)) != lockDesc.size {
		return nil, refuse(ReasonLock)
	}
	lockV, err := decodeStrict(lb)
	if err != nil {
		return nil, refuse(ReasonLock)
	}
	lock, err := object(lockV, "spin_lock_version", "must_understand", "metadata", "host_requirements", "variables", "triggers", "components")
	if err != nil {
		return nil, refuse(ReasonLock)
	}
	if _, ok := lock["spin_lock_version"]; !ok {
		return nil, refuse(ReasonBareWasm)
	}
	if v, _, err := integer(lock, "spin_lock_version"); err != nil || v > 1 {
		return nil, refuse(ReasonLock)
	}
	if err := checkTriggers(lock); err != nil {
		return nil, err
	}
	if err := p.unpackArchives(); err != nil {
		return nil, err
	}
	if err := p.convertComponents(lock); err != nil {
		return nil, err
	}
	md, ok := lock["metadata"]
	if !ok {
		md = map[string]any{}
	}
	mdm, err := object(md, "origin")
	if err != nil {
		return nil, refuse(ReasonLock)
	}
	mdm["origin"] = OriginScheme + from
	lock["metadata"] = mdm
	doc, err := encode(lock)
	if err != nil {
		return nil, refuse(ReasonOutput)
	}
	if err := p.write("spin.lock", doc); err != nil {
		return nil, err
	}
	if err := p.out.seal(); err != nil {
		return nil, refuse(ReasonOutput)
	}
	return &Result{LockPath: p.out.path + "/spin.lock", WorkingDir: p.out.path}, nil
}

// checkLayout checks oci-layout and that index.json lists want as an OCI
// image manifest. Annotations are ignored: ref.name may name another
// repository.
func (p *prep) checkLayout(want string) error {
	b, err := readIn(p.in, "oci-layout", 4096)
	if err != nil {
		return refuse(ReasonLayout)
	}
	v, err := decodeStrict(b)
	if err != nil {
		return refuse(ReasonLayout)
	}
	m, err := object(v, "imageLayoutVersion")
	if err != nil || len(m) != 1 || m["imageLayoutVersion"] != "1.0.0" {
		return refuse(ReasonLayout)
	}
	b, err = readIn(p.in, "index.json", p.lim.Manifest)
	if err != nil {
		return refuse(ReasonLayout)
	}
	v, err = decodeStrict(b)
	if err != nil {
		return refuse(ReasonLayout)
	}
	idx, err := object(v, "schemaVersion", "mediaType", "manifests", "annotations")
	if err != nil {
		return refuse(ReasonLayout)
	}
	if sv, ok, err := integer(idx, "schemaVersion"); err != nil || !ok || sv != 2 {
		return refuse(ReasonLayout)
	}
	ms, ok, err := array(idx, "manifests")
	if err != nil || !ok {
		return refuse(ReasonLayout)
	}
	for _, mv := range ms {
		d, err := parseDesc(mv)
		if err != nil {
			return refuse(ReasonLayout)
		}
		if d.digest == want && d.mediaType == mediaOCIManifest {
			return nil
		}
	}
	return refuse(ReasonLayout)
}

// parseManifest returns the config and layer descriptors of a schema-2 OCI
// image manifest.
func parseManifest(b []byte) (desc, []desc, error) {
	v, err := decodeStrict(b)
	if err != nil {
		return desc{}, nil, err
	}
	m, err := object(v, "schemaVersion", "mediaType", "artifactType", "config", "layers", "subject", "annotations")
	if err != nil {
		return desc{}, nil, err
	}
	if sv, ok, err := integer(m, "schemaVersion"); err != nil || !ok || sv != 2 {
		return desc{}, nil, errors.New("schema version")
	}
	if mt, ok, err := str(m, "mediaType"); err != nil || (ok && mt != mediaOCIManifest) {
		return desc{}, nil, errors.New("media type")
	}
	cv, ok := m["config"]
	if !ok {
		return desc{}, nil, errors.New("no config")
	}
	cfg, err := parseDesc(cv)
	if err != nil {
		return desc{}, nil, err
	}
	ls, ok, err := array(m, "layers")
	if err != nil || !ok {
		return desc{}, nil, errors.New("no layers")
	}
	layers := make([]desc, 0, len(ls))
	for _, lv := range ls {
		d, err := parseDesc(lv)
		if err != nil {
			return desc{}, nil, err
		}
		layers = append(layers, d)
	}
	return cfg, layers, nil
}

// checkTriggers requires at least one trigger and only http triggers.
func checkTriggers(lock map[string]any) error {
	ts, ok, err := array(lock, "triggers")
	if err != nil || !ok || len(ts) == 0 {
		return refuse(ReasonTriggers)
	}
	for _, tv := range ts {
		t, err := object(tv, "id", "trigger_type", "trigger_config", "trigger_dependencies")
		if err != nil {
			return refuse(ReasonLock)
		}
		tt, ok, err := str(t, "trigger_type")
		if err != nil || !ok || tt != "http" {
			return refuse(ReasonTriggers)
		}
	}
	return nil
}

// readBlob reads a whole blob (manifests and locks only) and checks its
// digest.
func (p *prep) readBlob(d string, limit int64) ([]byte, error) {
	b, err := readIn(p.in, "blobs/sha256/"+d[len("sha256:"):], limit)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if "sha256:"+hex.EncodeToString(sum[:]) != d {
		return nil, errors.New("digest mismatch")
	}
	return b, nil
}

// copyLayer copies a manifest layer into blobs/<hex> below the output while
// hashing it, once, and returns the copy's path below the output.
func (p *prep) copyLayer(d desc) (string, error) {
	if path, ok := p.copied[d.digest]; ok {
		return path, nil
	}
	if d.size > p.lim.Blob {
		return "", refuse(ReasonLimit)
	}
	f, size, err := openIn(p.in, "blobs/sha256/"+d.digest[len("sha256:"):])
	if err != nil || size != d.size {
		return "", refuse(ReasonBlob)
	}
	defer func() { _ = f.Close() }()
	rel := "blobs/" + d.digest[len("sha256:"):]
	if err := p.copyVerified(f, d.size, d.digest, rel); err != nil {
		return "", err
	}
	p.copied[d.digest] = rel
	return rel, nil
}

// copyVerified streams r (exactly size bytes) into rel while hashing it and
// fails, leaving nothing behind it in use, if the digest differs.
func (p *prep) copyVerified(r io.Reader, size int64, digest, rel string) error {
	if err := p.account(size); err != nil {
		return err
	}
	w, err := p.out.create(rel)
	if err != nil {
		return createError(err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), &ctxReader{ctx: p.ctx, r: io.LimitReader(r, size+1)})
	cerr := w.Close()
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return refuse(ReasonDeadline)
	case err != nil || cerr != nil:
		return refuse(ReasonOutput)
	case n != size || "sha256:"+hex.EncodeToString(h.Sum(nil)) != digest:
		return refuse(ReasonBlob)
	}
	return nil
}

func (p *prep) account(n int64) error {
	p.written += n
	p.files++
	if p.written > p.lim.Total || p.files > p.lim.Files {
		return refuse(ReasonLimit)
	}
	return nil
}

func (p *prep) write(rel string, b []byte) error {
	if err := p.account(int64(len(b))); err != nil {
		return err
	}
	w, err := p.out.create(rel)
	if err != nil {
		return createError(err)
	}
	_, err = w.Write(b)
	if cerr := w.Close(); err != nil || cerr != nil {
		return refuse(ReasonOutput)
	}
	return nil
}

// createError maps a failed create: an existing path or a path through a
// file is a conflict between the application's files, anything else an
// output failure.
func createError(err error) error {
	if errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOTDIR) {
		return refuse(ReasonFile)
	}
	return refuse(ReasonOutput)
}

// checkSpace requires the output file system to have room for every layer.
func (p *prep) checkSpace(total int64) error {
	var st unix.Statfs_t
	if err := unix.Fstatfs(p.out.fd, &st); err != nil {
		return refuse(ReasonOutput)
	}
	if int64(st.Bavail)*st.Bsize < total { //nolint:gosec // block counts fit int64
		return refuse(ReasonSpace)
	}
	return nil
}

// unpackArchives indexes the regular files of every archive layer by the
// digest of their content, as Spin's unpack_archive_layer does, copying each
// into the output while hashing it.
func (p *prep) unpackArchives() error {
	for _, d := range p.layers {
		if d.mediaType != MediaArchive {
			continue
		}
		if d.size > p.lim.Blob {
			return refuse(ReasonLimit)
		}
		f, size, err := openIn(p.in, "blobs/sha256/"+d.digest[len("sha256:"):])
		if err != nil || size != d.size {
			return refuse(ReasonBlob)
		}
		err = p.unpackOne(f, d)
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *prep) unpackOne(f io.Reader, d desc) error {
	layerHash := sha256.New()
	raw := io.TeeReader(&ctxReader{ctx: p.ctx, r: io.LimitReader(f, d.size+1)}, layerHash)
	zr, err := gzip.NewReader(raw)
	if err != nil {
		return refuse(ReasonBlob)
	}
	// A truncated stream is refused: Spin 4.2.5 refuses it too ("failed to
	// iterate over archive"), and the local path must not run what `spin
	// up` cannot.
	tr := tar.NewReader(zr)
	for i := 0; ; i++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return refuse(ReasonBlob)
		}
		if i >= p.lim.Files {
			return refuse(ReasonLimit)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size > p.lim.Blob {
			return refuse(ReasonLimit)
		}
		tmp := fmt.Sprintf("members/%s-%d", d.digest[len("sha256:"):][:16], i)
		mh, err := p.copyMember(tr, h.Size, tmp)
		if err != nil {
			return err
		}
		md := "sha256:" + hex.EncodeToString(mh.Sum(nil))
		if _, dup := p.members[md]; dup {
			continue // the first copy stays; the same content either way
		}
		final := "members/" + md[len("sha256:"):]
		if err := p.out.rename(tmp, final); err != nil {
			return refuse(ReasonOutput)
		}
		p.members[md] = final
	}
	// Drain the decompressed stream, then the rest of the compressed bytes,
	// and verify the layer itself.
	if n, err := io.CopyN(io.Discard, zr, p.lim.Total+1); err != nil && !errors.Is(err, io.EOF) {
		return refuse(ReasonBlob)
	} else if n > p.lim.Total {
		return refuse(ReasonLimit)
	}
	if _, err := io.Copy(io.Discard, raw); err != nil {
		return refuse(ReasonBlob)
	}
	if "sha256:"+hex.EncodeToString(layerHash.Sum(nil)) != d.digest {
		return refuse(ReasonBlob)
	}
	return nil
}

// copyMember copies one archive member while hashing it.
func (p *prep) copyMember(r io.Reader, size int64, rel string) (hash.Hash, error) {
	if err := p.account(size); err != nil {
		return nil, err
	}
	w, err := p.out.create(rel)
	if err != nil {
		return nil, refuse(ReasonOutput)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(r, size))
	cerr := w.Close()
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return nil, refuse(ReasonDeadline)
	case err != nil || cerr != nil || n != size:
		return nil, refuse(ReasonBlob)
	}
	return h, nil
}

// convertComponents rewrites component sources and files as Spin's loader
// does, after checking what the local runner supports.
func (p *prep) convertComponents(lock map[string]any) error {
	cs, ok, err := array(lock, "components")
	if err != nil {
		return refuse(ReasonLock)
	}
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for _, cv := range cs {
		c, err := object(cv, "id", "metadata", "source", "env", "files", "config", "dependencies",
			"dependencies_inherit_configuration", "trigger_dependencies", "host_requirements")
		if err != nil {
			return refuse(ReasonLock)
		}
		id, ok, err := str(c, "id")
		if err != nil || !ok || !nameRE.MatchString(id) || id == "." || id == ".." || seen[id] {
			return refuse(ReasonComponent)
		}
		seen[id] = true
		for _, k := range []string{"dependencies", "trigger_dependencies"} {
			if v, ok := c[k]; ok {
				m, isObj := v.(map[string]any)
				if !isObj || len(m) != 0 {
					return refuse(ReasonComponent)
				}
			}
		}
		if err := p.convertSource(c); err != nil {
			return err
		}
		if err := p.convertFiles(c, id); err != nil {
			return err
		}
	}
	return nil
}

// convertSource replaces the component source wholesale with
// {content_type, source: file://<copy>}; Spin requires the digest and
// ignores inline content when it has one.
func (p *prep) convertSource(c map[string]any) error {
	src, err := object(c["source"], "content_type", "source", "inline", "digest")
	if err != nil {
		return refuse(ReasonComponent)
	}
	ct, ok, err := str(src, "content_type")
	if err != nil || !ok {
		return refuse(ReasonComponent)
	}
	d, ok, err := str(src, "digest")
	if err != nil || !ok || !digestRE.MatchString(d) {
		return refuse(ReasonComponent)
	}
	l, ok := p.layers[d]
	if !ok || (l.mediaType != mediaWasm && l.mediaType != mediaWasmWKG) {
		return refuse(ReasonBlob)
	}
	rel, err := p.copyLayer(l)
	if err != nil {
		return err
	}
	c["source"] = map[string]any{"content_type": ct, "source": "file://" + p.out.path + "/" + rel}
	return nil
}

// convertFiles materializes the component's files under
// assets/<component id>/ and replaces the list with one directory mount at
// "/", as Spin's loader does. Inline content wins over a digest, as in
// Spin.
func (p *prep) convertFiles(c map[string]any, id string) error {
	v, ok := c["files"]
	if !ok {
		return nil
	}
	files, isArr := v.([]any)
	if !isArr {
		return refuse(ReasonFile)
	}
	if len(files) == 0 {
		return nil
	}
	base := "assets/" + id
	for _, fv := range files {
		f, err := object(fv, "path", "source", "inline", "digest")
		if err != nil {
			return refuse(ReasonFile)
		}
		path, ok, err := str(f, "path")
		if err != nil || !ok || !safePath(path) {
			return refuse(ReasonFile)
		}
		dst := base + "/" + path
		if in, ok, err := str(f, "inline"); err != nil {
			return refuse(ReasonFile)
		} else if ok {
			b, err := base64.RawStdEncoding.Strict().DecodeString(in)
			if err != nil {
				return refuse(ReasonFile)
			}
			if err := p.write(dst, b); err != nil {
				return err
			}
			continue
		}
		d, ok, err := str(f, "digest")
		if err != nil || !ok || !digestRE.MatchString(d) {
			return refuse(ReasonFile)
		}
		if err := p.copyFile(d, dst); err != nil {
			return err
		}
	}
	c["files"] = []any{map[string]any{"source": "file://" + p.out.path + "/" + base, "path": "/"}}
	if err := p.out.mkdirAll(base); err != nil {
		return refuse(ReasonOutput)
	}
	return nil
}

// copyFile copies the content with digest d (a non-Wasm, non-lock layer,
// or an archive member) to dst.
func (p *prep) copyFile(d, dst string) error {
	var src string
	if m, ok := p.members[d]; ok {
		src = m
	} else if l, ok := p.layers[d]; ok && l.mediaType != mediaWasm && l.mediaType != mediaWasmWKG && l.mediaType != MediaLockedApp {
		rel, err := p.copyLayer(l)
		if err != nil {
			return err
		}
		src = rel
	} else {
		return refuse(ReasonBlob)
	}
	// The source is a verified copy in the output; read it back by
	// descriptor and write the file.
	f, size, err := openIn(p.out.fd, src)
	if err != nil {
		return refuse(ReasonOutput)
	}
	defer func() { _ = f.Close() }()
	return p.copyVerified(f, size, d, dst)
}

// safePath accepts what Spin's is_safe_to_join accepts and nothing more
// lenient: relative, every element a normal name (no empty element, ".",
// ".." or NUL).
func safePath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || len(p) > 4096 {
		return false
	}
	for _, e := range strings.Split(p, "/") {
		if e == "" || e == "." || e == ".." || strings.ContainsRune(e, 0) {
			return false
		}
	}
	return true
}

// ctxReader stops a copy when the context ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(b)
}
