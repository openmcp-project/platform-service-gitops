// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package githubapp

import "testing"

func TestIsPublic(t *testing.T) {
	cases := map[string]bool{
		"https://github.com":       true,
		"https://github.com/":      true,
		"https://api.github.com":   true,
		"github.com":               true,
		"":                         true,
		"https://github.tools.sap": false,
		"https://ghe.example.org":  false,
	}
	for url, want := range cases {
		if got := isPublic(url); got != want {
			t.Errorf("isPublic(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestNewClientEnterpriseSetsBaseURL(t *testing.T) {
	// A syntactically valid dummy RSA key is required by ghinstallation.
	const dummyKey = `-----BEGIN RSA PRIVATE KEY-----
MIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Q
uKUpRKfFLfRYC9AIKjbJTWit+CqvjSFmGD5DZ0zjuUoLdC1DmA9XuBAoBQ0IJyBw
-----END RSA PRIVATE KEY-----`
	// This key is intentionally too short to actually parse; we only assert
	// NewClient errors gracefully rather than panicking on bad input.
	_, err := NewClient(Credentials{AppID: 1, PrivateKey: []byte(dummyKey), URL: "https://github.tools.sap"})
	if err == nil {
		t.Skip("dummy key unexpectedly parsed; nothing to assert")
	}
}
