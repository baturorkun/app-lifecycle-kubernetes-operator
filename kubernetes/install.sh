#!/usr/bin/env bash
set -euo pipefail

# Install the operator as a pod into the local Kind cluster.
# Usage:
#   ./install.sh           — build image, load into Kind, deploy
#   ./install.sh --delete  — delete the operator deployment only
#   ./install.sh --clean   — delete the operator AND uninstall CRDs

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

KIND_CLUSTER="${KIND_CLUSTER:-dev}"
IMAGE_NAME="app-lifecycle-operator"
IMAGE_TAG="local"
# Podman tags images with "localhost/" prefix. Use the full form so containerd
# on Kind nodes resolves it locally instead of trying docker.io.
IMAGE="localhost/${IMAGE_NAME}:${IMAGE_TAG}"

# ── helpers ──────────────────────────────────────────────────────────────────
info()  { echo "▶ $*"; }
ok()    { echo "✅ $*"; }
err()   { echo "❌ $*" >&2; exit 1; }

require() {
  command -v "$1" &>/dev/null || err "'$1' is required but not found"
}

# ── delete / clean ────────────────────────────────────────────────────────────
if [[ "${1:-}" == "--delete" ]]; then
  info "Deleting operator deployment..."
  kubectl delete deployment app-lifecycle-controller-manager -n app-lifecycle-operator --ignore-not-found
  ok "Deployment deleted."
  exit 0
fi

if [[ "${1:-}" == "--clean" ]]; then
  info "Deleting operator deployment and CRDs..."
  kubectl delete deployment app-lifecycle-controller-manager -n app-lifecycle-operator --ignore-not-found
  kubectl delete crd namespacelifecyclepolicies.apps.ops.dev --ignore-not-found
  ok "Operator and CRDs removed."
  exit 0
fi

# ── preflight ─────────────────────────────────────────────────────────────────
require podman
require kind
require kubectl
require kustomize

# Verify the Kind cluster is running
kind get clusters 2>/dev/null | grep -q "^${KIND_CLUSTER}$" \
  || err "Kind cluster '${KIND_CLUSTER}' not found. Create it first."

# ── 1. build image with podman ────────────────────────────────────────────────
info "Building image ${IMAGE}..."
cd "$REPO_DIR"
podman build -t "${IMAGE}" .
ok "Image built: ${IMAGE}"

# ── 2. load image into Kind ───────────────────────────────────────────────────
info "Loading image into Kind cluster '${KIND_CLUSTER}'..."
# podman save → kind load (kind can read OCI archives from stdin)
podman save "${IMAGE}" -o /tmp/operator-image.tar
kind load image-archive /tmp/operator-image.tar --name "${KIND_CLUSTER}"
rm -f /tmp/operator-image.tar
ok "Image loaded into Kind cluster"

# ── 3. install / update CRDs ─────────────────────────────────────────────────
info "Installing CRDs..."
kustomize build "$REPO_DIR/config/crd" | kubectl apply -f -
ok "CRDs installed"

# ── 4. deploy operator ────────────────────────────────────────────────────────
info "Deploying operator with image ${IMAGE}..."

# Patch kustomization to use our local image, build manifests, then restore
KUSTOMIZE_MANAGER="$REPO_DIR/config/manager"
ORIG_KUSTOMIZATION="$KUSTOMIZE_MANAGER/kustomization.yaml"
BACKUP_KUSTOMIZATION="$KUSTOMIZE_MANAGER/kustomization.yaml.bak"

cp "$ORIG_KUSTOMIZATION" "$BACKUP_KUSTOMIZATION"
trap 'mv "$BACKUP_KUSTOMIZATION" "$ORIG_KUSTOMIZATION"' EXIT

# Point kustomize at our local image with imagePullPolicy Never
cd "$KUSTOMIZE_MANAGER"
kustomize edit set image "controller=${IMAGE}"

# Build and apply the full config/default, then patch imagePullPolicy
kustomize build "$REPO_DIR/config/default" \
  | sed 's|imagePullPolicy:.*|imagePullPolicy: Never|g' \
  | kubectl apply -f -

ok "Operator deployed"

# ── 5. wait for rollout ───────────────────────────────────────────────────────
info "Waiting for operator pod to be ready..."
kubectl rollout status deployment/app-lifecycle-controller-manager \
  -n app-lifecycle-operator \
  --timeout=120s
ok "Operator is running"

echo ""
echo "📋 Pod status:"
kubectl get pods -n app-lifecycle-operator
echo ""
echo "📋 To follow logs:"
echo "   kubectl logs -n app-lifecycle-operator -l control-plane=app-lifecycle-controller-manager -f"
