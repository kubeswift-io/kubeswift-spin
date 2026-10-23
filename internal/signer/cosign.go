package signer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Signer signs an artifact digest with claims.
type Signer interface {
	// Sign signs ref (repository@sha256:...) with the claims as signed
	// annotations.
	Sign(ctx context.Context, ref string, claims map[string]string) error
	// KeyID is the SHA-256 of the signing key's SubjectPublicKeyInfo.
	KeyID(ctx context.Context) (string, error)
}

// Cosign signs with the cosign CLI, the same major version KubeSwift
// verifies with. The key is a file or a KMS reference cosign understands
// (awskms://, gcpkms://, azurekms://, hashivault://); its password, for a
// file key, is in COSIGN_PASSWORD. Registry credentials for pushing the
// signature come from DOCKER_CONFIG.
type Cosign struct {
	Binary string
	Key    string
	// SigningConfig names a sigstore signing configuration without a
	// transparency log or timestamp authority: signatures are verified
	// offline against the key, as KubeSwift verifies them.
	SigningConfig string
	Timeout       time.Duration
	// Env is the environment cosign runs with (COSIGN_PASSWORD,
	// DOCKER_CONFIG, KMS credentials).
	Env []string
}

func (c *Cosign) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Binary, args...) //nolint:gosec // fixed binary, validated arguments
	cmd.Env = c.Env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("cosign %s: %w: %s", args[0], err, lastLine(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// lastLine keeps cosign's final error line; cosign never prints key
// material.
func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// Sign implements Signer.
func (c *Cosign) Sign(ctx context.Context, ref string, claims map[string]string) error {
	args := []string{"sign", "--yes", "--key", c.Key, "--signing-config", c.SigningConfig}
	keys := make([]string, 0, len(claims))
	for k := range claims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-a", k+"="+claims[k])
	}
	args = append(args, ref)
	_, err := c.run(ctx, args...)
	return err
}

// KeyID implements Signer.
func (c *Cosign) KeyID(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "public-key", "--key", c.Key)
	if err != nil {
		return "", err
	}
	return KeyIDFromPEM(out)
}

// KeyIDFromPEM is the SHA-256 of a PEM public key's SubjectPublicKeyInfo,
// as SwiftArtifactVerificationPolicy reports it in status.authorities.
func KeyIDFromPEM(b []byte) (string, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PUBLIC KEY" {
		return "", errors.New("not a PEM public key")
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

// CheckSigningConfig requires the signing configuration file to exist.
func CheckSigningConfig(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("signing configuration %s: %w", path, err)
	}
	return nil
}
