// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package credentials

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
)

func TestAuthMethod_HTTPSBasic(t *testing.T) {
	auth, err := authMethod(&Credential{
		Method:   AuthMethodHTTPSBasic,
		Username: "alice",
		Password: []byte("ghp_token"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	basic, ok := auth.(*githttp.BasicAuth)
	if !ok {
		t.Fatalf("auth type = %T, want *http.BasicAuth", auth)
	}
	if basic.Username != "alice" || basic.Password != "ghp_token" {
		t.Errorf("basic auth fields not set correctly")
	}
}

func TestAuthMethod_SSHRequiresKnownHosts(t *testing.T) {
	_, err := authMethod(&Credential{
		Method:   AuthMethodSSH,
		Identity: []byte("does-not-matter"),
		// no KnownHosts
	})
	if !errors.Is(err, ErrKnownHostsRequired) {
		t.Fatalf("error = %v, want ErrKnownHostsRequired", err)
	}
}

func TestAuthMethod_SSHValid(t *testing.T) {
	priv, knownHosts := genSSHKeyAndKnownHosts(t)

	auth, err := authMethod(&Credential{
		Method:     AuthMethodSSH,
		Identity:   priv,
		KnownHosts: knownHosts,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := auth.(*gitssh.PublicKeys); !ok {
		t.Fatalf("auth type = %T, want *ssh.PublicKeys", auth)
	}
}

func TestKnownHostsCallback(t *testing.T) {
	_, knownHosts := genSSHKeyAndKnownHosts(t)
	// The public key that known_hosts pins (the server key).
	serverKey := parseFirstKnownHostKey(t, knownHosts)

	cb, err := knownHostsCallback(knownHosts)
	if err != nil {
		t.Fatalf("unexpected error building callback: %v", err)
	}

	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 22}

	t.Run("matching host and key is accepted", func(t *testing.T) {
		if err := cb("github.com:22", addr, serverKey); err != nil {
			t.Errorf("expected accept, got %v", err)
		}
	})

	t.Run("unknown host is rejected", func(t *testing.T) {
		if err := cb("gitlab.com:22", addr, serverKey); err == nil {
			t.Error("expected rejection for unknown host")
		}
	})

	t.Run("mismatched key is rejected", func(t *testing.T) {
		_, otherKH := genSSHKeyAndKnownHosts(t)
		otherKey := parseFirstKnownHostKey(t, otherKH)
		if err := cb("github.com:22", addr, otherKey); err == nil {
			t.Error("expected rejection for mismatched key")
		}
	})

	// Regression guard: a host with MULTIPLE keys (as real known_hosts have,
	// e.g. GitHub's ed25519/rsa/ecdsa). The server may present any one of them;
	// the callback must accept a match on a non-first entry, not bail on the
	// first line. This is the case the earlier hand-rolled callback got wrong.
	t.Run("second of multiple keys for same host is accepted", func(t *testing.T) {
		_, kh1 := genSSHKeyAndKnownHosts(t)
		_, kh2 := genSSHKeyAndKnownHosts(t)
		combined := append(append([]byte{}, kh1...), kh2...)

		multiCB, err := knownHostsCallback(combined)
		if err != nil {
			t.Fatalf("unexpected error building callback: %v", err)
		}
		serverKey2 := parseFirstKnownHostKey(t, kh2)
		if err := multiCB("github.com:22", addr, serverKey2); err != nil {
			t.Errorf("expected accept for second listed key, got %v", err)
		}
	})
}

func TestKnownHostsCallback_Invalid(t *testing.T) {
	if _, err := knownHostsCallback([]byte("this is not a known_hosts line")); err == nil {
		t.Error("expected error for malformed known_hosts")
	}
}

func TestClassifyListError(t *testing.T) {
	tests := []struct {
		name string
		in   error
		want error
	}{
		{"auth required", transport.ErrAuthenticationRequired, ErrAuthFailed},
		{"authz failed", transport.ErrAuthorizationFailed, ErrAuthFailed},
		{"invalid auth method", transport.ErrInvalidAuthMethod, ErrAuthFailed},
		{"other -> unreachable", errors.New("dial tcp: no such host"), ErrRepoUnreachable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyListError(tt.in)
			if !errors.Is(got, tt.want) {
				t.Errorf("classifyListError(%v) => %v, want errors.Is(_, %v)", tt.in, got, tt.want)
			}
		})
	}
}

func TestValidateAccess_NilCredential(t *testing.T) {
	if err := ValidateAccess(context.Background(), "https://example.com/x.git", nil); err == nil {
		t.Error("expected error for nil credential")
	}
}

// ValidateAccess treats an empty-but-reachable remote as success (auth worked,
// repo just has no refs yet). This guards that intent: if classifyListError
// ever starts treating ErrEmptyRemoteRepository as an auth failure, the
// short-circuit in ValidateAccess must still return nil for it.
func TestEmptyRemoteRepositoryIsNotAuthFailure(t *testing.T) {
	if errors.Is(classifyListError(transport.ErrEmptyRemoteRepository), ErrAuthFailed) {
		t.Error("empty remote must not be classified as an auth failure")
	}
}

// --- helpers ---

// genSSHKeyAndKnownHosts creates an ed25519 keypair, returns the PEM-encoded
// private key and a known_hosts file pinning the public key to github.com.
func genSSHKeyAndKnownHosts(t *testing.T) (privPEM, knownHosts []byte) {
	t.Helper()
	const host = "github.com"
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemBlock, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh public key: %v", err)
	}
	line := knownHostsLine(host, sshPub)
	return pem.EncodeToMemory(pemBlock), []byte(line)
}

func parseFirstKnownHostKey(t *testing.T, knownHosts []byte) ssh.PublicKey {
	t.Helper()
	_, _, pubKey, _, _, err := ssh.ParseKnownHosts(knownHosts)
	if err != nil {
		t.Fatalf("parse known_hosts: %v", err)
	}
	return pubKey
}

func knownHostsLine(host string, key ssh.PublicKey) string {
	return fmt.Sprintf("%s %s %s\n", host, key.Type(), base64.StdEncoding.EncodeToString(key.Marshal()))
}
