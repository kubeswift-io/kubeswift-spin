package compiledcache

import (
	"fmt"
	"sort"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cachev1alpha1 "github.com/kubeswift-io/kubeswift-spin/api/v1alpha1"
)

// TypeCompiledCache is the SpinApp condition that reports the compiled
// cache. It is an optimization status: it never affects Available or
// Progressing.
const TypeCompiledCache = "CompiledCache"

// Record condition reasons.
const (
	ReasonPending        = "Pending"
	ReasonExported       = "Exported"
	ReasonNotCompiled    = "NotCompiled"
	ReasonSigned         = "Signed"
	ReasonRejected       = "Rejected"
	ReasonNotTrusted     = "NotTrusted"
	ReasonNotRequired    = "NotRequired"
	ReasonPrepared       = "Prepared"
	ReasonProgressing    = "Progressing"
	ReasonRecordNotFound = "RecordNotFound"
)

// TrustedArtifact returns the signed artifact of a record, or "" when the
// record must not be used: only an artifact the signer signed, which is the
// artifact the current compile attempt exported, is ever mounted.
func TrustedArtifact(rec *cachev1alpha1.SpinCompiledCache) string {
	if rec == nil || rec.Status.Compile == nil || rec.Status.Signature == nil {
		return ""
	}
	c, s := rec.Status.Compile, rec.Status.Signature
	if c.Artifact == "" || s.Artifact != c.Artifact || s.SignedAt == nil || s.Rejected {
		return ""
	}
	if repo, _, ok := strings.Cut(c.Artifact, "@"); !ok || repo != rec.Spec.Repository {
		return ""
	}
	if _, err := PinnedReference(c.Artifact); err != nil {
		return ""
	}
	return c.Artifact
}

func condition(t string, ok bool, reason, msg string) metav1.Condition {
	st := metav1.ConditionFalse
	if ok {
		st = metav1.ConditionTrue
	}
	return metav1.Condition{Type: t, Status: st, Reason: reason, Message: truncate(msg)}
}

