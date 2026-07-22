# Examples

End-to-end example resources for the GitHub App GitOps flow. These mirror the
data used during testing (org `cloud-orchestration`, instance `sap-ghe`, repo
`gitops-hackaton-test`); only the Secret carries dummy values.

Apply in order (the numbers reflect the dependency chain):

| File | Role | Notes |
|------|------|-------|
| `01-secret.yaml` | platform owner | **dummy** appID + private key — replace before use |
| `02-githubinstance.yaml` | platform owner | cluster-scoped, references the Secret |
| `03-appinstallation.yaml` | end user | Project namespace, references the GitHubInstance |
| `04-gitrepository.yaml` | end user | Project namespace, references the AppInstallation |

```sh
# platform owner (once)
kubectl create namespace platform-service-gitops-system
kubectl apply -f examples/01-secret.yaml          # after filling in real values
kubectl apply -f examples/02-githubinstance.yaml

# end user
kubectl create namespace my-project
kubectl apply -f examples/03-appinstallation.yaml
kubectl apply -f examples/04-gitrepository.yaml
```

To test against a real GitHub App, replace the dummy `appID` and `privateKey` in
`01-secret.yaml`, or create the Secret from a `.pem` directly:

```sh
kubectl -n platform-service-gitops-system create secret generic sap-ghe \
  --from-literal=appID=<APP_ID> \
  --from-literal=url=https://github.tools.sap \
  --from-file=privateKey=./app-private-key.pem
```

See [docs/gitops-github](../docs/gitops-github/README.md) for the full guide.
