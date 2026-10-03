package translate

import (
	"fmt"
	"math"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"
)

const mebibyte = int64(1) << 20

// ResourcePolicy bounds the sandbox shape a SpinApp may request. A microVM
// reserves its vCPUs and RAM for its whole lifetime, so the bounds are a
// denial-of-service control as much as a sizing rule.
type ResourcePolicy struct {
	// MinMemory is the smallest guest RAM accepted. The guest kernel and the
	// Spin runtime need memory of their own on top of the Wasm instances.
	MinMemory resource.Quantity
	// MaxMemory is the largest guest RAM accepted per replica.
	MaxMemory resource.Quantity
	// MaxVCPU is the largest vCPU count accepted per replica.
	MaxVCPU int32
}

// DefaultResourcePolicy returns the policy used when the controller flags are
// not set.
func DefaultResourcePolicy() ResourcePolicy {
	return ResourcePolicy{
		MinMemory: resource.MustParse("256Mi"),
		MaxMemory: resource.MustParse("16Gi"),
		MaxVCPU:   8,
	}
}

// pick returns the quantity that sizes the sandbox for one resource. A
// microVM's vCPU count and RAM are hard ceilings, the same role a container
// limit plays, so an explicit limit wins. A request alone is used as the
// size. When neither is set the profile default applies.
func pick(res spinv1alpha1.Resources, name corev1.ResourceName, def resource.Quantity) (resource.Quantity, string, error) {
	limit, hasLimit := res.Limits[name]
	request, hasRequest := res.Requests[name]
	if hasLimit && hasRequest && request.Cmp(limit) > 0 {
		return resource.Quantity{}, "", fmt.Errorf("resources.requests.%s (%s) exceeds resources.limits.%s (%s)",
			name, request.String(), name, limit.String())
	}
	switch {
	case hasLimit:
		return limit, "resources.limits." + string(name), nil
	case hasRequest:
		return request, "resources.requests." + string(name), nil
	default:
		return def, "executor default " + string(name), nil
	}
}

// VCPUs converts the SpinApp CPU quantity to a whole vCPU count.
//
// Rule: take resources.limits.cpu, else resources.requests.cpu, else the
// executor default; round up to the next whole CPU. 100m, 250m, 500m and 1
// all become 1 vCPU; 1500m becomes 2. Zero or negative values are rejected,
// as are values above policy.MaxVCPU.
func VCPUs(res spinv1alpha1.Resources, def resource.Quantity, policy ResourcePolicy) (int32, error) {
	q, source, err := pick(res, corev1.ResourceCPU, def)
	if err != nil {
		return 0, err
	}
	if q.Sign() <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero, got %s", source, q.String())
	}
	// Bound the value before converting so MilliValue cannot overflow.
	if q.Cmp(*resource.NewQuantity(math.MaxInt32, resource.DecimalSI)) > 0 {
		return 0, fmt.Errorf("%s value %s is out of range", source, q.String())
	}
	vcpus := (q.MilliValue() + 999) / 1000
	if vcpus > math.MaxInt32 {
		return 0, fmt.Errorf("%s value %s is out of range", source, q.String())
	}
	if policy.MaxVCPU > 0 && vcpus > int64(policy.MaxVCPU) {
		return 0, fmt.Errorf("%s is %s (%d vCPUs after rounding up), above the maximum of %d vCPUs per replica",
			source, q.String(), vcpus, policy.MaxVCPU)
	}
	return int32(vcpus), nil //nolint:gosec // vcpus is bounded by math.MaxInt32 above
}

// Memory converts the SpinApp memory quantity to the guest RAM size.
//
// Rule: take resources.limits.memory, else resources.requests.memory, else
// the executor default; convert to bytes using the quantity's own suffix
// semantics (Mi and Gi are binary, M and G are decimal); round up to a whole
// MiB; express the result in Mi. "1G" therefore becomes 954Mi and "1Gi"
// becomes 1024Mi. Values below policy.MinMemory or above policy.MaxMemory are
// rejected rather than adjusted.
func Memory(res spinv1alpha1.Resources, def resource.Quantity, policy ResourcePolicy) (resource.Quantity, error) {
	q, source, err := pick(res, corev1.ResourceMemory, def)
	if err != nil {
		return resource.Quantity{}, err
	}
	if q.Sign() <= 0 {
		return resource.Quantity{}, fmt.Errorf("%s must be greater than zero, got %s", source, q.String())
	}
	if !policy.MaxMemory.IsZero() && q.Cmp(policy.MaxMemory) > 0 {
		return resource.Quantity{}, fmt.Errorf("%s is %s, above the maximum of %s per replica",
			source, q.String(), policy.MaxMemory.String())
	}
	// Value rounds a fractional byte count up, so the guest never gets less
	// than requested.
	bytes := q.Value()
	mib := (bytes + mebibyte - 1) / mebibyte
	out := resource.MustParse(fmt.Sprintf("%dMi", mib))
	if !policy.MinMemory.IsZero() && out.Cmp(policy.MinMemory) < 0 {
		return resource.Quantity{}, fmt.Errorf("%s is %s, below the minimum of %s for a KubeSwift sandbox running Spin (the guest kernel and Spin runtime need memory of their own)",
			source, q.String(), policy.MinMemory.String())
	}
	return out, nil
}