func truncate(s string) string {
	const n = 1024
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// RecordConditions computes the conditions of a record from its status.
func RecordConditions(rec *cachev1alpha1.SpinCompiledCache, now time.Time) []metav1.Condition {
	st := rec.Status
	c := st.Compile
	var compiled metav1.Condition
	switch {
	case c != nil && c.Artifact != "":
		compiled = condition(cachev1alpha1.ConditionCompiled, true, ReasonExported, fmt.Sprintf("exported %s", c.Artifact))
	case c != nil && c.Sandbox != nil && c.FinishedAt == nil:
		compiled = condition(cachev1alpha1.ConditionCompiled, false, ReasonCompiling,
			fmt.Sprintf("compiler sandbox %s/%s is running (attempt %d)", rec.Spec.Namespace, c.Sandbox.Name, c.Attempt))
	case c != nil && c.Failures > 0:
		msg := c.Message
		if c.RetryAfter != nil && c.RetryAfter.After(now) {
			msg = fmt.Sprintf("%s; next attempt after %s", msg, c.RetryAfter.UTC().Format(time.RFC3339))
		}
		compiled = condition(cachev1alpha1.ConditionCompiled, false, ReasonCompileFailed, fmt.Sprintf("attempt %d failed (%s): %s", c.Attempt, c.Reason, msg))
	default:
		compiled = condition(cachev1alpha1.ConditionCompiled, false, ReasonPending, "waiting for a compiler sandbox")
	}

	var trusted metav1.Condition
	s := st.Signature
	switch {
	case TrustedArtifact(rec) != "":
		trusted = condition(cachev1alpha1.ConditionTrusted, true, ReasonSigned, fmt.Sprintf("signed %s with key %s", s.Artifact, s.KeyID))
	case c == nil || c.Artifact == "":
		trusted = condition(cachev1alpha1.ConditionTrusted, false, ReasonNotCompiled, "no exported artifact to sign yet")
	case s != nil && s.Artifact == c.Artifact && s.Rejected:
		trusted = condition(cachev1alpha1.ConditionTrusted, false, ReasonRejected, fmt.Sprintf("the signer refused %s (%s): %s", c.Artifact, s.Reason, s.Message))
	case s != nil && s.Artifact == c.Artifact && s.Failures > 0:
		trusted = condition(cachev1alpha1.ConditionTrusted, false, ReasonSignFailed, fmt.Sprintf("signing %s failed %d times (%s): %s", c.Artifact, s.Failures, s.Reason, s.Message))
	default:
		trusted = condition(cachev1alpha1.ConditionTrusted, false, ReasonSigning, fmt.Sprintf("waiting for the cache signer to check and sign %s", c.Artifact))
	}

	placed := placedCondition(rec, trusted.Status == metav1.ConditionTrue)

	var ready metav1.Condition
	switch {
	case trusted.Status == metav1.ConditionTrue && placed.Status == metav1.ConditionTrue:
		ready = condition(cachev1alpha1.ConditionReady, true, ReasonReady, "new replicas mount the signed cache")
	case compiled.Status != metav1.ConditionTrue:
		ready = condition(cachev1alpha1.ConditionReady, false, compiled.Reason, compiled.Message)
	case trusted.Status != metav1.ConditionTrue:
		ready = condition(cachev1alpha1.ConditionReady, false, trusted.Reason, trusted.Message)
	default:
		ready = condition(cachev1alpha1.ConditionReady, false, placed.Reason, placed.Message)
	}
	out := []metav1.Condition{compiled, trusted, placed, ready}
	for i := range out {
		out[i].ObservedGeneration = rec.Generation
	}
	return out
}

func placedCondition(rec *cachev1alpha1.SpinCompiledCache, trusted bool) metav1.Condition {
	if !trusted {
		return condition(cachev1alpha1.ConditionPlaced, false, ReasonNotTrusted, "nothing is placed before the artifact is signed")
	}
	if len(rec.Status.Pools) == 0 {
		return condition(cachev1alpha1.ConditionPlaced, true, ReasonNotRequired, "no consumer checks out from a warm pool")
	}
	var waiting, failed []string
	for _, pool := range rec.Status.Pools {
		var p *cachev1alpha1.PlacementStatus
		for i := range rec.Status.Placements {
			if rec.Status.Placements[i].Pool == pool {
				p = &rec.Status.Placements[i]
			}
		}
		switch {
		case p == nil:
			waiting = append(waiting, pool)
		case p.Prepared, p.Reason == "NoTargetNodes":
			// A pool with no slots has no node to prepare.
		case placementFailed(p.Reason):
			failed = append(failed, fmt.Sprintf("%s (%s: %s)", pool, p.Reason, p.Message))
		default:
			waiting = append(waiting, fmt.Sprintf("%s (%d of %d nodes)", pool, p.PreparedNodes, p.TargetNodes))
		}
	}
	switch {
	case len(failed) > 0:
		return condition(cachev1alpha1.ConditionPlaced, false, ReasonPlacementFailed, fmt.Sprintf("preparing the artifact failed on pools %v", sorted(failed)))
	case len(waiting) > 0:
		return condition(cachev1alpha1.ConditionPlaced, false, ReasonProgressing, fmt.Sprintf("preparing the artifact on pools %v", sorted(waiting)))
	}
	return condition(cachev1alpha1.ConditionPlaced, true, ReasonPrepared, "every node of every consumer's warm pool holds the artifact")
}

// placementFailed reports whether a SwiftArtifactPlacement Prepared reason
// is a failure rather than progress.
func placementFailed(reason string) bool {
	switch reason {
	case "PrepareFailed", "VerificationFailed", "ClaimMismatch", "PolicyDenied", "ArtifactRefused", "PoolNotFound":
		return true
	}
	return false
}

// AppCondition is the SpinApp CompiledCache condition. skip is set when the
// SpinApp cannot have a cache; otherwise rec is its record, nil when the
// cache manager has not created it yet.
func AppCondition(skip *Skip, rec *cachev1alpha1.SpinCompiledCache) metav1.Condition {
	if skip != nil {
		return condition(TypeCompiledCache, false, skip.Reason, skip.Message)
	}
	if rec == nil {
		return condition(TypeCompiledCache, false, ReasonRequested,
			"waiting for the kubeswift-spin cache manager to record the request; replicas start without a compiled cache")
	}
	if TrustedArtifact(rec) != "" {
		if c := apimeta.FindStatusCondition(rec.Status.Conditions, cachev1alpha1.ConditionPlaced); c != nil && c.Status == metav1.ConditionFalse {
			reason := ReasonPlacing
			if c.Reason == ReasonPlacementFailed {
				reason = ReasonPlacementFailed
			}
			return condition(TypeCompiledCache, true, reason,
				fmt.Sprintf("new replicas mount the signed cache %s; %s", rec.Name, c.Message))
		}
		return condition(TypeCompiledCache, true, ReasonReady, fmt.Sprintf("new replicas mount the signed cache %s", rec.Name))
	}
	for _, t := range []string{cachev1alpha1.ConditionCompiled, cachev1alpha1.ConditionTrusted} {
		c := apimeta.FindStatusCondition(rec.Status.Conditions, t)
		if c == nil {
			return condition(TypeCompiledCache, false, ReasonRequested, fmt.Sprintf("record %s is waiting for the cache manager", rec.Name))
		}
		if c.Status == metav1.ConditionTrue {
			continue
		}
		reason := ReasonCompiling
		switch c.Reason {
		case ReasonCompileFailed:
			reason = ReasonCompileFailed
		case ReasonSigning, ReasonNotCompiled:
			reason = ReasonSigning
		case ReasonSignFailed, ReasonRejected:
			reason = ReasonSignFailed
		}
		return condition(TypeCompiledCache, false, reason,
			fmt.Sprintf("record %s: %s; replicas start without a compiled cache", rec.Name, c.Message))
	}
	return condition(TypeCompiledCache, false, ReasonSigning, fmt.Sprintf("record %s is waiting for its signature", rec.Name))
}

func sorted(s []string) []string {
	sort.Strings(s)
	return s
}
