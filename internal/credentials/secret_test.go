// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package credentials_test

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/openmcp-project/platform-service-gitops/internal/credentials"
)

func secret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{Data: data}
}

// Secret data keys, duplicated from the package under test for readability.
const (
	keyUsername = "username"
	keyPassword = "password"
)

func TestResolveSecret_HTTPSBasic(t *testing.T) {
	cred, err := credentials.ResolveSecret(secret(map[string][]byte{
		keyUsername: []byte("alice"),
		keyPassword: []byte("ghp_token"),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.Method != credentials.AuthMethodHTTPSBasic {
		t.Errorf("Method = %q, want %q", cred.Method, credentials.AuthMethodHTTPSBasic)
	}
	if cred.Username != "alice" {
		t.Errorf("Username = %q, want %q", cred.Username, "alice")
	}
	if string(cred.Password) != "ghp_token" {
		t.Errorf("Password mismatch")
	}
}

func TestResolveSecret_SSH(t *testing.T) {
	cred, err := credentials.ResolveSecret(secret(map[string][]byte{
		"identity":    []byte("PRIVATE-KEY"),
		"known_hosts": []byte("github.com ssh-ed25519 AAAA"),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.Method != credentials.AuthMethodSSH {
		t.Errorf("Method = %q, want %q", cred.Method, credentials.AuthMethodSSH)
	}
	if string(cred.Identity) != "PRIVATE-KEY" {
		t.Errorf("Identity mismatch")
	}
	if string(cred.KnownHosts) != "github.com ssh-ed25519 AAAA" {
		t.Errorf("KnownHosts mismatch")
	}
}

// The "password" key is overloaded (PAT vs SSH passphrase). Presence of
// "identity" must win, and "password" must be read as the passphrase — not
// mistaken for an HTTPS credential.
func TestResolveSecret_SSHWithPassphrase(t *testing.T) {
	cred, err := credentials.ResolveSecret(secret(map[string][]byte{
		"identity":  []byte("PRIVATE-KEY"),
		keyPassword: []byte("passphrase"),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.Method != credentials.AuthMethodSSH {
		t.Errorf("Method = %q, want %q (identity must win over password)", cred.Method, credentials.AuthMethodSSH)
	}
	if string(cred.Passphrase) != "passphrase" {
		t.Errorf("Passphrase = %q, want %q", cred.Passphrase, "passphrase")
	}
}

func TestResolveSecret_Errors(t *testing.T) {
	tests := []struct {
		name    string
		data    map[string][]byte
		wantErr error // errors.Is target; nil means "any error"
	}{
		{
			name:    "empty secret",
			data:    map[string][]byte{},
			wantErr: credentials.ErrUnsupportedSecretFormat,
		},
		{
			name:    "unsupported format (bearerToken only)",
			data:    map[string][]byte{"bearerToken": []byte("xxx")},
			wantErr: credentials.ErrUnsupportedSecretFormat,
		},
		{
			name: "HTTPS missing password",
			data: map[string][]byte{keyUsername: []byte("alice")},
		},
		{
			name: "HTTPS empty password",
			data: map[string][]byte{keyUsername: []byte("alice"), keyPassword: []byte("")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := credentials.ResolveSecret(secret(tt.data))
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want errors.Is(_, %v)", err, tt.wantErr)
			}
		})
	}
}

func TestResolveSecret_NilSecret(t *testing.T) {
	if _, err := credentials.ResolveSecret(nil); err == nil {
		t.Fatal("expected error for nil secret")
	}
}
