# gitea-runner-k8s

A Gitea Actions runner that runs every workflow job as its own Kubernetes pod, via the gitea-k8s-runner-plugin backend plugin.

The runner (a patched `gitea/runner` with backend plugin support) claims tasks from Gitea as usual. Each workflow job runs as its own `batch/v1` Job built from the podspec of the job class its `runs-on` label names. The Kubernetes backend plugin runs as a native sidecar next to the runner.

The chart is built on the [bjw-s common library](https://github.com/bjw-s-labs/helm-charts): the values below are turned into bjw-s values, and any bjw-s key you set (`controllers`, `persistence`, `defaultPodOptions`, ...) is merged on top.

## Install

```sh
helm install runner oci://ghcr.io/perfectra1n/charts/gitea-runner-k8s \
  --namespace ci --create-namespace \
  --set gitea.url=https://gitea.example.com \
  --set gitea.existingSecret=gitea-runner-token
```

## Examples

Tested examples live in the repository's [`examples/`](https://github.com/perfectra1n/gitea-k8s-runner-plugin/tree/main/examples) directory: every values file there is linted and rendered in CI, and the end-to-end suite installs [`examples/values/classes.yaml`](https://github.com/perfectra1n/gitea-k8s-runner-plugin/blob/main/examples/values/classes.yaml) in a kind cluster and runs the example workflows against a real Gitea.

## Job classes

Each entry of `classes` becomes a ConfigMap `<release>-podspec-<name>` and runner labels `<name>:k8s:/podspecs/<name>/podspec.yaml` plus one per alias. The podspec is a plain `corev1.PodSpec`. The container named `main` runs the steps and needs `/bin/sh`, `env` and `tar`; one is added with the job's `container:` image if the podspec has none. `schedulerName`, `priorityClassName`, affinity, `imagePullSecrets` and extra sidecars (e.g. a dind sidecar for `docker build`) pass through unchanged. A volume named `shared`, if declared, holds the workspace (for example an ephemeral PVC); otherwise a 10Gi `emptyDir` is used.

```yaml
classes:
  - name: dind
    podspec:
      containers:
        - name: main
          image: ghcr.io/catthehacker/ubuntu:act-24.04
          env: [{name: DOCKER_HOST, value: tcp://localhost:2375}]
      initContainers:
        - name: dind
          image: docker:dind
          restartPolicy: Always
          securityContext: {privileged: true}
          env: [{name: DOCKER_TLS_CERTDIR, value: ""}]
```

## Requirements

Kubernetes: `>=1.31.0-0`

The chart is built on the [bjw-s common library chart](https://github.com/bjw-s-labs/helm-charts/tree/main/charts/library/common); `Chart.yaml` pins its version.

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| cache.enabled | bool | `true` | Run the runner's cache server (actions/cache) and expose it to job pods. |
| cache.port | int | `8088` | Port of the cache server and its Service. |
| classes | list | one `default` class on the act runner image | Job classes. Each class becomes a podspec ConfigMap mounted into the plugin and runner labels `<name>:k8s:<path>` (plus one per alias), so `runs-on: <name>` runs on that podspec. The podspec is a corev1.PodSpec; a container named `main` runs the steps and needs /bin/sh, env and tar. |
| clusterDomain | string | `"cluster.local"` | Kubernetes cluster domain, used for the cache server address. |
| data | object | `{"accessMode":"ReadWriteOnce","existingClaim":"","size":"5Gi","storageClass":null}` | Runner state (.runner registration file, cache blobs). |
| data.existingClaim | string | `""` | Existing PVC to use instead of creating one. |
| data.storageClass | string | `nil` | Storage class of the created PVC. |
| gitea.existingSecret | string | `""` | Existing Secret holding the registration token. |
| gitea.existingSecretKey | string | `"token"` | Key of the token in `gitea.existingSecret`. |
| gitea.token | string | `""` | Runner registration token. Ignored when `gitea.existingSecret` is set. |
| gitea.url | string | `""` | URL of the Gitea instance, e.g. https://gitea.example.com |
| global | object | `{}` | bjw-s global settings (fullnameOverride, labels, ...). |
| jobNamespace | string | `""` | Namespace where job pods run. Defaults to the release namespace. The chart creates the Role and RoleBinding there. |
| plugin.image.pullPolicy | string | `"IfNotPresent"` |  |
| plugin.image.repository | string | `"ghcr.io/perfectra1n/gitea-k8s-runner-plugin"` | The Kubernetes backend plugin image. |
| plugin.image.tag | string | `""` | Defaults to the chart appVersion. |
| plugin.options | object | `{}` | Extra plugin options passed with every job (ready_timeout, image_pull_policy, service_resources, labels). |
| plugin.resources | object | `{}` | Resources of the plugin sidecar. |
| runner.capacity | int | `10` | Jobs run concurrently. Pods sit Pending under the cluster scheduler, so this can be high. |
| runner.config | object | `{}` | Extra runner config merged into the generated config.yaml. |
| runner.extraLabels | list | `[]` | Extra labels that do not use the plugin (e.g. "self-hosted:host"). |
| runner.image.pullPolicy | string | `"IfNotPresent"` |  |
| runner.image.repository | string | `"ghcr.io/perfectra1n/gitea-runner-k8s"` | Patched gitea/runner image with backend plugin support. |
| runner.image.tag | string | `""` | Defaults to the chart appVersion. |
| runner.logLevel | string | `"info"` | Log level of the runner. |
| runner.name | string | `""` | Runner name shown in Gitea. Defaults to the release fullname. |
| runner.resources | object | `{}` | Resources of the runner container. |
| runner.timeout | string | `"3h"` | Maximum job duration; also bounds each job pod (activeDeadlineSeconds). |
