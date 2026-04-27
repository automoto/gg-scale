#!/usr/bin/env sh
# Apply the pinned Agones install manifest and wait for the controller to roll
# out. Invoked by the `agones-install` compose service (k8s profile).
set -eu

KUBECONFIG="${KUBECONFIG:-/kube/kubeconfig.yaml}"
export KUBECONFIG

echo "applying Agones install manifest..."
kubectl apply --server-side --force-conflicts -f /manifests/agones-install.yaml

echo "waiting for agones-system namespace to settle..."
kubectl -n agones-system rollout status deploy/agones-controller --timeout=180s
kubectl -n agones-system rollout status deploy/agones-allocator --timeout=180s

echo "Agones install complete."
