// Package cosign pins the cosign CLI that the cache signer image builds from
// source, so its dependencies are resolved, scanned and updated here.
//
// Run mirrors cosign's main package (cmd/cosign/main.go): the same command
// tree and the same KMS providers. It is never called. It exists so that
// govulncheck, which OSV-Scanner runs on this module, has a package to
// analyze: the import in tools.go is behind a build tag, because Go cannot
// import a main package, and without this file the call analysis fails and
// OSV-Scanner exits with an error.
package cosign

import (
	"github.com/sigstore/cosign/v3/cmd/cosign/cli"
	_ "github.com/sigstore/cosign/v3/cmd/cosign/errors"

	_ "github.com/sigstore/sigstore/pkg/signature/kms/aws"
	_ "github.com/sigstore/sigstore/pkg/signature/kms/azure"
	_ "github.com/sigstore/sigstore/pkg/signature/kms/gcp"
	_ "github.com/sigstore/sigstore/pkg/signature/kms/hashivault"
)

// Run executes the cosign command line as cosign's main does.
func Run() error {
	return cli.New().Execute()
}
