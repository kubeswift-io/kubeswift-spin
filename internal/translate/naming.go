package translate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	// maxSandboxName keeps a SwiftSandbox name usable as a label value and
	// a pod hostname. KubeSwift names the launcher pod after the sandbox and
	// labels it sandbox.kubeswift.io/sandbox=<name>, so 63 characters is a
	// hard limit.
	maxSandboxName = 63
	// ordinalReserve is the space kept for "-<ordinal>". The replica limit
	// is far below 10000, so four characters are always enough.
	ordinalReserve = 5
	hashLen        = 8
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// shortHash is a stable, non-reversible identifier derived from s.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:hashLen]
}

// baseName returns the deterministic prefix used for all children of a
// SpinApp. A name that is already a short DNS label is used unchanged, so a
// SpinApp named "hello" gets sandboxes "hello-0", "hello-1". Longer names,
// and names containing dots (legal for a SpinApp, not for a pod hostname),
// are truncated and suffixed with a hash of the full name so two different
// SpinApps can never map to the same prefix.
func baseName(app string) string {
	limit := maxSandboxName - ordinalReserve
	if len(app) <= limit && dnsLabel.MatchString(app) {
		return app
	}
	clean := strings.Trim(strings.ReplaceAll(app, ".", "-"), "-")
	keep := limit - hashLen - 1
	if len(clean) > keep {
		clean = strings.TrimRight(clean[:keep], "-")
	}
	if clean == "" {
		return "spin-" + shortHash(app)
	}
	return clean + "-" + shortHash(app)
}

// SandboxName returns the SwiftSandbox name for one replica ordinal.
func SandboxName(app string, ordinal int) string {
	return baseName(app) + "-" + strconv.Itoa(ordinal)
}

// AppLabelValue returns a label-safe value identifying a SpinApp. It is used
// for selection only; the controller owner reference is the authority.
func AppLabelValue(app string) string {
	return baseName(app)
}

// ParseOrdinal extracts the replica ordinal from a label value.
func ParseOrdinal(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid ordinal %q", v)
	}
	return n, nil
}
