# Local Testing Guide

## Prerequisites

- `ocpctl`, `kind`, `kubectl`, `docker` installed
- Access to `github.tools.sap` with the GitHub App credentials

## 1. Spin up the environment

```bash
ocpctl env apply test-gitops
```

This creates two kind clusters: `test-gitops-platform` and `test-gitops-onboarding`.

Add the onboarding kubeconfig (run once after env is ready):

```bash
kind get kubeconfig --name test-gitops-onboarding > /tmp/onboarding.kubeconfig
KUBECONFIG=~/.kube/config:/tmp/onboarding.kubeconfig kubectl config view --flatten > /tmp/merged && mv /tmp/merged ~/.kube/config
```

## 2. Build and load the image

```bash
make docker-build IMG=platform-service-gitops:dev
kind load docker-image platform-service-gitops:dev --name test-gitops-platform
```

## 3. Deploy the controller

```bash
cd config/manager && kustomize edit set image controller=platform-service-gitops:dev && cd ../..
kubectl config use-context kind-test-gitops-platform
kubectl apply -k config/default
```

The `init-crds` init container runs first and installs CRDs on the correct clusters:
- `GitHubInstance` → platform cluster
- `AppInstallation`, `GitRepository`, `Kustomization` → onboarding cluster

## 4. Apply test resources

**Platform cluster** (Secret + GitHubInstance):
```bash
kubectl config use-context kind-test-gitops-platform
kubectl apply -f examples/01-secret.yaml       # fill in real appID + privateKey first
kubectl apply -f examples/02-githubinstance.yaml
```

**Onboarding cluster** (AppInstallation + ControlPlane + GitRepository):
```bash
kubectl config use-context kind-test-gitops-onboarding
kubectl create namespace my-project --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f examples/03-appinstallation.yaml
kubectl apply -f examples/04-gitrepository.yaml  # add propagateTo (see below)
```

Create the ControlPlane the GitRepository propagates to (see
`examples/06-controlplane.yaml`):

```yaml
# examples/06-controlplane.yaml
apiVersion: core.open-control-plane.io/v2alpha1
kind: ControlPlane
metadata:
  name: test-mcp
  namespace: my-project
spec:
  iam: {}
```

```bash
kubectl apply -f examples/06-controlplane.yaml
# Wait until it is provisioned:
kubectl -n my-project get controlplane test-mcp -o wide   # PHASE -> Ready
```

Add `propagateTo` to the GitRepository (or include it in `examples/04-gitrepository.yaml`):

```yaml
  propagateTo:
    - kind: ControlPlane
      name: test-mcp
```

## 5. Verify

```bash
# AppInstallation should show installed=true
kubectl config use-context kind-test-gitops-onboarding
kubectl -n my-project get appinstallation my-connection

# GitRepository should show Ready + tokenExpiresAt
kubectl -n my-project get gitrepository gitops-hackaton-test -o jsonpath='{.status.propagated}' | jq .
```

Add the MCP context once the ControlPlane is provisioned:

```bash
MCP_CLUSTER=$(kubectl config use-context kind-test-gitops-platform && \
  kubectl get cluster -A --no-headers | grep "mcp-" | awk '{print $2}')
kind get kubeconfig --name "$MCP_CLUSTER" > /tmp/mcp.kubeconfig
KUBECONFIG=~/.kube/config:/tmp/mcp.kubeconfig kubectl config view --flatten > /tmp/merged && mv /tmp/merged ~/.kube/config
kubectl config rename-context "kind-$MCP_CLUSTER" kind-test-mcp
```

```bash
# flux-system namespace must exist on the MCP before the token Secret is written
kubectl config use-context kind-test-mcp
kubectl create namespace flux-system

# Flux source CRDs must also be installed on the MCP so the controller can create
# the Flux GitRepository (source.toolkit.fluxcd.io/v1). A freshly provisioned MCP
# may not have them yet:
kubectl apply -f https://github.com/fluxcd/source-controller/releases/latest/download/source-controller.crds.yaml

# Token Secret should appear after next reconcile (~10s)
kubectl -n flux-system get secret gitops-hackaton-test-token
```

## 5b. Verify the kind:Secret (bring-your-own credential) path

The Secret path also propagates to MCPs (`examples/05-gitrepository-secret-pat.yaml`),
copying your credential Secret in verbatim instead of minting a token.

```bash
kubectl config use-context kind-test-gitops-onboarding
# Fill in a real PAT/username first (verify it: git ls-remote https://USER:PAT@host/org/repo)
kubectl apply -f examples/05-gitrepository-secret-pat.yaml

# Per-MCP status: expect phase=Ready, reason=SecretSynced (tokenExpiresAt is null)
kubectl -n my-project get gitrepository my-infra -o jsonpath='{.status.propagated}' | jq .

# On the MCP: the credential Secret is copied verbatim as <name>-credentials,
# and a Flux GitRepository references it.
kubectl config use-context kind-test-mcp
kubectl -n flux-system get secret my-infra-credentials -o jsonpath='{.data}' | jq 'keys'   # ["password","username"]
kubectl -n flux-system get gitrepository.source.toolkit.fluxcd.io my-infra -o jsonpath='{.spec.secretRef.name}'
```

If `status.propagated` shows `FluxFailed` / `SecretCopyFailed` mentioning
`flux-system` or an unknown `GitRepository` kind, the MCP is missing the
namespace or the Flux source CRDs — apply them (see the block above) and the next
reconcile will succeed.

## 6. Tear down

```bash
ocpctl env delete test-gitops
```
