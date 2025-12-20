# Installation Guide

This guide covers installation, deployment, and distribution options for the App Lifecycle Kubernetes Operator.

## Prerequisites

- go version v1.24.6+
- docker version 17.03+
- kubectl version v1.11.3+
- Access to a Kubernetes v1.11.3+ cluster

## Installation Methods

### Method 1: Deploy from Source (Recommended for Development)

**1. Install CRDs into the cluster:**

```sh
make install
```

**2. Run the operator locally:**

```sh
make run
```

This will run the operator on your local machine, connecting to your current kubectl context.

### Method 2: Deploy to Cluster

**1. Build and push your image:**

```sh
make docker-build docker-push IMG=<some-registry>/app-lifecycle-kubernetes-operator:tag
```

**NOTE:** This image ought to be published in a personal registry you specified.
Make sure you have the proper permission to the registry if the above commands don't work.

**2. Install the CRDs:**

```sh
make install
```

**3. Deploy the operator to the cluster:**

```sh
make deploy IMG=<some-registry>/app-lifecycle-kubernetes-operator:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
> privileges or be logged in as admin.

**4. Create sample resources:**

```sh
kubectl apply -k config/samples/
```

> **NOTE**: Ensure that the samples have default values to test it out.

### Method 3: Install via YAML Bundle

**1. Build the installer:**

```sh
make build-installer IMG=<some-registry>/app-lifecycle-kubernetes-operator:tag
```

This generates an `install.yaml` file in the `dist` directory containing all resources.

**2. Apply the bundle:**

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/app-lifecycle-kubernetes-operator/<tag or branch>/dist/install.yaml
```

Or from local file:

```sh
kubectl apply -f dist/install.yaml
```

### Method 4: Install via Helm Chart

**1. Generate the Helm chart:**

```sh
kubebuilder edit --plugins=helm/v2-alpha
```

**2. Install from the chart:**

The chart will be generated under `dist/chart/`.

```sh
helm install app-lifecycle-operator ./dist/chart
```

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the `--force` flag and manually ensure that any custom configuration
previously added to `dist/chart/values.yaml` or `dist/chart/manager/manager.yaml`
is manually re-applied afterwards.

## Verification

After installation, verify the operator is running:

```sh
# Check if CRDs are installed
kubectl get crd namespacelifecyclepolicies.apps.ops.dev

# Check operator pod (if deployed to cluster)
kubectl get pods -n app-lifecycle-kubernetes-operator-system

# Check operator logs
kubectl logs -n app-lifecycle-kubernetes-operator-system <pod-name>
```

## Uninstallation

### If Deployed to Cluster

**1. Delete sample resources:**

```sh
kubectl delete -k config/samples/
```

**2. Undeploy the controller:**

```sh
make undeploy
```

**3. Delete the CRDs:**

```sh
make uninstall
```

### If Installed via YAML Bundle

```sh
kubectl delete -f dist/install.yaml
```

### If Installed via Helm

```sh
helm uninstall app-lifecycle-operator
```

Then delete CRDs:

```sh
kubectl delete crd namespacelifecyclepolicies.apps.ops.dev
```

## Development Workflow

### Running Locally

For development, it's easiest to run the operator locally:

```sh
# Install CRDs
make install

# Run operator locally (connects to current kubectl context)
make run
```

### Making Changes

After modifying the API (types.go):

```sh
# Update generated code
make generate

# Update CRD manifests
make manifests

# Re-install CRDs
make install
```

### Testing

```sh
# Run tests
make test

# Run with verbose output
make test ARGS="-v"
```

### Building and Pushing Images

```sh
# Build container image
make docker-build IMG=<your-registry>/app-lifecycle-kubernetes-operator:tag

# Push to registry
make docker-push IMG=<your-registry>/app-lifecycle-kubernetes-operator:tag

# Or do both in one command
make docker-build docker-push IMG=<your-registry>/app-lifecycle-kubernetes-operator:tag
```

## Project Distribution

### Option 1: YAML Bundle

Build a single YAML file containing all resources:

```sh
make build-installer IMG=<registry>/app-lifecycle-kubernetes-operator:tag
```

This creates `dist/install.yaml` which can be distributed to users:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/app-lifecycle-kubernetes-operator/<version>/dist/install.yaml
```

### Option 2: Helm Chart

Generate a Helm chart for distribution:

```sh
kubebuilder edit --plugins=helm/v2-alpha
```

The chart will be available in `dist/chart/` and can be:
- Published to a Helm repository
- Packaged and distributed as a `.tgz` file
- Installed directly from the directory

### Option 3: Container Image

Push the operator image to a public registry:

```sh
# Docker Hub
make docker-build docker-push IMG=dockerhub-org/app-lifecycle-kubernetes-operator:v1.0.0

# GitHub Container Registry
make docker-build docker-push IMG=ghcr.io/org/app-lifecycle-kubernetes-operator:v1.0.0

# Quay.io
make docker-build docker-push IMG=quay.io/org/app-lifecycle-kubernetes-operator:v1.0.0
```

Then users can deploy with:

```sh
make deploy IMG=<your-published-image>
```

## Troubleshooting Installation

### RBAC Errors

If you see permission errors during `make deploy`:

```sh
# Grant yourself cluster-admin (use with caution)
kubectl create clusterrolebinding cluster-admin-binding \
  --clusterrole=cluster-admin \
  --user=<your-email>
```

### CRD Already Exists

If CRD installation fails because it already exists:

```sh
# Delete old CRD
kubectl delete crd namespacelifecyclepolicies.apps.ops.dev

# Re-install
make install
```

### Image Pull Errors

If the operator pod can't pull the image:

1. Verify the image exists in your registry
2. Check registry permissions
3. Create image pull secret if using private registry:

```sh
kubectl create secret docker-registry regcred \
  --docker-server=<registry> \
  --docker-username=<username> \
  --docker-password=<password> \
  --docker-email=<email> \
  -n app-lifecycle-kubernetes-operator-system
```

Then patch the deployment to use the secret.

### Webhook Certificate Errors

If using webhooks (not currently implemented), certificate errors may occur.
The operator uses cert-manager for webhook certificates. Ensure cert-manager is installed:

```sh
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.13.0/cert-manager.yaml
```

## Additional Resources

- [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)
- [Kubernetes Operator Pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
- [Custom Resource Definitions](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/)

## Getting Help

Run `make help` for a list of all available make targets:

```sh
make help
```

For issues and questions, please [open an issue](https://github.com/your-org/app-lifecycle-kubernetes-operator/issues) on GitHub.
