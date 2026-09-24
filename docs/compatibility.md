# Compatibility with Forgejo backend plugins

The protocol in [`proto/plugin/v1alpha/plugin.proto`](../proto/plugin/v1alpha/plugin.proto) is package `plugin.v1alpha`, service `BackendPlugin`, with the same RPC names, message names, field numbers and reserved ranges as Forgejo runner's backend plugin protocol. Two implementations that agree on those interoperate on the wire, whatever language or gRPC library they use.

## What is verified automatically

- **Field numbers and names.** [`gen/plugin/v1alpha/wire_test.go`](../gen/plugin/v1alpha/wire_test.go) pins the encoded tag bytes of the fields that matter for compatibility, the reserved ranges, and the full method names (`/plugin.v1alpha.BackendPlugin/<Rpc>`). A renumbering fails `mise run test`, and `mise run proto-breaking` runs `buf breaking` (wire rules) against `main`.
- **Different gRPC stacks on each side.** The plugin server uses grpc-go. The runner patches use connect-go in gRPC mode over h2c (upstream `gitea/runner` already depends on connect). The end-to-end suite (`mise run //test/e2e:test`) runs every example workflow through that pairing, so the two stacks are exercised against each other on every CI run.
- **The protocol in the runner patches** is the same `.proto` with only `go_package` changed, generated with upstream's own `make generate-plugin-proto`; `mise run runner:gen-check` fails if the two drift.

## Semantics this implementation follows

- Per environment the runner issues one RPC at a time; a cancelled stream (job cancelled, step timed out) may be followed immediately by `Remove`. The plugin serialises RPCs per environment and, when an `Exec` stream is cancelled, kills the running command and every process it started.
- `Exec` ends with `ExecComplete{exit_code}` when the command ran, whatever its exit code, and with `ExecFailed{error_message}` only when it could not be run (pod gone, exec transport broken, unsupported `user`).
- `CopyIn`: only the first chunk carries `environment_id` and `dest_path`; later chunks that set them are rejected with `InvalidArgument`. The destination directory is created if missing.
- `CopyOut` streams a tar whose top-level entry is the base name of `src_path`, matching `docker cp`; a missing path is `NotFound`.
- `Remove` is idempotent, including for environment ids the plugin process never saw: after a plugin restart it deletes the Job by name, but only if the Job carries this plugin instance's labels, so instances sharing a namespace never remove each other's jobs.
- The plugin serves `grpc.health.v1` alongside `BackendPlugin`.
- `StartComplete.image_env` carries the job container's environment. As on the docker backend, the runner uses it to extend `PATH`; steps inherit the rest from the container itself. `CreateResponse.default_path_variable` is therefore only a last resort, for images without `PATH`.
- `CreateResponse` is answered before the pod exists, and the runner exports its `tool_cache_path` as `RUNNER_TOOL_CACHE` over the image's own value. So the plugin always reports `/__w/_tool` and, at `Start`, makes it a symlink to the image's tool cache (`RUNNER_TOOL_CACHE`, else `AGENT_TOOLSDIRECTORY`) when that is an absolute, writable directory.

## Cross-implementation smoke tests

These are run by hand and recorded here, since they need a Forgejo installation or a third-party plugin that CI does not provision.

| Pairing | Result |
|---|---|
| Patched Gitea runner + this plugin | Passing in CI (end-to-end suite). |
| Patched Gitea runner + another `plugin.v1alpha` plugin | Not yet recorded. |
| `forgejo-runner` (13.1+) + this plugin | Not yet recorded. |

To record one, point the other side at the plugin's socket (for this plugin: `gitea-k8s-runner-plugin --listen tcp://0.0.0.0:7070 --namespace <ns> --instance smoke --kubeconfig <file>`), run a workflow with checkout, a `run:` step, a service and a failing step, and note the runner version, plugin version and outcome in the table.
