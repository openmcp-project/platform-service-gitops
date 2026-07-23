// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

// Package credentials resolves and validates Git credentials referenced by a
// GitRepository. This file handles the kind:Secret path (#538): a user-supplied
// Kubernetes Secret containing either HTTPS PAT or SSH credentials, using the
// Flux source.toolkit.fluxcd.io Secret key convention so the credential can be
// propagated to MCPs unchanged.
package credentials

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// AuthMethod identifies how a resolved Git credential authenticates.
type AuthMethod string

const (
	// AuthMethodHTTPSBasic is username + password (Personal Access Token) auth.
	AuthMethodHTTPSBasic AuthMethod = "HTTPSBasic"
	// AuthMethodSSH is SSH private-key auth.
	AuthMethodSSH AuthMethod = "SSH"
)

// Flux Secret data keys (source.toolkit.fluxcd.io convention). We adopt these
// verbatim so a validated Secret can be synced into an MCP and consumed by a
// Flux GitRepository without any key translation.
const (
	keyUsername   = "username"    // HTTPS basic auth user
	keyPassword   = "password"    // HTTPS basic auth token; OR optional SSH key passphrase
	keyIdentity   = "identity"    // SSH private key
	keyKnownHosts = "known_hosts" // SSH known_hosts
)

// ErrUnsupportedSecretFormat is returned when a Secret matches neither the
// HTTPS PAT nor the SSH shape supported by #538.
var ErrUnsupportedSecretFormat = errors.New("secret matches neither HTTPS (username/password) nor SSH (identity) format")

// Credential is the normalized result of resolving a Git credential Secret.
// Callers branch on Method; secret material is intentionally not stringified
// anywhere (no String() method) to avoid accidental logging.
type Credential struct {
	Method AuthMethod

	// HTTPS basic auth (set when Method == AuthMethodHTTPSBasic).
	Username string
	Password []byte

	// SSH (set when Method == AuthMethodSSH).
	Identity   []byte
	KnownHosts []byte
	Passphrase []byte // optional; from the "password" key
}

// ResolveSecret inspects a user-supplied Secret and returns a normalized
// Credential, or an error describing why it is invalid. It performs no network
// I/O — repository reachability/validity is checked separately (#8).
//
// Detection keys off the presence of the "identity" key (SSH), because
// "password" is overloaded across both formats and cannot discriminate.
func ResolveSecret(secret *corev1.Secret) (*Credential, error) {
	if secret == nil {
		return nil, errors.New("secret is nil")
	}

	switch {
	case has(secret, keyIdentity):
		return resolveSSH(secret)
	case has(secret, keyUsername):
		return resolveHTTPS(secret)
	default:
		return nil, ErrUnsupportedSecretFormat
	}
}

func resolveHTTPS(secret *corev1.Secret) (*Credential, error) {
	username := secret.Data[keyUsername]
	password := secret.Data[keyPassword]
	if len(username) == 0 {
		return nil, fmt.Errorf("HTTPS credential: %q is required", keyUsername)
	}
	if len(password) == 0 {
		return nil, fmt.Errorf("HTTPS credential: %q is required", keyPassword)
	}
	return &Credential{
		Method:   AuthMethodHTTPSBasic,
		Username: string(username),
		Password: password,
	}, nil
}

func resolveSSH(secret *corev1.Secret) (*Credential, error) {
	identity := secret.Data[keyIdentity]
	if len(identity) == 0 {
		return nil, fmt.Errorf("SSH credential: %q is required", keyIdentity)
	}
	return &Credential{
		Method:     AuthMethodSSH,
		Identity:   identity,
		KnownHosts: secret.Data[keyKnownHosts], // optional here; enforced when validating access (#8)
		Passphrase: secret.Data[keyPassword],   // optional passphrase
	}, nil
}

func has(secret *corev1.Secret, key string) bool {
	return len(secret.Data[key]) > 0
}
