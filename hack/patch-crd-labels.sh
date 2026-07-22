#!/usr/bin/env python3
"""Patch openmcp.cloud/cluster labels into CRD manifests after controller-gen regeneration.

controller-gen does not preserve hand-edited metadata fields; this script re-applies
the cluster routing labels that determine which cluster each CRD is installed on.

Usage: hack/patch-crd-labels.sh <manifests-dir>
"""

import sys
import os
import re

LABELS = {
    "gitops.open-control-plane.io_gitrepositories": "onboarding",
    "gitops.open-control-plane.io_kustomizations": "onboarding",
    "github.gitops.open-control-plane.io_appinstallations": "onboarding",
    "github.gitops.open-control-plane.io_githubinstances": "platform",
}

def patch_file(path, cluster):
    with open(path) as f:
        content = f.read()

    # Already patched — skip
    if "openmcp.cloud/cluster" in content:
        return

    # Insert labels block after the controller-gen annotation line
    patched = re.sub(
        r'(    controller-gen\.kubebuilder\.io/version: [^\n]+\n)',
        r'\1  labels:\n    openmcp.cloud/cluster: ' + cluster + '\n',
        content,
        count=1,
    )

    if patched == content:
        print(f"WARNING: could not patch {path}", file=sys.stderr)
        return

    with open(path, "w") as f:
        f.write(patched)
    print(f"Patched {path} -> cluster={cluster}")


manifests_dir = sys.argv[1] if len(sys.argv) > 1 else "api/crds/manifests"

for filename in os.listdir(manifests_dir):
    if not filename.endswith(".yaml"):
        continue
    # derive CRD name from filename: strip leading "---" separator to get <crd>.yaml
    crd_name = filename.replace(".yaml", "")
    if crd_name in LABELS:
        patch_file(os.path.join(manifests_dir, filename), LABELS[crd_name])
