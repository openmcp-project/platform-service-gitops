# Design: GitHubInstance + AppInstallation CRDs (GitHub App connectivity)

**Date:** 2026-07-21
**Issue:** [openmcp/internal-backlog#539](https://github.tools.sap/openmcp/internal-backlog/issues/539)
**Epic:** [openmcp/internal-backlog#536](https://github.tools.sap/openmcp/internal-backlog/issues/536)
**Repo:** `platform-service-gitops`

## Goal

Introduce the GitHub App authentication layer so that `GitRepository` resources
can resolve credentials via `credentialRef`. Two new CRDs in a new API group
`github.gitops.open-control-plane.io/v1alpha1` (Go path `api/github/v1alpha1/`):

1. **`GitHubInstance`** (cluster-scoped) — Platform-Owner-managed. Points at one
   or more Kubernetes Secrets that each hold GitHub App credentials
   (`appID`, `privateKey`, `url`). Represents the platform's connection to a
   GitHub instance (public github.com or a GitHub Enterprise Server).
2. **`AppInstallation`** (namespaced) — End-user-managed. References a
   `GitHubInstance` and names an org (or user). The controller verifies the
   platform's GitHub App is installed on that org and reports the result in
   `.status`.

## Role separation (the core idea)

- **Platform Owner** (once): creates the GitHub App, creates the credential
  Secret(s), creates the `GitHubInstance`. The end-user never handles the
  private key.
- **End User**: adds the platform's App to their own GitHub org, then creates an
  `AppInstallation` referencing the `GitHubInstance`. The controller checks that
  the App is actually installed on the named org.

## API group

- kubebuilder group: `github`, version `v1alpha1`, kind `GitHubInstance` and
  `AppInstallation`.
- Full API group overridden in `groupversion_info.go` to
  `github.gitops.open-control-plane.io` (mirrors how the existing `core` group
  overrides its group to `gitops.open-control-plane.io`). This matches the
  `credentialRef.group` already used by `GitRepository`.

## Credential Secret (Platform-Owner-managed)

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: sap-ghe
  namespace: <controller-namespace>   # TBD — configurable, default e.g. platform-service-gitops-system
stringData:
  appID:      "1436"
  privateKey: |
    -----BEGIN RSA PRIVATE KEY-----
    ...
  url:        https://github.tools.sap  # public would be https://github.com
```

- **URL lives in the Secret** (not in the CR). public vs. Enterprise is derived
  automatically from the URL: `github.com` / `api.github.com` → public, anything
  else → Enterprise (go-github `WithEnterpriseURLs` + ghinstallation `BaseURL`
  set to `<url>/api/v3`).
- Secret keys: `appID`, `privateKey`, `url`.

## GitHubInstance (cluster-scoped)

```yaml
apiVersion: github.gitops.open-control-plane.io/v1alpha1
kind: GitHubInstance
metadata:
  name: sap-ghe
spec:
  secretRefs:                     # list of credential secrets
    - name: sap-ghe
      namespace: <controller-namespace>   # optional; defaults to controller ns
status:
  conditions:
    - type: Ready                 # all referenced secrets present & well-formed
    - type: CredentialsValid      # App auth (GET /app) succeeds for each secret
```

- `spec.secretRefs` is a **list** — a single `GitHubInstance` may carry multiple
  App credentials / hosts.
- Controller validates each secret exists and has the required keys, and
  (optionally) that each App authenticates (`GET /app`). Sets conditions.

## AppInstallation (namespaced)

```yaml
apiVersion: github.gitops.open-control-plane.io/v1alpha1
kind: AppInstallation
metadata:
  name: my-connection
  namespace: my-project
spec:
  instanceRef:
    name: sap-ghe                 # which GitHubInstance
  credentialName: sap-ghe         # which secret within the instance's secretRefs
                                  # optional if the instance lists exactly one
  org: cloud-orchestration        # OR:
  # user: D067641
status:
  installationID: 16282
  conditions:
    - type: AppInstalled          # AppFound / AppNotInstalled
    - type: AccessVerified        # AccessVerified / AccessDenied
```

- `credentialName` selects one secret from the instance's `secretRefs`. If the
  instance lists exactly one secret, `credentialName` is optional.
- Exactly one of `org` / `user` must be set (validated).

## Status conditions (per #539)

Modeled with `metav1.Condition` + `meta.SetStatusCondition` (matches existing
`GitRepository` reconciler style).

| Kind | Condition type | True reason | False reason |
|------|----------------|-------------|--------------|
| AppInstallation | `AppInstalled` | `AppFound` | `AppNotInstalled` |
| AppInstallation | `AccessVerified` | `AccessVerified` | `AccessDenied` |
| GitHubInstance | `Ready` | secrets resolved | `SecretNotFound` / `SecretMalformed` |

- **No error-loop on "not installed"**: a `404` from
  `GetOrganizationInstallation` maps to `AppInstalled=False` (reason
  `AppNotInstalled`), returns `nil` error + requeue — matching the #539 AC.

## GitHub client logic (reused from tested CLI)

The verified `ghapp-check` CLI logic moves into an internal package
`internal/github/` (or `pkg/github/`):

- Deps: `github.com/google/go-github/v88` + `github.com/bradleyfalzon/ghinstallation/v2`
  (added to `go.mod`; not present yet).
- Flow: load `appID` + `privateKey` from Secret → `ghinstallation.NewAppsTransport…`
  (JWT) → build go-github client (`WithEnterpriseURLs` if URL is not github.com,
  and set `atr.BaseURL = <url>/api/v3`) → `Apps.Get(ctx, "")` (auth check) →
  `Apps.GetOrganizationInstallation` / `GetUserInstallation` (installation check).
- **Verified end-to-end** against github.tools.sap with App 1436 (installation
  16282), including installation-token minting.

## Reconciler wiring

- `internal/controller/github/appinstallation_controller.go`
- `internal/controller/github/githubinstance_controller.go`
- Registered in `cmd/main.go` (both `SetupWithManager`), scheme registration for
  the new group.
- RBAC markers: own CRDs (get/list/watch/update/patch + status), plus
  `secrets` (get/list/watch) in the credential namespace.
- `AppInstallation` `.Watches()` its `GitHubInstance` (and the credential
  Secrets) so changes re-trigger reconciliation, not just `RequeueAfter`.
- Periodic re-check via `RequeueAfter` (installation state can change out-of-band).

## Follow-up wiring (out of scope for this task, noted)

- `GitRepository` reconciler placeholder (`gitrepository_controller.go:62`,
  "until AppInstallation type lands") will later resolve `credentialRef` →
  `AppInstallation`. Not implemented here.
- `propagateTo` scoped-token generation per MCP — separate issue.

## Constraints / project rules (from AGENTS.md)

- Scaffold via `kubebuilder create api --group github --version v1alpha1 --kind …`.
  **Do not** hand-create scaffold files.
- After type/marker edits: `make manifests` + `make generate`.
- Never edit `zz_generated.*`, `config/crd/bases/*`, `config/rbac/role.yaml`,
  `PROJECT` by hand.
- SPDX header on every new Go file (matches existing repo files):
  `// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors`
  `// SPDX-License-Identifier: Apache-2.0`
- Conditions use `metav1.Condition`; logging follows K8s message-style.
- Tests: Ginkgo + Gomega (envtest), matching existing `core` suite.

## Open items to confirm during implementation

- **Controller namespace** for default secret location — TBD, made configurable
  via a manager flag (e.g. `--credential-namespace`), default
  `platform-service-gitops-system`.
- Whether `GitHubInstance` should actively call `GET /app` on reconcile
  (`CredentialsValid`) or only structurally validate the secrets. Design assumes
  active check; cheap and gives real feedback.

## Acceptance mapping (#539)

| AC | Covered by |
|----|-----------|
| `AppInstallation` creatable in a namespace | `AppInstallation` (namespaced) |
| Controller checks App installed + access | `github` client `GetOrganizationInstallation` |
| `.status`: AppInstalled/AppNotInstalled/AccessVerified/AccessDenied | conditions above |
| No error-loop when not installed | 404 → condition, nil error + requeue |
| `GitRepository` resolves via `credentialRef` | group matches; resolution itself is follow-up |
