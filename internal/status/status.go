// Package status maps KubeSwift execution state onto the standard SpinApp
// status: the Available and Progressing conditions SpinKube defines, and
// readyReplicas.
//
// kubeswift-spin owns these fields only for SpinApps whose executor it
// manages. Spin Operator writes no status at all when an executor has
// createDeployment: false (verified against v0.6.1), so there is no second
// writer to coordinate with. The controller still patches with an optimistic
// lock and preserves conditions of any other type.
package status

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubeswift-io/kubeswift-spin/internal/rollout"
)

// Condition types defined by SpinKube.
const (
	TypeAvailable   = "Available"
	TypeProgressing = "Progressing"
)

// Condition reasons. They are part of the user-facing contract; do not
// rename them.
const (
	ReasonExecutorNotFound         = "ExecutorNotFound"
	ReasonExecutorInvalid          = "ExecutorInvalid"
	ReasonUnsupportedConfiguration = "UnsupportedConfiguration"
	ReasonWarmPoolIncompatible     = "WarmPoolIncompatible"
	ReasonSandboxConflict          = "SandboxConflict"
	ReasonSandboxCreating          = "SandboxCreating"
	ReasonSandboxMaterializing     = "SandboxMaterializing"
	ReasonRollingUpdate            = "RollingUpdate"
	ReasonSandboxRunning           = "SandboxRunning"
	ReasonSandboxFailed            = "SandboxFailed"
	ReasonRuntimeImageUnavailable  = "RuntimeImageUnavailable"
	ReasonNetworkUnavailable       = "NetworkUnavailable"
	ReasonApplicationReady         = "ApplicationReady"
)

// maxDetail bounds text copied from a SwiftSandbox status message.
const maxDetail = 200

// Instance is the status view of one replica.
type Instance struct {
	rollout.Instance
	// FailureReason and FailureMessage come from the SwiftSandbox
	// GuestRunning condition when the sandbox is terminal.
	FailureReason  string
	FailureMessage string
	// Materializing is true while KubeSwift builds the rootfs.
	Materializing bool
	// WaitingReason is the SwiftSandbox Resolved=False reason, such as
	// KernelNotFound, while the sandbox cannot start.
	WaitingReason  string
	WaitingMessage string
}

// Exposure describes whether replicas can be reached and probed.
type Exposure struct {
	// Detected is true when the installed SwiftSandbox CRD advertises port
	// exposure and readiness probes.
	Detected bool
	// Implemented is true when this kubeswift-spin build uses them.
	Implemented bool
}

// Usable reports whether replicas can be exposed and their readiness
// verified.
func (e Exposure) Usable() bool { return e.Detected && e.Implemented }

// Blocker is a reason the controller is not converging.
type Blocker struct {
	Reason  string
	Message string
}

// Input is everything status computation needs.
type Input struct {
	Generation int64
	Replicas   int32
	Revision   string
	Instances  []Instance
	Exposure   Exposure
	Blocker    *Blocker
	// RetryIn describes a pending backoff, for example "20s".
	RetryIn string
}

// Result is the computed status.
type Result struct {
	ReadyReplicas int32
	Available     metav1.Condition
	Progressing   metav1.Condition
}

