//go:build tools

// Package cosign pins the cosign CLI that the cache signer image builds from
// source, so its dependencies are resolved, scanned and updated here.
package cosign

import _ "github.com/sigstore/cosign/v3/cmd/cosign"
