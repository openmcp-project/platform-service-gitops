# Connecting a Project to GitHub via a GitHub App

This guide explains how to wire up GitHub App authentication so that
`GitRepository` resources can resolve credentials. There are two roles:

- **Platform owner** — sets up the GitHub App once (creates the App, stores its
  credentials in a Secret, and creates a cluster-scoped `GitHubInstance`).
- **End user** — creates an `AppInstallation` and a `GitRepository` in their
  Project namespace. They never handle the App private key.

## Resource overview

```
Secret (platform-owner)        appID + privateKey + url
   ▲
   │ referenced by
GitHubInstance (cluster-scoped, platform-owner)
   ▲
   │ instanceRef
AppInstallation (namespaced, end-user)   -> checks App is installed on the org
   ▲
   │ credentialRef
GitRepository (namespaced, end-user)     -> mints a scoped token to verify access
```

- `github.gitops.open-control-plane.io/v1alpha1`: `GitHubInstance`, `AppInstallation`
- `gitops.open-control-plane.io/v1alpha1`: `GitRepository`

## Step 1 — Platform owner: create the App and credential Secret

1. Create a GitHub App (on github.com or your GitHub Enterprise Server) and
   generate a private key (`.pem`).
2. Store the App ID, private key and instance URL in a Secret in the controller's
   credential namespace (default `platform-service-gitops-system`). The Secret
   name must match a `secretRefs` entry on the `GitHubInstance`.

```bash
kubectl -n platform-service-gitops-system create secret generic sap-ghe \
  --from-literal=appID=<APP_ID> \
  --from-literal=url=https://github.tools.sap \
  --from-file=privateKey=./app-private-key.pem
```

Public github.com uses `url=https://github.com`; anything else is treated as a
GitHub Enterprise Server instance automatically.

## Step 2 — Platform owner: create the GitHubInstance

Cluster-scoped, references the Secret(s). See
[examples/githubinstance.yaml](examples/githubinstance.yaml).

```bash
kubectl apply -f docs/gitops-github/examples/githubinstance.yaml
```

The controller sets `Ready=True` once every referenced Secret exists and is
well-formed.

## Step 3 — End user: create an AppInstallation

Namespaced (Project namespace). The user must first install the platform's App
into their own GitHub org, then create the `AppInstallation`. See
[examples/appinstallation.yaml](examples/appinstallation.yaml).

```bash
kubectl apply -f docs/gitops-github/examples/appinstallation.yaml
```

The controller checks the App installation and reports:

- `AppInstalled=True` / `AccessVerified=True` — App is installed on the org.
- `AppInstalled=False` (reason `AppNotInstalled`) — install the App first; the
  controller waits without error-looping.

## Step 4 — End user: create a GitRepository

Namespaced, references the `AppInstallation` via `credentialRef`. See
[examples/gitrepository.yaml](examples/gitrepository.yaml).

```bash
kubectl apply -f docs/gitops-github/examples/gitrepository.yaml
```

The controller resolves the `AppInstallation`, mints a scoped installation token
to prove access (the token is not stored here), and sets `Ready=True`.

## Status quick reference

| Resource | Condition | Meaning |
|----------|-----------|---------|
| GitHubInstance | `Ready` | all referenced Secrets present & valid |
| AppInstallation | `AppInstalled` | App installed on the org/user |
| AppInstallation | `AccessVerified` | App authenticated successfully |
| GitRepository | `CredentialResolved` | AppInstallation resolved + token minted |
| GitRepository | `Ready` | credentials resolved, access verified |
