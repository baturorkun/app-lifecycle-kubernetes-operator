# Copilot Instructions for app-lifecycle-kubernetes-operator

## Project Overview
- **Purpose:** Kubernetes operator to manage application lifecycle in namespaces (freeze/resume Deployments/StatefulSets) for cost, maintenance, and state consistency.
- **Core CRD:** `NamespaceLifecyclePolicy` (see `api/v1alpha1/namespacelifecyclepolicy_types.go`)
- **Controller logic:** `internal/controller/namespacelifecyclepolicy_controller.go`

## Architecture & Data Flow
- **Freeze:** Scales target workloads to 0, stores original replica count in annotation.
- **Resume:** Restores replicas from annotation, removes annotation.
- **Startup Policy:** On operator start, policies are processed by `startupPolicy`, `startupResumePriority`, and `startupResumeDelay`.
- **Adaptive Throttling:** Batch resume, slow/pause based on cluster health (CPU/mem, pending pods, restarts, node readiness). Metrics can be sourced from kubelet or custom endpoints (see `linux/metrics.py`).
- **Pod Balancing:** Optionally triggers rolling restarts to rebalance pods when new nodes join.

## Developer Workflows
- **Install CRDs:** `make install`
- **Run locally:** `make run` (add `DEBUG=true` for verbose logs)
- **Deploy to cluster:** `make deploy IMG=<your-image>`
- **Test:** `make test` (unit tests), see also `test/e2e/`
- **Generate code after API changes:** `make generate && make manifests && make install`
- **Sample policies:** `config/samples/`
- **Test scripts:** `kubernetes/test-throttling/` (see its README for usage)

## Project Conventions
- **Idempotency:** All operations are idempotent, keyed by `operationId`.
- **Status tracking:** All actions update CRD status fields (phase, message, lastHandledOperationId, etc).
- **Label selectors:** Use `selector` in policy spec to target specific workloads.
- **Grace periods:** Override shutdown times via `terminationGracePeriodSeconds` in policy spec.
- **Startup ordering:** Use `startupResumePriority` and `startupResumeDelay` for controlled resume.
- **Node readiness:** Use `startupNodeReadinessPolicy` to wait for nodes before resuming.
- **Custom metrics:** For RKE2 or restricted clusters, run `linux/metrics.py` as a node service and configure `adaptiveThrottling.signalChecks.checkNodeUsage.scrape`.

## Integration Points
- **Kubernetes API:** Operator watches/patches Deployments, StatefulSets, and custom CRDs.
- **Metrics:** Can use kubelet stats or custom HTTP endpoints (see `linux/metrics.py`).
- **RBAC:** Ensure correct permissions for all managed resources (see `config/rbac/`).

## Debugging & Troubleshooting
- **Logs:** Use `DEBUG=true` for detailed logs. Log subsystems: `adaptive`, `node-event`, `startup-check`.
- **Check CRD status:** `kubectl get/describe namespacelifecyclepolicy -A`
- **Common issues:**
  - Policy not taking effect: check `operationId`, namespace existence, logs
  - Metrics 0: check kubelet stats or custom metrics endpoint
  - RBAC: verify permissions in `config/rbac/`

## Key Files & Directories
- `api/v1alpha1/`: CRD types
- `internal/controller/`: Controller logic
- `config/`: CRDs, RBAC, samples
- `linux/metrics.py`: Custom metrics server for node usage
- `kubernetes/test-throttling/`: Test scripts for policy scenarios

---

For more, see the main `README.md` and per-directory READMEs. When in doubt, follow the patterns in the sample policies and controller code.
