# SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
# SPDX-License-Identifier: Apache-2.0
#
# hack/dev.mk — local development helpers for a disposable kind cluster.
#
# This file is intentionally SEPARATE from the root Makefile (which is driven by
# the hack/common submodule / kubebuilder scaffolding). It adds a fast
# inner-loop: spin up a kind cluster, install the CRDs, seed example resources,
# and run the controller out-of-cluster against it.
#
# Usage (run from the repo root):
#
#   make -f hack/dev.mk dev          # create cluster, install CRDs, run controller
#   make -f hack/dev.mk dev-seed     # (re)apply the sample Secret + CRs only
#   make -f hack/dev.mk dev-clean    # delete the kind cluster
#
# The controller runs on your host (via the root Makefile's `run` target) using
# the kubeconfig context of the kind cluster, so it can reach the credential
# Secret you apply below. Set a real App private key in the seed Secret to test
# against github.tools.sap.

ROOT_MAKEFILE ?= Makefile
KIND          ?= kind
KUBECTL       ?= kubectl

# Dedicated dev cluster, kept separate from the e2e cluster in the root Makefile.
DEV_CLUSTER        ?= platform-service-gitops-dev
DEV_KUBE_CONTEXT   := kind-$(DEV_CLUSTER)
CREDENTIAL_NS      ?= platform-service-gitops-system

.PHONY: dev dev-cluster dev-seed dev-run dev-clean

## dev: create a kind cluster, install CRDs, seed samples, and run the controller.
dev: dev-cluster dev-seed dev-run

## dev-cluster: create the kind cluster (idempotent) and install the CRDs.
dev-cluster:
	@if ! $(KIND) get clusters | grep -q '^$(DEV_CLUSTER)$$'; then \
		echo "Creating kind cluster $(DEV_CLUSTER)"; \
		$(KIND) create cluster --name=$(DEV_CLUSTER); \
	else \
		echo "kind cluster $(DEV_CLUSTER) already exists"; \
	fi
	@$(KUBECTL) cluster-info --context $(DEV_KUBE_CONTEXT)
	@echo "Installing CRDs"
	@$(MAKE) -f $(ROOT_MAKEFILE) install

## dev-seed: create the credential namespace and apply the example Secret + CRs.
dev-seed:
	@echo "Ensuring namespace $(CREDENTIAL_NS)"
	@$(KUBECTL) --context $(DEV_KUBE_CONTEXT) create namespace $(CREDENTIAL_NS) \
		--dry-run=client -o yaml | $(KUBECTL) --context $(DEV_KUBE_CONTEXT) apply -f -
	@echo "Applying sample GitHubInstance + credential Secret"
	@$(KUBECTL) --context $(DEV_KUBE_CONTEXT) apply -f config/samples/github_v1alpha1_githubinstance.yaml
	@echo "Applying sample AppInstallation (namespace my-project)"
	@$(KUBECTL) --context $(DEV_KUBE_CONTEXT) create namespace my-project \
		--dry-run=client -o yaml | $(KUBECTL) --context $(DEV_KUBE_CONTEXT) apply -f -
	@$(KUBECTL) --context $(DEV_KUBE_CONTEXT) apply -f config/samples/github_v1alpha1_appinstallation.yaml
	@echo
	@echo "NOTE: edit the privateKey in config/samples/github_v1alpha1_githubinstance.yaml"
	@echo "      with a real GitHub App key before expecting AppInstalled=True."

## dev-run: run the controller out-of-cluster against the kind cluster.
dev-run:
	@echo "Running controller against $(DEV_KUBE_CONTEXT) (Ctrl-C to stop)"
	@$(KUBECTL) config use-context $(DEV_KUBE_CONTEXT) >/dev/null
	@$(MAKE) -f $(ROOT_MAKEFILE) run

## dev-clean: delete the kind cluster.
dev-clean:
	@echo "Deleting kind cluster $(DEV_CLUSTER)"
	@$(KIND) delete cluster --name=$(DEV_CLUSTER)
