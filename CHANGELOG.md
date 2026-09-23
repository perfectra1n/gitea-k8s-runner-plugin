# Changelog

## 0.1.0 (2026-09-23)


### Features

* **chart:** gitea-runner-k8s Helm chart on the bjw-s common library ([5b92a29](https://github.com/perfectra1n/gitea-k8s-runner-plugin/commit/5b92a29d948ef7d3ffc122ea63da63e88847017b))
* **plugin:** kubernetes Job lifecycle and gRPC server ([f29035d](https://github.com/perfectra1n/gitea-k8s-runner-plugin/commit/f29035dd0c17df44a870c147cf810e6b433c6a7a))
* **plugin:** options and podspec-to-Job builder ([176e499](https://github.com/perfectra1n/gitea-k8s-runner-plugin/commit/176e4995861e03bb2da9b0328323545db9ef3183))
* **proto:** plugin.v1alpha protocol, wire-compatible with Forgejo ([43c3bf8](https://github.com/perfectra1n/gitea-k8s-runner-plugin/commit/43c3bf8eba38e98829e1b12ba7909f9ce77f65fa))
* **runner:** patch 0001 act/plugin gRPC backend client ([95112a7](https://github.com/perfectra1n/gitea-k8s-runner-plugin/commit/95112a704cdfa73a4279bf548332a0bb49cc49f7))
* **runner:** patches 0002-0004 plugin config, labels, run-context backend ([fbbe9f5](https://github.com/perfectra1n/gitea-k8s-runner-plugin/commit/fbbe9f5ab641279d35e692a9c61a86bde7eecb59))
* **runner:** ship the .proto, a make target and user docs in the patches ([7504d95](https://github.com/perfectra1n/gitea-k8s-runner-plugin/commit/7504d955d1436e2fa3a087301df9ec357f32b828))


### Bug Fixes

* **plugin:** safe shutdown, owner-checked removes, whole-tree kills ([aef26ca](https://github.com/perfectra1n/gitea-k8s-runner-plugin/commit/aef26cab394e5a27eee9202097e805e26aa20aa3))


### Documentation

* README, compatibility notes, CI and release-please ([18d951d](https://github.com/perfectra1n/gitea-k8s-runner-plugin/commit/18d951dc71e7175c7d273dc15f721632245d3412))


### Tests

* **e2e:** kind + Gitea end-to-end suite running the tested examples ([5b7593c](https://github.com/perfectra1n/gitea-k8s-runner-plugin/commit/5b7593c338d64069c8cc01b2f1d7edede4d8cc2b))


### Build System

* **runner:** patch-series tooling against gitea/runner v3.5.0 ([b697047](https://github.com/perfectra1n/gitea-k8s-runner-plugin/commit/b6970470cb6edb22aafc9f4d3cd48fce623c5b56))


### Continuous Integration

* let the two golangci-lint runs overlap ([b518b82](https://github.com/perfectra1n/gitea-k8s-runner-plugin/commit/b518b82f97664206504a9b013d026d84c30bc252))
* one job per gate family ([1f83b8c](https://github.com/perfectra1n/gitea-k8s-runner-plugin/commit/1f83b8c226ef814155dcb1061d1f9941e2f06b5d))
* **release:** label images with this repository as their source ([cd5da8f](https://github.com/perfectra1n/gitea-k8s-runner-plugin/commit/cd5da8febb6460a8b8e2e88299ae115ff22ccc4c))
* **release:** start at 0.1.0 ([fc0be02](https://github.com/perfectra1n/gitea-k8s-runner-plugin/commit/fc0be02a85dd178616c0dc288311eea1fc0dc36f))
* run upstream's suite in the environment it assumes ([0f2c6c3](https://github.com/perfectra1n/gitea-k8s-runner-plugin/commit/0f2c6c35526bd991482ae3647dc054acff76a579))