// Compute derives the SpinApp conditions and ready replica count.
func Compute(in Input) Result {
	var ready, running, current int32
	var failed []Instance
	pending, materializing, outdated := 0, 0, 0
	var waiting *Instance
	for i := range in.Instances {
		inst := in.Instances[i]
		if inst.Deleting || inst.Stale || int32(inst.Ordinal) >= in.Replicas {
			continue
		}
		if inst.Revision == in.Revision {
			current++
		} else {
			outdated++
		}
		switch inst.Health {
		case rollout.Ready:
			ready++
			running++
		case rollout.Running:
			running++
		case rollout.Terminal:
			failed = append(failed, inst)
		default:
			pending++
			if inst.Materializing {
				materializing++
			}
			if inst.WaitingReason != "" && waiting == nil {
				waiting = &in.Instances[i]
			}
		}
	}

	r := Result{ReadyReplicas: ready}
	cond := func(t string, s metav1.ConditionStatus, reason, msg string) metav1.Condition {
		return metav1.Condition{Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: in.Generation}
	}

	// Progressing.
	switch {
	case in.Blocker != nil:
		r.Progressing = cond(TypeProgressing, metav1.ConditionFalse, in.Blocker.Reason, in.Blocker.Message)
	case len(failed) > 0:
		f := failed[0]
		reason := ReasonSandboxFailed
		if isRuntimeImageFailure(f.FailureReason) {
			reason = ReasonRuntimeImageUnavailable
		}
		msg := fmt.Sprintf("sandbox %s failed (%s)", f.Name, detail(f.FailureReason, f.FailureMessage))
		if reason == ReasonSandboxFailed {
			msg += "; inspect the Spin output with `swiftctl sandbox logs " + f.Name + "`"
		}
		if in.RetryIn != "" {
			msg += "; replacing in " + in.RetryIn
		}
		r.Progressing = cond(TypeProgressing, metav1.ConditionFalse, reason, msg)
	case waiting != nil:
		r.Progressing = cond(TypeProgressing, metav1.ConditionTrue, ReasonSandboxCreating,
			fmt.Sprintf("sandbox %s is waiting: %s", waiting.Name, detail(waiting.WaitingReason, waiting.WaitingMessage)))
	case outdated > 0:
		r.Progressing = cond(TypeProgressing, metav1.ConditionTrue, ReasonRollingUpdate,
			fmt.Sprintf("replacing sandboxes with revision %s: %d of %d updated", in.Revision, current, in.Replicas))
	case materializing > 0:
		r.Progressing = cond(TypeProgressing, metav1.ConditionTrue, ReasonSandboxMaterializing,
			fmt.Sprintf("%d of %d sandboxes are materializing the runtime rootfs", materializing, in.Replicas))
	case pending > 0 || current < in.Replicas:
		r.Progressing = cond(TypeProgressing, metav1.ConditionTrue, ReasonSandboxCreating,
			fmt.Sprintf("%d of %d sandboxes running", running, in.Replicas))
	default:
		r.Progressing = cond(TypeProgressing, metav1.ConditionTrue, ReasonSandboxRunning,
			fmt.Sprintf("%d of %d sandboxes running at revision %s", running, in.Replicas, in.Revision))
	}

	// Available follows Deployment semantics with maxUnavailable=1: the app
	// is available when at most one desired replica is not ready.
	need := in.Replicas - 1
	if need < 1 {
		need = 1
	}
	switch {
	case ready >= need:
		r.Available = cond(TypeAvailable, metav1.ConditionTrue, ReasonApplicationReady,
			fmt.Sprintf("%d of %d replicas ready", ready, in.Replicas))
	case running > 0 && !in.Exposure.Usable():
		r.Available = cond(TypeAvailable, metav1.ConditionFalse, ReasonNetworkUnavailable,
			fmt.Sprintf("%d of %d sandboxes running, but no replica is reported ready: %s", running, in.Replicas, exposureMessage(in.Exposure)))
	default:
		r.Available = cond(TypeAvailable, metav1.ConditionFalse, r.Progressing.Reason,
			fmt.Sprintf("%d of %d replicas ready", ready, in.Replicas))
	}
	return r
}

func exposureMessage(e Exposure) string {
	if e.Detected && !e.Implemented {
		return "the installed KubeSwift advertises sandbox port exposure and readiness probes, but this kubeswift-spin release does not use them yet; upgrade kubeswift-spin"
	}
	return "the installed KubeSwift SwiftSandbox API has no inbound port exposure or readiness probes, so the Spin HTTP listener cannot be reached or verified (see docs/upstream/kubeswift-sandbox-service-exposure.md)"
}

// isRuntimeImageFailure reports whether a SwiftSandbox failure reason means
// the runtime rootfs image could not be pulled, verified or materialized.
func isRuntimeImageFailure(reason string) bool {
	switch reason {
	case "ImageResolveFailed", "ImagePullSecretInvalid", "RootfsMaterializeFailed":
		return true
	}
	return false
}

func detail(reason, msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) > maxDetail {
		msg = msg[:maxDetail] + "..."
	}
	switch {
	case reason != "" && msg != "":
		return reason + ": " + msg
	case reason != "":
		return reason
	case msg != "":
		return msg
	default:
		return "no reason reported"
	}
}

// Apply sets the computed conditions on conds, preserving conditions of any
// other type. It returns true when anything changed.
func Apply(conds *[]metav1.Condition, r Result) bool {
	a := meta.SetStatusCondition(conds, r.Available)
	p := meta.SetStatusCondition(conds, r.Progressing)
	return a || p
}
