package signer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
	"github.com/kubeswift-io/kubeswift-spin/internal/compiledcache"
	"github.com/kubeswift-io/kubeswift-spin/internal/metrics"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift-spin/internal/sandboxapi"
)

// Reconciler signs the exported artifacts of SpinCompiledCache records.
type Reconciler struct {
	// Client reads and patches records (the signer's own namespace only).
	Client client.Client
	// APIReader reads compiler sandboxes directly from the API server; the
	// signer keeps no sandbox cache and needs only get.
	APIReader client.Reader
	Settings  compiledcache.Settings
	Fetcher   *Fetcher
	Signer    Signer
	// Identity is the signer's identity and version, the compiler claim.
	Identity string
	// NameOptions are passed when parsing references (tests: insecure).
	NameOptions []name.Option
	Now         func() time.Time
	// RegistryTimeout bounds the registry calls of one signing decision;
	// 2 minutes when zero.
	RegistryTimeout time.Duration

	keyMu sync.Mutex
	keyID string
}

// SetupWithManager registers the reconciler.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("compiledcache-signer").
		For(&cachev1alpha1.SpinCompiledCache{}).
		Complete(r)
}

// signatureKeys are the JSON fields of SignatureStatus.
var signatureKeys = []string{"artifact", "sandboxUID", "keyID", "signer", "signedAt", "rejected", "failures", "lastFailureTime", "reason", "message"}

// backoff is the wait after failures failed attempts: 30 seconds doubling
// to an hour.
func backoff(failures int32) time.Duration {
	d := 30 * time.Second
	for i := int32(1); i < failures && d < time.Hour; i++ {
		d *= 2
	}
	return min(d, time.Hour)
}

// Reconcile signs one record's artifact if it needs it.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rec cachev1alpha1.SpinCompiledCache
	if err := r.Client.Get(ctx, req.NamespacedName, &rec); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	c := rec.Status.Compile
	if !rec.DeletionTimestamp.IsZero() || c == nil || c.Artifact == "" || c.Sandbox == nil {
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx).WithValues("record", rec.Name, "namespace", rec.Spec.Namespace, "artifact", c.Artifact)
	ctx = log.IntoContext(ctx, logger)
	now := r.Now()

	// A decision is about one compiler's output: a later compiler that
	// exported the same bytes is judged again, unless they are already
	// signed.
	next := cachev1alpha1.SignatureStatus{Artifact: c.Artifact, SandboxUID: c.Sandbox.UID}
	if s := rec.Status.Signature; s != nil && s.Artifact == c.Artifact {
		if s.SignedAt != nil {
			return ctrl.Result{}, nil
		}
		if s.SandboxUID == c.Sandbox.UID {
			if s.Rejected {
				return ctrl.Result{}, nil
			}
			if s.LastFailureTime != nil {
				if wait := s.LastFailureTime.Add(backoff(s.Failures)).Sub(now); wait > 0 {
					return ctrl.Result{RequeueAfter: wait}, nil
				}
			}
			next.Failures = s.Failures
		}
	}

	requeue := time.Duration(0)
	err := r.sign(ctx, &rec)
	switch e, isInvalid := IsInvalid(err); {
	case err == nil:
		keyID, kerr := r.key(ctx)
		if kerr != nil {
			logger.Error(kerr, "signed, but cannot read the signing key's public key")
		}
		next.SignedAt, next.KeyID, next.Signer, next.Failures = &metav1.Time{Time: now}, keyID, r.Identity, 0
		metrics.CompiledCacheSignatures.WithLabelValues("signed").Inc()
		logger.Info("signed compiled cache")
	case isInvalid:
		next.Rejected, next.Reason, next.Message = true, e.Reason, e.Msg
		metrics.CompiledCacheSignatures.WithLabelValues("rejected").Inc()
		logger.Info("refused to sign compiled cache", "reason", e.Reason, "detail", e.Msg)
	default:
		next.Failures++
		next.LastFailureTime = &metav1.Time{Time: now}
		next.Reason, next.Message = "SignFailed", bounded(err.Error())
		requeue = backoff(next.Failures)
		metrics.CompiledCacheSignatures.WithLabelValues("failed").Inc()
		logger.Error(err, "signing failed; retrying", "retryIn", requeue)
	}
	if err := r.patch(ctx, &rec, &next); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// sign checks the compiler and the artifact and signs it. An InvalidError
