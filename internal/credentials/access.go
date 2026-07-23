// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package credentials

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"
)

// Classified errors so the reconciler can map to distinct status reasons.
// None of these carry secret material.
var (
	// ErrAuthFailed indicates the credential was rejected by the remote.
	ErrAuthFailed = errors.New("git authentication failed")
	// ErrRepoUnreachable indicates a network/transport failure reaching the remote.
	ErrRepoUnreachable = errors.New("git repository unreachable")
	// ErrKnownHostsRequired indicates an SSH credential lacked known_hosts.
	ErrKnownHostsRequired = errors.New("known_hosts is required for SSH access")
)

// defaultSSHUser is the conventional SSH user for Git hosting providers
// (e.g. git@github.com).
const defaultSSHUser = "git"

// ValidateAccess verifies that cred can reach repoURL by performing a
// git ls-remote against the remote. It performs network I/O and does not
// clone anything. Returns nil on success, or one of the classified errors
// above (wrapped) on failure.
func ValidateAccess(ctx context.Context, repoURL string, cred *Credential) error {
	if cred == nil {
		return errors.New("credential is nil")
	}

	auth, err := authMethod(cred)
	if err != nil {
		return err
	}

	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: "origin",
		URLs: []string{repoURL},
	})

	if _, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth}); err != nil {
		// An empty remote is reachable and authenticated — it simply has no
		// refs yet (e.g. a freshly created repository). For credential
		// validation that is a success, not a failure.
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return nil
		}
		return classifyListError(err)
	}
	return nil
}

// authMethod builds the go-git transport auth for the credential.
func authMethod(cred *Credential) (transport.AuthMethod, error) {
	switch cred.Method {
	case AuthMethodHTTPSBasic:
		return &githttp.BasicAuth{
			Username: cred.Username,
			Password: string(cred.Password),
		}, nil
	case AuthMethodSSH:
		return sshAuth(cred)
	default:
		return nil, fmt.Errorf("unsupported auth method %q", cred.Method)
	}
}

// sshAuth builds SSH public-key auth with strict, in-memory host-key
// verification against the provided known_hosts.
func sshAuth(cred *Credential) (transport.AuthMethod, error) {
	if len(cred.KnownHosts) == 0 {
		return nil, ErrKnownHostsRequired
	}

	// go-git parses the private key (with optional passphrase) from bytes.
	auth, err := gitssh.NewPublicKeys(defaultSSHUser, cred.Identity, string(cred.Passphrase))
	if err != nil {
		// Do not wrap: key parse errors can echo key material in some paths.
		return nil, errors.New("SSH credential: could not parse private key")
	}

	hostKeyCB, err := knownHostsCallback(cred.KnownHosts)
	if err != nil {
		return nil, err
	}
	auth.HostKeyCallback = hostKeyCB
	return auth, nil
}

// knownHostsCallback builds an ssh.HostKeyCallback from in-memory known_hosts
// bytes. The bytes are written to a private temp file because the parser is
// file-based; the file is removed before returning. It uses skeema/knownhosts
// (the same library go-git uses internally) rather than the stdlib parser
// because it correctly matches a port-qualified host (go-git passes
// "github.com:22") against a bare "github.com" entry, and handles multiple host
// keys per host. Strict verification is kept: an unknown or mismatched host key
// is rejected.
func knownHostsCallback(knownHostsData []byte) (ssh.HostKeyCallback, error) {
	f, err := os.CreateTemp("", "known_hosts-*")
	if err != nil {
		return nil, fmt.Errorf("SSH credential: preparing known_hosts: %w", err)
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()

	if _, err := f.Write(knownHostsData); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("SSH credential: writing known_hosts: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("SSH credential: writing known_hosts: %w", err)
	}

	cb, err := knownhosts.New(name)
	if err != nil {
		return nil, fmt.Errorf("SSH credential: invalid known_hosts: %w", err)
	}
	return cb.HostKeyCallback(), nil
}

// classifyListError maps go-git transport errors to our classified errors so
// the caller can distinguish auth failures from unreachable repositories.
// The original error is wrapped for logs but the classified sentinel drives
// status reasons.
func classifyListError(err error) error {
	switch {
	case errors.Is(err, transport.ErrAuthenticationRequired),
		errors.Is(err, transport.ErrAuthorizationFailed),
		errors.Is(err, transport.ErrInvalidAuthMethod):
		return fmt.Errorf("%w: %v", ErrAuthFailed, err)
	default:
		return fmt.Errorf("%w: %v", ErrRepoUnreachable, err)
	}
}
