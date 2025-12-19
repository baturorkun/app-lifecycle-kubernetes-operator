# app-lifecycle-kubernetes-operator
This operator manages the lifecycle of applications within a namespace, ensuring graceful shutdown and controlled, balanced startup of Deployments and StatefulSets during events like node maintenance.

## Description
The App Lifecycle Kubernetes Operator provides a robust mechanism for managing application lifecycles at the namespace level. When a node needs to be drained for maintenance or other reasons, simply cordoning the node is often not enough to ensure zero downtime or graceful handling of stateful applications. This operator introduces the `NamespaceLifecyclePolicy` Custom Resource Definition (CRD) to address this challenge. By creating a `NamespaceLifecyclePolicy` resource, you can define strategies for gracefully terminating Deployments and StatefulSets within a specific namespace. The operator watches for these policies and, when triggered (e.g., by a node event or manual annotation), orchestrates a graceful shutdown of the workloads. Subsequently, it ensures that the pods are rescheduled and started in a controlled manner, aiming for a balanced distribution across the available nodes in the cluster. This prevents "thundering herd" problems and ensures high availability for your applications.

## Getting Started

### Prerequisites
- go version v1.24.6+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/app-lifecycle-kubernetes-operator:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/app-lifecycle-kubernetes-operator:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/app-lifecycle-kubernetes-operator:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/app-lifecycle-kubernetes-operator/<tag or branch>/dist/install.yaml
```

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
kubebuilder edit --plugins=helm/v2-alpha
```

2. See that a chart was generated under 'dist/chart', and users
can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

## Contributing
We welcome contributions from the community! If you'd like to contribute to this project, please follow these guidelines:

1.  **Reporting Bugs:** If you find a bug, please open an issue in the GitHub repository. Include a clear title, a detailed description of the issue, steps to reproduce it, and any relevant logs or error messages.
2.  **Suggesting Enhancements:** For new features or improvements, please open an issue to discuss your ideas before submitting a pull request. This allows us to align on the design and implementation.
3.  **Pull Requests:**
    *   Fork the repository and create a new branch for your feature or bug fix.
    *   Ensure your code follows the existing style and conventions.
    *   Add or update tests as appropriate.
    *   Make sure all tests pass by running `make test`.
    *   Update the documentation (`README.md`, etc.) if your changes affect it.
    *   Submit a pull request with a clear description of your changes.

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
