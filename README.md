# gitea-k8s-runner-plugin

Forgejo-style backend plugins for [Gitea's runner](https://gitea.com/gitea/runner),
plus a Kubernetes backend that runs every workflow job as its own pod.

> Status: pre-release, under construction. See
> [the design](docs/superpowers/specs/2026-09-23-gitea-k8s-runner-plugin-design.md).

## Development

Every toolchain and task is managed by [mise](https://mise.jdx.dev):

```sh
mise install     # pinned toolchains (.mise/config.toml, locked in .mise/mise.lock)
mise tasks       # list tasks
mise run ci      # the full local hermetic gate
```

## Licence

MIT — see [LICENSE](LICENSE).
