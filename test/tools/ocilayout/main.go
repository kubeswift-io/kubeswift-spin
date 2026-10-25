// Command ocilayout writes an OCI artifact as an OCI image layout the way
// KubeSwift's materializer does for spec.artifacts with layout "oci", and
// prints the manifest digest. Test tool for hack/test-runtime-image.sh.
//
// Usage: ocilayout <reference> <directory>
package main

import (
	"fmt"
	"os"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: ocilayout <reference> <directory>")
		os.Exit(2)
	}
	if err := write(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "ocilayout:", err)
		os.Exit(1)
	}
}

func write(ref, dir string) error {
	// name.Insecure only allows plain HTTP for hosts go-containerregistry
	// already treats as local (the test registry on localhost).
	r, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		return err
	}
	desc, err := remote.Get(r)
	if err != nil {
		return err
	}
	img, err := desc.Image()
	if err != nil {
		return err
	}
	lp, err := layout.Write(dir, empty.Index)
	if err != nil {
		return err
	}
	if err := lp.AppendImage(img, layout.WithAnnotations(map[string]string{"org.opencontainers.image.ref.name": r.String()})); err != nil {
		return err
	}
	fmt.Println(desc.Digest.String())
	return nil
}
