#!/bin/sh
# Render the chart with tests/values-test.yaml and assert its wiring with yq.
# Run from the repository root: sh charts/gitea-runner-k8s/tests/assert.sh
set -eu

chart=charts/gitea-runner-k8s
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
fails=0

render() { # name, extra helm args...
  name=$1
  shift
  helm template r "$chart" -n ci-runner "$@" >"$out/$name.yaml"
}

check() { # description, file, yq expression that must print "true"
  if [ "$(yq ea "$3" "$2")" = "true" ]; then
    echo "ok   $1"
  else
    echo "FAIL $1"
    fails=$((fails + 1))
  fi
}

must_fail() { # description, expected error substring, helm args...
  desc=$1
  want=$2
  shift 2
  if err=$(helm template r "$chart" -n ci-runner "$@" 2>&1 >/dev/null); then
    echo "FAIL $desc: rendered, want an error"
    fails=$((fails + 1))
  elif printf '%s' "$err" | grep -q "$want"; then
    echo "ok   $desc"
  else
    echo "FAIL $desc: error lacks '$want': $err"
    fails=$((fails + 1))
  fi
}

render main -f "$chart/tests/values-test.yaml"
f=$out/main.yaml
cfg='select(.kind == "ConfigMap" and .metadata.name == "r-gitea-runner-k8s-config") | .data["config.yaml"] | from_yaml'
deploy='select(.kind == "Deployment") | .spec.template.spec'

for class in small dind; do
  check "class $class: podspec ConfigMap" "$f" \
    "[select(.kind == \"ConfigMap\" and .metadata.name == \"r-gitea-runner-k8s-podspec-$class\") | .data[\"podspec.yaml\"] | from_yaml | .containers[0].name == \"main\"] | any"
  check "class $class: podspec mounted in the plugin" "$f" \
    "[$deploy | .initContainers[] | select(.name == \"plugin\") | .volumeMounts[] | select(.mountPath == \"/podspecs/$class\")] | length == 1"
  check "class $class: runner label" "$f" \
    "[$cfg | .runner.labels[] | select(. == \"$class:k8s:/podspecs/$class/podspec.yaml\")] | length == 1"
done
check "alias label routes to its class podspec" "$f" \
  "[$cfg | .runner.labels[] | select(. == \"ubuntu-latest:k8s:/podspecs/small/podspec.yaml\")] | length == 1"
check "podspec passes through (schedulerName, dind sidecar)" "$f" \
  '[select(.kind == "ConfigMap" and .metadata.name == "r-gitea-runner-k8s-podspec-dind") | .data["podspec.yaml"] | from_yaml | .initContainers[0].securityContext.privileged] | any'

check "Role in jobNamespace" "$f" '[select(.kind == "Role") | .metadata.namespace == "ci-jobs"] | all'
check "RoleBinding in jobNamespace binds the runner SA" "$f" \
  'select(.kind == "RoleBinding") | ((.metadata.namespace == "ci-jobs") and (.roleRef.name == "r-gitea-runner-k8s-jobs") and (.subjects[0].name == "r-gitea-runner-k8s") and (.subjects[0].namespace == "ci-runner"))'
check "Role allows jobs, pods/exec and events" "$f" \
  '[select(.kind == "Role") | .rules[] | .resources[]] | (contains(["jobs"]) and contains(["pods/exec"]) and contains(["events"]))'
check "pod uses the bound ServiceAccount" "$f" "[$deploy | .serviceAccountName == \"r-gitea-runner-k8s\"] | all"

check "cache Service exists" "$f" '[select(.kind == "Service" and .metadata.name == "r-gitea-runner-k8s-cache") | .spec.ports[0].port == 8088] | length == 1'
check "cache.host points at the Service" "$f" "[$cfg | .cache.host == \"r-gitea-runner-k8s-cache.ci-runner.svc.cluster.local\"] | all"

check "plugin is a native sidecar" "$f" "[$deploy | .initContainers[] | select(.name == \"plugin\") | .restartPolicy == \"Always\"] | all"
check "plugin watches jobNamespace with a stable instance" "$f" \
  "[$deploy | .initContainers[] | select(.name == \"plugin\") | (.args | contains([\"--namespace=ci-jobs\", \"--instance=r-gitea-runner-k8s\"]))] | all"
check "socket shared by runner and plugin" "$f" \
  "[$deploy | ((.containers[] | select(.name == \"main\")), (.initContainers[] | select(.name == \"plugin\"))) | .volumeMounts[] | select(.name == \"socket\")] | length == 2"
check "runner config points at the plugin socket" "$f" "[$cfg | .plugins.k8s.address == \"unix:///run/plugin/plugin.sock\"] | all"
check "single replica, Recreate" "$f" 'select(.kind == "Deployment") | ((.spec.replicas == 1) and (.spec.strategy.type == "Recreate"))'
check "registration token from the generated Secret" "$f" \
  "[$deploy | .containers[0].env[] | select(.name == \"GITEA_RUNNER_REGISTRATION_TOKEN\") | .valueFrom.secretKeyRef.name == \"r-gitea-runner-k8s-registration\"] | all"

render existing --set gitea.url=https://g --set gitea.existingSecret=tok --set gitea.existingSecretKey=t
check "existingSecret: no Secret rendered" "$out/existing.yaml" '[select(.kind == "Secret")] | length == 0'
check "existingSecret: token read from it" "$out/existing.yaml" \
  'select(.kind == "Deployment") | .spec.template.spec.containers[0].env[] | select(.name == "GITEA_RUNNER_REGISTRATION_TOKEN") | .valueFrom.secretKeyRef | ((.name == "tok") and (.key == "t"))'
check "jobNamespace defaults to the release namespace" "$out/existing.yaml" '[select(.kind == "Role") | .metadata.namespace == "ci-runner"] | all'
check "default class on the act image" "$out/existing.yaml" \
  '[select(.kind == "ConfigMap" and .metadata.name == "r-gitea-runner-k8s-config") | .data["config.yaml"] | from_yaml | .runner.labels[] | select(. == "ubuntu-latest:k8s:/podspecs/default/podspec.yaml")] | length == 1'

render override -f "$chart/tests/values-test.yaml" --set controllers.main.pod.priorityClassName=ci-runner
check "bjw-s keys set by the user win" "$out/override.yaml" '[select(.kind == "Deployment") | .spec.template.spec.priorityClassName == "ci-runner"] | all'

must_fail "gitea.url required" "gitea.url is required" --set gitea.token=x
must_fail "token or existingSecret required" "gitea.existingSecret" --set gitea.url=https://g
must_fail "class names are DNS labels" "not a valid class name" --set gitea.url=https://g --set gitea.token=x --set 'classes[0].name=Bad_Name'

if [ "$fails" -ne 0 ]; then
  echo "$fails chart assertion(s) failed" >&2
  exit 1
fi
