// Package signer is the kubeswift-spin cache signer (ADR 0014). It is the
// only holder of the signing key. For each SpinCompiledCache whose compiler
// exported an artifact, it checks the compiler sandbox and the artifact
// itself, then signs the artifact's digest with the record's claims.
package signer

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
)

// InvalidError is an artifact or compiler that fails a check. It is never
// signed: the record is marked rejected. Any other error is transient and
// retried.
type InvalidError struct{ Reason, Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func invalid(reason, format string, args ...any) error {
	return &InvalidError{Reason: reason, Msg: fmt.Sprintf(format, args...)}
}

// IsInvalid reports whether err is an InvalidError, and returns it.
func IsInvalid(err error) (*InvalidError, bool) {
	var e *InvalidError
	ok := errors.As(err, &e)
	return e, ok
}

var componentID = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Limits bound what the signer reads from an artifact.
type Limits struct {
	// MaxSize bounds both the compressed layer and the uncompressed files.
	MaxSize int64
	// MaxEntries bounds files and directories.
	MaxEntries int
}

// Fetcher reads artifacts from the registry.
type Fetcher struct {
	Keychain authn.Keychain
	Limits   Limits
	// Options are extra remote options (tests: an insecure registry).
	Options []remote.Option
}

func (f *Fetcher) opts() []remote.Option {
	return append([]remote.Option{remote.WithAuthFromKeychain(f.Keychain)}, f.Options...)
}

// ResolveExportTag returns the digest the export tag of a compiler sandbox
// names. Only KubeSwift's export Job pushes that tag, so its digest binds the
// artifact to the compiler without trusting the sandbox's status.
func (f *Fetcher) ResolveExportTag(repository, tag string, nameOpts ...name.Option) (string, error) {
	ref, err := name.NewTag(repository+":"+tag, nameOpts...)
	if err != nil {
		return "", invalid("ExportTagInvalid", "export tag %s:%s is not a valid reference", repository, tag)
	}
	desc, err := remote.Head(ref, f.opts()...)
	if err != nil {
		return "", fmt.Errorf("resolve export tag %s: %w", ref, err)
	}
	return desc.Digest.String(), nil
}

// manifest is the part of an OCI image manifest the signer checks.
type manifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	MediaType     string `json:"mediaType"`
	ArtifactType  string `json:"artifactType"`
	Config        struct {
		MediaType string `json:"mediaType"`
		Size      int64  `json:"size"`
	} `json:"config"`
	Layers []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"layers"`
}

// Check reads the artifact by digest and checks that it is a compiled cache
// for rec: the manifest KubeSwift's export produces, one gzip tar layer with
// only metadata.json and Wasmtime cache entries, within the limits, and
// metadata that names the record's identity. It returns the metadata.
func (f *Fetcher) Check(rec *cachev1alpha1.SpinCompiledCache, artifact string, nameOpts ...name.Option) (*runtimecontract.CacheMetadata, error) {
	ref, err := name.NewDigest(artifact, nameOpts...)
	if err != nil {
		return nil, invalid("ArtifactInvalid", "%s is not a digest reference", artifact)
	}
	img, err := remote.Image(ref, f.opts()...)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", artifact, err)
	}
	raw, err := img.RawManifest()
	if err != nil {
		return nil, fmt.Errorf("read the manifest of %s: %w", artifact, err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, invalid("ManifestInvalid", "the manifest of %s is not JSON", artifact)
	}
	switch {
	case m.SchemaVersion != 2 || m.MediaType != "application/vnd.oci.image.manifest.v1+json":
		return nil, invalid("ManifestInvalid", "%s is not an OCI image manifest", artifact)
	case m.ArtifactType != compiledcache.ArtifactType:
		return nil, invalid("ManifestInvalid", "%s has artifactType %q, not %q", artifact, m.ArtifactType, compiledcache.ArtifactType)
	case len(m.Layers) != 1:
		return nil, invalid("ManifestInvalid", "%s has %d layers, not 1", artifact, len(m.Layers))
	case m.Layers[0].MediaType != "application/vnd.oci.image.layer.v1.tar+gzip":
		return nil, invalid("ManifestInvalid", "%s has a %s layer, not a gzip tar", artifact, m.Layers[0].MediaType)
	case m.Layers[0].Size <= 0 || m.Layers[0].Size > f.Limits.MaxSize:
		return nil, invalid("ArtifactTooLarge", "%s has a %d-byte layer; the limit is %d", artifact, m.Layers[0].Size, f.Limits.MaxSize)
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("read the layer of %s: %w", artifact, err)
	}
	if len(layers) != 1 {
		return nil, invalid("ManifestInvalid", "%s has %d layers, not 1", artifact, len(layers))
	}
	rc, err := layers[0].Compressed()
	if err != nil {
		return nil, fmt.Errorf("read the layer of %s: %w", artifact, err)
	}
	defer func() { _ = rc.Close() }()
	return f.checkLayer(rec, io.LimitReader(rc, f.Limits.MaxSize+1))
}

// checkLayer checks the layer's files. The compressed stream is verified
// against its digest by the registry client as it is read to the end.
func (f *Fetcher) checkLayer(rec *cachev1alpha1.SpinCompiledCache, r io.Reader) (*runtimecontract.CacheMetadata, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, invalid("LayerInvalid", "the layer is not gzip")
	}
	tr := tar.NewReader(gz)
	var meta []byte
	version := ""
	files := map[string]bool{}
	var total int64
	entries := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, invalid("LayerInvalid", "the layer is not a valid tar archive")
		}
		entries++
		if entries > f.Limits.MaxEntries {
			return nil, invalid("ArtifactTooLarge", "more than %d entries", f.Limits.MaxEntries)
		}
		raw := strings.TrimSuffix(strings.TrimPrefix(h.Name, "./"), "/")
		if raw == "" {
			raw = "."
		}
		p := path.Clean(raw)
		if p != raw || strings.HasPrefix(p, "/") || p == ".." || strings.HasPrefix(p, "../") {
			return nil, invalid("EntryInvalid", "unexpected path %q", bounded(h.Name))
		}
		parts := strings.Split(p, "/")
		switch h.Typeflag {
		case tar.TypeDir:
			ok := p == "." || p == runtimecontract.CompiledCacheEntries || p == runtimecontract.CompiledCacheEntries+"/modules" ||
				(len(parts) == 3 && parts[0] == runtimecontract.CompiledCacheEntries && parts[1] == "modules" && runtimecontract.ValidWasmtimeCacheDir(parts[2]))
			if !ok {
				return nil, invalid("EntryInvalid", "unexpected directory %q", bounded(p))
			}
		case tar.TypeReg:
			total += h.Size
			if h.Size < 0 || total > f.Limits.MaxSize {
				return nil, invalid("ArtifactTooLarge", "the files exceed %d bytes", f.Limits.MaxSize)
			}
			switch {
			case p == runtimecontract.CompiledCacheMetadataFile:
				if meta != nil {
					return nil, invalid("EntryInvalid", "metadata.json appears twice")
				}
				if meta, err = io.ReadAll(io.LimitReader(tr, 1<<20+1)); err != nil || len(meta) > 1<<20 {
					return nil, invalid("MetadataInvalid", "metadata.json is unreadable or larger than 1 MiB")
				}
			case len(parts) == 4 && parts[0] == runtimecontract.CompiledCacheEntries && parts[1] == "modules" &&
				runtimecontract.ValidWasmtimeCacheDir(parts[2]) && runtimecontract.ValidCacheEntryName(parts[3]):
				if version != "" && version != parts[2] {
					return nil, invalid("EntryInvalid", "entries in more than one Wasmtime version directory")
				}
				version = parts[2]
				if files[parts[3]] {
					return nil, invalid("EntryInvalid", "entry %s appears twice", parts[3])
				}
				files[parts[3]] = true
				// Read it, so the layer digest is verified to the end. The
				// tar reader stops at the header's size, which counts
				// against the limit above.
				if _, err := io.CopyN(io.Discard, tr, h.Size); err != nil {
					return nil, fmt.Errorf("read entry %s: %w", parts[3], err)
				}
			default:
				return nil, invalid("EntryInvalid", "unexpected file %q", bounded(p))
			}
		default:
			return nil, invalid("EntryInvalid", "%q is not a regular file or directory", bounded(p))
		}
	}
	// Drain the rest so the digest check covers the whole stream; padding
	// after the archive must be small.
	if n, err := io.Copy(io.Discard, io.LimitReader(gz, 1<<20)); err != nil || n == 1<<20 {
		return nil, invalid("LayerInvalid", "the layer is truncated or has data after the archive")
	}
	if meta == nil {
		return nil, invalid("MetadataInvalid", "the artifact has no metadata.json")
	}
	var md runtimecontract.CacheMetadata
	dec := json.NewDecoder(strings.NewReader(string(meta)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&md); err != nil {
		return nil, invalid("MetadataInvalid", "metadata.json is not compiled cache metadata")
	}
	names := make([]string, 0, len(files))
	for f := range files {
		names = append(names, f)
	}
	slices.Sort(names)
	switch {
	case md.Kind != runtimecontract.CompiledCacheKind:
		return nil, invalid("MetadataMismatch", "metadata kind is %q", bounded(md.Kind))
	case md.Source != rec.Spec.Source || md.Runtime != rec.Spec.Runtime || md.Architecture != rec.Spec.Architecture:
		return nil, invalid("MetadataMismatch", "metadata names another source, runtime or architecture than the record")
	case len(names) == 0:
		return nil, invalid("MetadataMismatch", "the artifact has no cache entries")
	case md.WasmtimeCache != version || !slices.Equal(md.Entries, names):
		return nil, invalid("MetadataMismatch", "metadata does not list the artifact's cache entries")
	case len(md.Components) == 0 || len(md.Components) > 256:
		return nil, invalid("MetadataMismatch", "metadata lists %d compiled components", len(md.Components))
	}
	for _, c := range md.Components {
		if len(c) > 128 || !componentID.MatchString(c) {
			return nil, invalid("MetadataMismatch", "metadata lists component %q, which is not a Spin component ID", bounded(c))
		}
	}
	return &md, nil
}

func bounded(s string) string {
	if len(s) > 96 {
		return s[:96] + "..."
	}
	return s
}