// means it must never be signed.
func (r *Reconciler) sign(ctx context.Context, rec *cachev1alpha1.SpinCompiledCache) error {
	c := rec.Status.Compile
	if err := r.Settings.CheckRecord(rec); err != nil {
		return invalid("RecordInvalid", "%v", err)
	}
	// Registry calls share one deadline, so a stalled registry cannot hold
	// the signer.
	ctx, cancel := context.WithTimeout(ctx, r.registryTimeout())
	defer cancel()
	// The sandbox is read as stored, every field included, so a field
	// kubeswift-spin's own types do not declare cannot hide from the check.
	var obj unstructured.Unstructured
	obj.SetGroupVersionKind(sandboxv1alpha1.GroupVersion.WithKind("SwiftSandbox"))
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: rec.Spec.Namespace, Name: c.Sandbox.Name}, &obj); err != nil {
		if apierrors.IsNotFound(err) {
			return invalid("CompilerGone", "compiler sandbox %s no longer exists, so its output cannot be checked", c.Sandbox.Name)
		}
		return err
	}
	if err := compiledcache.CheckCompiler(rec, &obj, r.Settings.Runtimes); err != nil {
		return invalid("CompilerCheckFailed", "%v", err)
	}
	var sb sandboxv1alpha1.SwiftSandbox
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &sb); err != nil {
		return invalid("CompilerCheckFailed", "decode compiler sandbox %s: %v", c.Sandbox.Name, err)
	}
	if sb.Status.Phase != sandboxv1alpha1.SwiftSandboxCompleted {
		return invalid("CompilerCheckFailed", "compiler sandbox %s is %s, not Completed", sb.Name, sb.Status.Phase)
	}
	digest, err := r.Fetcher.ResolveExportTag(ctx, rec.Spec.Repository, compiledcache.ExportTag(&sb), r.NameOptions...)
	if err != nil {
		var terr *transport.Error
		if errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound {
			return invalid("ExportTagMissing", "the registry has no export tag %s for the compiler sandbox", compiledcache.ExportTag(&sb))
		}
		return err
	}
	if want := rec.Spec.Repository + "@" + digest; want != c.Artifact {
		return invalid("DigestMismatch", "the compiler's export tag names %s, the record names %s", want, c.Artifact)
	}
	if _, err := r.Fetcher.Check(ctx, rec, c.Artifact, r.NameOptions...); err != nil {
		return err
	}
	claims := compiledcache.IdentityOf(rec).Claims(r.Identity)
	if err := compiledcache.ValidClaims(claims); err != nil {
		return invalid("RecordInvalid", "%v", err)
	}
	return r.Signer.Sign(ctx, c.Artifact, claims)
}

func (r *Reconciler) key(ctx context.Context) (string, error) {
	r.keyMu.Lock()
	defer r.keyMu.Unlock()
	if r.keyID != "" {
		return r.keyID, nil
	}
	id, err := r.Signer.KeyID(ctx)
	if err == nil {
		r.keyID = id
	}
	return id, err
}

// patch writes status.signature, the only field the signer writes, with the
// record's resourceVersion.
func (r *Reconciler) patch(ctx context.Context, rec *cachev1alpha1.SpinCompiledCache, s *cachev1alpha1.SignatureStatus) error {
	obj, err := compiledcache.ReplaceObject(s, signatureKeys)
	if err != nil {
		return err
	}
	b, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": rec.ResourceVersion},
		"status":   map[string]any{"signature": obj},
	})
	if err != nil {
		return err
	}
	if err := r.Client.Status().Patch(ctx, rec, client.RawPatch(types.MergePatchType, b)); err != nil {
		return fmt.Errorf("patch SpinCompiledCache %s signature: %w", rec.Name, err)
	}
	return nil
}

func (r *Reconciler) registryTimeout() time.Duration {
	if r.RegistryTimeout > 0 {
		return r.RegistryTimeout
	}
	return 2 * time.Minute
}
