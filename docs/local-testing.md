# Local Testing Guide

## Prerequisites

- `ocpctl`, `kind`, `kubectl`, `docker` installed
- Access to `github.tools.sap` with the GitHub App credentials

## 1. Spin up the environment

```bash
ocpctl env apply test-gitops
```

This creates two kind clusters: `test-gitops-platform` and `test-gitops-onboarding`.

Add the onboarding and MCP kubeconfigs (run once after env is ready):

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
kubectl apply -f ghapp-check/testing/01-secret.yaml
kubectl apply -f ghapp-check/testing/02-githubinstance.yaml
```

**Onboarding cluster** (AppInstallation + ControlPlane + GitRepository):
```bash
kubectl config use-context kind-test-gitops-onboarding
kubectl create namespace my-project --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f ghapp-check/testing/03-appinstallation.yaml
kubectl apply -f ghapp-check/testing/04-controlplane.yaml
kubectl apply -f ghapp-check/testing/05-gitrepository.yaml
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

# Token Secret should appear after next reconcile
kubectl -n flux-system get secret gitops-hackaton-test-token
```

## 6. Tear down

```bash
ocpctl env delete test-gitops
```
