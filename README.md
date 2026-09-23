# gitea-k8s-runner-plugin

Run every Gitea Actions job as its own Kubernetes pod. This project gives [Gitea's runner](https://gitea.com/gitea/runner) Forgejo-style **backend plugins** and ships a **Kubernetes backend plugin**, plus a Helm chart that deploys both.

Without it, a Gitea runner on Kubernetes needs a privileged Docker-in-Docker sidecar, runs jobs one dockerd at a time, holds resources while idle, and hides jobs from Kubernetes. With it:

- Each workflow job is a `batch/v1` Job built from a **podspec you choose per label** (`runs-on: large` → the `large` class podspec): requests and limits, node selectors, tolerations, `schedulerName`, `priorityClassName`, image pull secrets and extra sidecars all apply.
- Jobs wait in `Pending` like any other pod, so a single runner with a high `capacity` scales with the cluster, and Kubernetes (quotas, schedulers, autoscalers) sees every job.
- `services:` become native sidecars in the job pod, reachable on `localhost` and by service name, and `container:` swaps the job image.
- `actions/checkout`, JavaScript and composite actions, `actions/cache`, artifacts and `docker build` (on a class with a dind sidecar) work unchanged.

> Status: pre-release. The protocol is `plugin.v1alpha`, wire-compatible with [Forgejo runner](https://code.forgejo.org/forgejo/runner)'s backend plugins.

## How it works

```
┌──────────────────────── runner pod (Deployment, 1 replica) ────────────────────────┐
│  gitea-runner (patched)                          k8s plugin (native sidecar)       │
│   · polls Gitea, claims tasks                     · gRPC on a unix socket          │
│   · resolves actions, evaluates expressions  ◀──▶ · Create/Start → batch/v1 Job    │
│   · sequences steps, streams logs                 · Exec → pods/exec into `main`   │
│   · cache server (reachable via a Service)        · CopyIn/Out → tar over exec     │
│                                                   · Remove → delete the Job        │
└───────────────────────────────────────────────────────────────┬────────────────────┘
                                                                ▼
                    Job pod: `main` (sleeps; steps are exec'd into it)
                    + services as native sidecars + podspec extras (e.g. dind)
```

The runner does everything a runner does (claiming tasks, fetching actions, evaluating expressions, sequencing steps, streaming logs). Only the container operations go to the plugin: create and start an environment, exec a command, copy files in and out, remove it.

The repository has four parts:

| Path | What |
|---|---|
| [`runner/`](runner/) | A small patch series against `gitea/runner` v3.5.0 adding `plugins:` config, `<label>:<plugin>:<arg>` routing and a gRPC-backed execution environment. Nothing in it is Kubernetes-specific, so it can go upstream as generic backend plugins. |
| [`plugin/`](plugin/) | The Kubernetes backend plugin. |
| [`charts/gitea-runner-k8s/`](charts/gitea-runner-k8s/) | The Helm chart, built on the [bjw-s common library](https://github.com/bjw-s-labs/helm-charts). |
| [`examples/`](examples/) | Chart values and workflows. CI installs these exact files in a kind cluster with a real Gitea and runs the workflows, so every example here is known to work. |

## Quick start

Requirements: Kubernetes 1.31 or newer (native sidecars), Helm 3.8+ or Helm 4, and Gitea with Actions enabled (CI tests against Gitea 1.27).

1. Get a runner registration token from Gitea: *Site Administration → Actions → Runners → Create new runner* (or a repository's or organization's *Settings → Actions → Runners*). On the Gitea host, `gitea actions generate-runner-token` also prints one.

2. Store it in a Secret:

   ```sh
   kubectl create namespace ci
   kubectl -n ci create secret generic gitea-runner-registration --from-literal=token=<token>
   ```

3. Install the chart with [`examples/values/minimal.yaml`](examples/values/minimal.yaml), setting your Gitea URL:

   ```sh
   helm install runner oci://ghcr.io/perfectra1n/charts/gitea-runner-k8s -n ci \
     -f https://raw.githubusercontent.com/perfectra1n/gitea-k8s-runner-plugin/main/examples/values/minimal.yaml \
     --set gitea.url=https://gitea.example.com
   ```

   The runner registers itself (the registration is kept on a PVC) and appears in Gitea with the labels `default`, `ubuntu-latest` and `ubuntu-24.04`.

4. Push a workflow to `.gitea/workflows/` in a repository, for example [`examples/workflows/hello.yaml`](examples/workflows/hello.yaml):

   ```yaml
   name: basics
   on: [push]
   jobs:
     basics:
       runs-on: ubuntu-latest
       steps:
         - uses: actions/checkout@v4
         - run: echo "running in pod $HOSTNAME"
   ```

   Watch the job pod come and go with `kubectl -n ci get pods -w`.

## Job classes

A **class** is a named podspec. The chart turns each entry of `classes` into a ConfigMap mounted into the plugin and a runner label `<name>:k8s:/podspecs/<name>/podspec.yaml` (plus one per alias), so `runs-on: <name>` or any alias runs on that podspec.

```yaml
classes:
  - name: ubuntu
    aliases: [ubuntu-latest, ubuntu-24.04]
    podspec:
      containers:
        - name: main
          image: ghcr.io/catthehacker/ubuntu:act-24.04
          resources:
            requests: {cpu: 500m, memory: 1Gi}
            limits: {memory: 4Gi}
```

What the plugin does with a podspec:

- **`main`** is the container the steps run in. Its command is replaced by a sleep loop and steps arrive over `pods/exec`, so it needs `/bin/sh`, `env` and `tar` (every runner image has them). If the podspec has no `main`, one is added with the job's `container:` image; a job's `container:` image always replaces `main`'s image.
- **Services** (`services:` in the workflow) are added as native sidecars named `svc-<id>` with their env and ports. They share the pod network, and a `hostAliases` entry makes each service id resolve to `127.0.0.1`, so both `localhost:5432` and `postgres:5432` work.
- **The workspace** lives on a volume named `shared` mounted at `/__w`. Declare one yourself (for example an ephemeral PVC, as in the `large` class of [`examples/values/classes.yaml`](examples/values/classes.yaml)); otherwise a 10Gi `emptyDir` is used.
- **Everything else passes through**: `nodeSelector`, affinity, tolerations, `schedulerName`, `priorityClassName`, `imagePullSecrets`, `serviceAccountName`, security contexts and extra containers such as a dind sidecar.
- **Job settings**: `restartPolicy: Never`, `backoffLimit: 0`, `activeDeadlineSeconds` from the runner's job timeout and `ttlSecondsAfterFinished: 300` as a safety net.

Tested examples:

| File | Shows |
|---|---|
| [`examples/values/minimal.yaml`](examples/values/minimal.yaml) | The smallest install: one default class, jobs in the release namespace. |
| [`examples/values/classes.yaml`](examples/values/classes.yaml) | Several classes, jobs in their own namespace, a per-job PVC workspace, a Docker-in-Docker class, plugin options. The end-to-end suite installs exactly this file. |
| [`examples/values/scheduling.yaml`](examples/values/scheduling.yaml) | Placement: node selectors, tolerations, priority classes, pull secrets, extra pod labels. |

## Workflows

Workflows are written as for GitHub Actions. Each example below runs in CI against a real cluster:

| Workflow | Shows |
|---|---|
| [`hello.yaml`](examples/workflows/hello.yaml) | The quick-start workflow above. |
| [`basics.yaml`](examples/workflows/basics.yaml) | `actions/checkout`, a JavaScript action from GitHub, a local composite action ([`examples/actions/greet`](examples/actions/greet/action.yml)). |
| [`services.yaml`](examples/workflows/services.yaml) | A Postgres service reached on `localhost` and by its name. |
| [`container.yaml`](examples/workflows/container.yaml) | `container:` running the job on `alpine`. |
| [`cache.yaml`](examples/workflows/cache.yaml) | `actions/cache` saving in one job and restoring in the next. |
| [`artifacts.yaml`](examples/workflows/artifacts.yaml) | `actions/upload-artifact` on the PVC-backed `large` class. |
| [`docker-build.yaml`](examples/workflows/docker-build.yaml) | `docker build` and `docker run` on the `dind` class. |

Not supported on the plugin backend: `uses: docker://…` steps and Docker/Dockerfile actions (there is no Docker daemon); they fail with `docker actions are not supported on the k8s backend plugin`. Steps can still run `docker` against a dind sidecar, as in `docker-build.yaml`. Windows and macOS jobs are out of scope.

## Configuration

### Chart

The full list of values is in the [chart README](charts/gitea-runner-k8s/README.md). The chart is a thin layer over the bjw-s common library, so any bjw-s key (`controllers`, `persistence`, `defaultPodOptions`, …) can be set directly and wins over what the chart generates, for example `--set controllers.main.pod.priorityClassName=system-cluster-critical`.

### Plugin options

Set under `plugin.options` in the chart (they become `plugins.k8s.options` in the runner config and reach the plugin with every job):

| Option | Default | Meaning |
|---|---|---|
| `ready_timeout` | `10m` | How long a job pod may take to become ready, including time `Pending` under a capacity-aware scheduler. On expiry the job fails with the pod's last condition and events, and the Job is deleted. |
| `image_pull_policy` | `IfNotPresent` | Pull policy of `main` and service containers. |
| `service_resources` | none | Default requests/limits for service sidecars, as YAML. |
| `labels` | none | Extra labels on job pods, `k=v,k=v`; `${ENV_ID}` expands to the job's environment id. |
| `namespace` | the plugin's `--namespace` | Where Jobs are created (the chart sets it from `jobNamespace`). |
| `podspec` | none | Podspec for a plugin label without an argument. |

### Without the chart

The patched runner reads plugins from its `config.yaml`, and a label's middle segment names the plugin:

```yaml
plugins:
  k8s:
    address: unix:///run/plugin/plugin.sock   # or tcp://host:port
    options:
      ready_timeout: 15m
runner:
  labels:
    - "small:k8s:/podspecs/small/podspec.yaml"
```

Run the plugin next to it with a ServiceAccount allowed to manage Jobs and exec into pods in the job namespace (see the Role the chart renders):

```sh
gitea-k8s-runner-plugin --listen unix:///run/plugin/plugin.sock --namespace ci-jobs --instance my-runner
```

`--instance` labels every Job the plugin creates. Keep it stable across restarts: at startup the plugin deletes Jobs with its instance label that a previous process left behind. On `SIGTERM` it deletes the Jobs of every live job.

## Failure behaviour

Every failure shows up in the job log rather than hanging: a pod that does not become ready within `ready_timeout` (with its last condition and events), an image pull failing for over a minute, a crash-looping service (with its last 50 log lines), a pod evicted or deleted mid-job (naming its phase and reason), and an unreachable plugin (`plugin k8s at unix:///…: …`). The runner always removes the environment when a job ends or is cancelled; `activeDeadlineSeconds` and `ttlSecondsAfterFinished` bound leaks even if the runner and plugin both die.

## Compatibility with Forgejo

The protocol uses Forgejo's package, service, RPC and message names and field numbers, so this plugin also serves `forgejo-runner` (13.1+), and Forgejo backend plugins work with the patched runner. See [docs/compatibility.md](docs/compatibility.md).

## Development

[mise](https://mise.jdx.dev) manages every toolchain and task, locally and in CI:

```sh
mise install                    # pinned tools (.mise/config.toml, locked in .mise/mise.lock)
mise tasks                      # everything below, with descriptions
mise run ci                     # the hermetic gate: fmt, vet, tidy, codegen drift, lint, unit tests,
                                # the upstream runner suite with our patches, chart lint + assertions
mise run //test/e2e:test        # kind + Gitea end-to-end suite (isolated KUBECONFIG under .build/e2e)
mise run //test/e2e:cluster-delete
```

Working on the runner patches:

```sh
mise run runner:apply           # fresh gitea/runner v3.5.0 clone in .build/runner with the series applied
# edit, test and commit in .build/runner, then:
mise run runner:refresh         # rewrite runner/patches/ from the clone's commits
```

Releases are cut by [release-please](https://github.com/googleapis/release-please): merging its release PR tags the version and publishes the images `ghcr.io/perfectra1n/gitea-k8s-runner-plugin` and `ghcr.io/perfectra1n/gitea-runner-k8s` and the chart `oci://ghcr.io/perfectra1n/charts/gitea-runner-k8s`.

## Provenance and licence

MIT, see [LICENSE](LICENSE). The code here is written independently. It is compatible with Forgejo's protocol at the interface level only (names and field numbers); no code from `forgejo/runner` or other GPL projects is used. The patched `gitea/runner` is MIT.
