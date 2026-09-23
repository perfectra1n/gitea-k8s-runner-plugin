# Runner patches

[`patches/`](patches/) is a `git format-patch` series against [`gitea/runner`](https://gitea.com/gitea/runner) at the tag in [`UPSTREAM`](UPSTREAM). It adds generic out-of-process backend plugins; nothing in it is Kubernetes-specific.

| Patch | Adds |
|---|---|
| 0001 `act/plugin` | A client for the `plugin.v1alpha` gRPC protocol (connect-go in gRPC mode, which upstream already depends on) and `plugin.Environment`, a `container.ExecutionsEnvironment` backed by it. Includes the `.proto` (a copy of this repository's, differing only in `go_package`), its generated code with a `make generate-plugin-proto` target, and `plugintest`, an in-process fake plugin. |
| 0002 `act/runner` | A third job environment next to host and Docker: when `Config.BackendPicker` picks a plugin for the job's `runs-on`, the job image, services and label argument go to the plugin. `docker://` and Dockerfile actions fail with a clear error. |
| 0003 `config`, `labels` | The top-level `plugins:` config and labels of the form `<name>:<plugin>[:<arg>]` for configured plugins. A label whose middle segment is not a configured plugin stays opaque, as before. |
| 0004 `run` | Wiring: labels are parsed with the configured plugins, registered and declared by name, and jobs are routed to the plugin of the first matching `runs-on` label. Adds the user documentation, `docs/backend-plugins.md`. |

Each patch carries its own tests, and the full upstream suite runs with the series applied (`mise run runner:test`), as does upstream's own lint configuration (`mise run runner:lint`).

## Working on the patches

```sh
mise run runner:apply     # fresh clone of UPSTREAM in .build/runner, series applied
cd .build/runner          # edit, `go test ./...`, commit (one logical change per commit)
mise run runner:refresh   # regenerate patches/ from the clone's commits
mise run runner:test      # re-apply to a fresh clone and run upstream's suite
```

`runner:apply` refuses to discard uncommitted work or commits that are not in `patches/` yet (use `FORCE=1` to discard them). After changing `proto/`, run `mise run runner:gen`: it copies the `.proto` into the clone (rewriting `go_package`) and runs upstream's `make generate-plugin-proto`. Commit the result there and refresh; `mise run runner:gen-check` fails when the two differ.

Upstream tests that fail on the pristine tag on a particular host (for example on NixOS, where `/bin/sh` is bash) can be skipped locally with `RUNNER_TEST_SKIP`, a `go test -skip` regex, in the gitignored `.mise/config.local.toml`. CI runs everything.

## Upstreaming

The series follows upstream's contribution conventions (Conventional Commits, copyright headers, upstream's lint config) so it can be proposed as generic backend plugin support, alongside the discussion in [gitea/runner#31](https://gitea.com/gitea/runner/issues/31). It has not been submitted yet.

## Image

`mise run image-runner` builds upstream's own Dockerfile (`basic` target: `tini`, `run.sh`, registration through `GITEA_*` environment variables) from the patched clone, published as `ghcr.io/perfectra1n/gitea-runner-k8s`.
