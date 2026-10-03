#!/usr/bin/env bash
# Enforce security invariants on the rendered Helm chart:
#   - no wildcard RBAC, and no access to Secrets, ConfigMaps or Pods
#   - hardened container and pod security contexts
#   - no ":latest" image references
#   - every rendered SpinAppExecutor is a kubeswift-spin executor
set -euo pipefail
cd "$(dirname "$0")/.."

render() { helm template kubeswift-spin charts/kubeswift-spin --namespace kubeswift-spin-system "$@"; }

check() { python3 hack/check_chart_policy.py; }

render | check
render --set metrics.secure=true --set metrics.serviceMonitor.enabled=true | check
render --set image.digest=sha256:0000000000000000000000000000000000000000000000000000000000000000 \
       --set runtimeImage.digest=sha256:1111111111111111111111111111111111111111111111111111111111111111 | check
