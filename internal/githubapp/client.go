// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

// Package githubapp provides a thin wrapper around go-github + ghinstallation
// to authenticate as a GitHub App and check whether it is installed on a given
// organization or user. It works against both public github.com and GitHub
// Enterprise Server, deriving the mode from the instance URL.
//
// The logic mirrors the verified ghapp-check probe: sign an App JWT from the
// private key, then call GET /app and GET /orgs|users/{name}/installation.
package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v88/github"
)

// Credentials hold the material needed to authenticate as a GitHub App on a
// specific GitHub instance. They are read from a Kubernetes Secret.
type Credentials struct {
	// AppID is the numeric GitHub App ID.
	AppID int64
	// PrivateKey is the PEM-encoded RSA private key of the App.
	PrivateKey []byte
	// URL is the base URL of the GitHub instance, e.g. https://github.com or
	// https://github.tools.sap.
	URL string
}

// InstallationResult describes the outcome of an installation check.
type InstallationResult struct {
	// Installed reports whether the App is installed on the target.
	Installed bool
	// InstallationID is the numeric installation ID (0 when not installed).
	InstallationID int64
	// Account is the login of the account the App is installed on.
	Account string
}

// ErrNotInstalled is returned when the App is not installed on the target.
// Callers map this to an AppNotInstalled condition rather than an error loop.
var ErrNotInstalled = errors.New("github app is not installed")

// Client authenticates as a GitHub App against one GitHub instance.
type Client struct {
	gh *github.Client
}

// isPublic reports whether the URL points at public github.com.
func isPublic(url string) bool {
	h := strings.ToLower(strings.TrimSpace(url))
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	h = strings.TrimSuffix(h, "/")
	return h == "" || h == "github.com" || h == "api.github.com" || h == "www.github.com"
}

// requestTimeout bounds each GitHub call so a slow or hung instance cannot
// block a reconciler goroutine indefinitely.
const requestTimeout = 30 * time.Second

// NewClient builds an App-authenticated client for the given credentials.
// For Enterprise instances it configures both the go-github base URL and the
// ghinstallation transport BaseURL so tokens are minted against the instance.
func NewClient(creds Credentials) (*Client, error) {
	// Clone the default transport so each client has its own connection pool
	// rather than sharing http.DefaultTransport across all reconcilers.
	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	atr, err := ghinstallation.NewAppsTransport(baseTransport, creds.AppID, creds.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("building app transport: %w", err)
	}

	httpClient := &http.Client{Transport: atr, Timeout: requestTimeout}

	if isPublic(creds.URL) {
		gh, err := github.NewClient(github.WithHTTPClient(httpClient))
		if err != nil {
			return nil, fmt.Errorf("building client: %w", err)
		}
		return &Client{gh: gh}, nil
	}

	base := strings.TrimSuffix(creds.URL, "/") + "/"
	gh, err := github.NewClient(
		github.WithHTTPClient(httpClient),
		github.WithEnterpriseURLs(base, base),
	)
	if err != nil {
		return nil, fmt.Errorf("configuring enterprise URLs: %w", err)
	}
	// The transport needs the same api/v3 base, otherwise it would fetch tokens
	// from github.com.
	atr.BaseURL = strings.TrimSuffix(creds.URL, "/") + "/api/v3"

	return &Client{gh: gh}, nil
}

// VerifyApp authenticates as the App (GET /app) and returns its slug. A failure
// here means the credentials (App ID / private key) are wrong or the instance
// is unreachable.
func (c *Client) VerifyApp(ctx context.Context) (string, error) {
	app, _, err := c.gh.Apps.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("authenticating as app: %w", err)
	}
	return app.GetSlug(), nil
}

// CheckOrgInstallation checks whether the App is installed on the given org.
// A 404 is reported as Installed=false (not an error), matching #539.
func (c *Client) CheckOrgInstallation(ctx context.Context, org string) (InstallationResult, error) {
	inst, resp, err := c.gh.Apps.GetOrganizationInstallation(ctx, org)
	return installationResult(inst, resp, err)
}

// CheckUserInstallation checks whether the App is installed on the given user.
// A 404 is reported as Installed=false (not an error), matching #539.
func (c *Client) CheckUserInstallation(ctx context.Context, user string) (InstallationResult, error) {
	inst, resp, err := c.gh.Apps.GetUserInstallation(ctx, user)
	return installationResult(inst, resp, err)
}

func installationResult(inst *github.Installation, resp *github.Response, err error) (InstallationResult, error) {
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return InstallationResult{Installed: false}, nil
		}
		return InstallationResult{}, fmt.Errorf("checking installation: %w", err)
	}
	return InstallationResult{
		Installed:      true,
		InstallationID: inst.GetID(),
		Account:        inst.GetAccount().GetLogin(),
	}, nil
}

// MintInstallationToken creates a short-lived installation access token for the
// given installation ID. The token is not returned to callers that only need to
// verify it can be minted; downstream use (e.g. pushing to an MCP as a Secret)
// is handled elsewhere.
func (c *Client) MintInstallationToken(ctx context.Context, installationID int64) (string, error) {
	tok, _, err := c.gh.Apps.CreateInstallationToken(ctx, installationID, nil)
	if err != nil {
		return "", fmt.Errorf("minting installation token: %w", err)
	}
	return tok.GetToken(), nil
}
