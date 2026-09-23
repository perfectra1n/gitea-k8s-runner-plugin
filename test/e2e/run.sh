#!/bin/sh
# Push the example workflows (examples/workflows) plus the e2e-only ones
# (test/e2e/workflows) to a repository on the in-cluster Gitea, wait for every
# run, and assert the outcomes. Expects the stack from `mise run
# //test/e2e:install` and an isolated KUBECONFIG (the mise tasks set it).
set -eu

: "${KUBECONFIG:?KUBECONFIG must point at the e2e cluster}"
root=$(cd "$(dirname "$0")/../.." && pwd)
port=${GITEA_LOCAL_PORT:-13300}
gitea="http://127.0.0.1:$port"
user=e2e
pass=e2e-password-1234
repo=$user/workflows
jobs_ns=ci-jobs
runner_ns=ci-runner
timeout_s=${E2E_TIMEOUT:-1800}
work=$(mktemp -d)

kubectl -n gitea port-forward svc/gitea "$port:3000" >"$work/port-forward.log" 2>&1 &
pf=$!
cleanup() {
  kill "$pf" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

log() { printf '==> %s\n' "$*"; }
api() { # method path [json body]
  if [ $# -ge 3 ]; then
    curl -sfS -u "$user:$pass" -X "$1" -H 'Content-Type: application/json' --data-binary "$3" "$gitea/api/v1$2"
  else
    curl -sfS -u "$user:$pass" -X "$1" "$gitea/api/v1$2"
  fi
}
json() { yq -p json -o json "$@"; }

diagnostics() {
  set +e
  echo "::group::diagnostics"
  kubectl get jobs,pods -n "$jobs_ns" -o wide
  kubectl get events -n "$jobs_ns" --sort-by=.lastTimestamp | tail -40
  kubectl -n "$runner_ns" logs deploy/runner-gitea-runner-k8s -c main --tail=200
  kubectl -n "$runner_ns" logs deploy/runner-gitea-runner-k8s -c plugin --tail=200
  api GET "/repos/$repo/actions/jobs?limit=100" | json '.jobs[] | select(.conclusion != "success") | {"id": .id, "name": .name, "status": .status, "conclusion": .conclusion}'
  for id in $(api GET "/repos/$repo/actions/jobs?limit=100" | json -r '.jobs[] | select(.conclusion == "failure") | .id'); do
    echo "--- logs of job $id"
    api GET "/repos/$repo/actions/jobs/$id/logs" | tail -60
  done
  echo "::endgroup::"
}
fail() {
  echo "FAIL: $*" >&2
  diagnostics
  exit 1
}

log "waiting for Gitea on $gitea"
for _ in $(seq 60); do
  curl -sf "$gitea/api/healthz" >/dev/null 2>&1 && break
  sleep 2
done
curl -sf "$gitea/api/healthz" >/dev/null || fail "Gitea is not reachable"

log "creating $repo"
api DELETE "/repos/$repo" >/dev/null 2>&1 || true
api POST /user/repos '{"name":"workflows","auto_init":true,"default_branch":"main"}' >/dev/null

# One commit with every workflow, the local action and the first cancel trigger.
files=""
add() { # repo path, local file
  content=$(base64 <"$2" | tr -d '\n')
  files="$files{\"operation\":\"create\",\"path\":\"$1\",\"content\":\"$content\"},"
}
for f in "$root"/examples/workflows/*.yaml "$root"/test/e2e/workflows/*.yaml; do
  add ".gitea/workflows/$(basename "$f")" "$f"
done
add .gitea/actions/greet/action.yml "$root/examples/actions/greet/action.yml"
printf 'first\n' >"$work/first"
add cancel-trigger/first "$work/first"
nworkflows=0
for _ in "$root"/examples/workflows/*.yaml "$root"/test/e2e/workflows/*.yaml; do nworkflows=$((nworkflows + 1)); done
log "pushing $nworkflows workflows"
api POST "/repos/$repo/contents" "{\"branch\":\"main\",\"message\":\"e2e: add workflows\",\"files\":[${files%,}]}" >/dev/null

runs() { api GET "/repos/$repo/actions/runs?limit=100"; }
runs_of() { # workflow file -> run JSON objects, oldest first
  runs | json -I0 "[.workflow_runs[] | select(.path | test(\"$1\"))] | sort_by(.id) | .[]"
}

log "waiting for the cancel workflow's first run to start sleeping"
deadline=$(($(date +%s) + 900))
while :; do
  st=$(runs_of 'cancel.yaml' | head -1 | json -r '.status' 2>/dev/null || true)
  [ "$st" = "in_progress" ] && kubectl get pods -n "$jobs_ns" -o name | grep -q . && break
  [ "$(date +%s)" -lt "$deadline" ] || fail "cancel run never started (status: ${st:-none})"
  sleep 5
done
sleep 20 # let the sleeping step begin
log "pushing the cancelling commit"
printf 'second\n' >"$work/second"
files=""
add cancel-trigger/second "$work/second"
api POST "/repos/$repo/contents" "{\"branch\":\"main\",\"message\":\"e2e: cancel-now\",\"files\":[${files%,}]}" >/dev/null

# Both pushes trigger every workflow (cancel.yaml by its path filter).
want_runs=$((nworkflows * 2))
log "waiting for all $want_runs runs to complete (timeout ${timeout_s}s)"
deadline=$(($(date +%s) + timeout_s))
while :; do
  pending=$(runs | json -r '[.workflow_runs[] | select(.status != "completed")] | length')
  total=$(runs | json -r '.workflow_runs | length')
  printf '    %s of %s runs still going\n' "$pending" "$total"
  [ "$pending" = 0 ] && [ "$total" -ge "$want_runs" ] && break
  [ "$(date +%s)" -lt "$deadline" ] || fail "runs did not complete in ${timeout_s}s"
  sleep 15
done

failures=0
expect() { # workflow file, index among its runs, wanted conclusion
  got=$(runs_of "$1" | sed -n "$(($2 + 1))p" | json -r '.conclusion')
  if [ "$got" = "$3" ]; then
    echo "ok   $1 run #$(($2 + 1)): $got"
  else
    echo "FAIL $1 run #$(($2 + 1)): $got, want $3"
    failures=$((failures + 1))
  fi
}
for f in "$root"/examples/workflows/*.yaml; do
  expect "$(basename "$f")" 0 success
  expect "$(basename "$f")" 1 success
done
expect failing.yaml 0 failure
expect failing.yaml 1 failure
expect cancel.yaml 0 cancelled
expect cancel.yaml 1 success

fail_job=$(api GET "/repos/$repo/actions/jobs?limit=100" | json -r '[.jobs[] | select(.name == "fail") | .id] | sort | .[0]')
if api GET "/repos/$repo/actions/jobs/$fail_job/logs" | grep -q "exit code 7"; then
  echo "ok   failing step reports its exit code"
else
  echo "FAIL failing step's log lacks 'exit code 7'"
  failures=$((failures + 1))
fi

log "checking that every job pod was cleaned up"
for _ in $(seq 60); do
  left=$(kubectl get jobs -n "$jobs_ns" -l app.kubernetes.io/managed-by=gitea-k8s-runner-plugin -o name | wc -l | tr -d ' ')
  [ "$left" = 0 ] && break
  sleep 3
done
if [ "$left" = 0 ]; then
  echo "ok   no job pods left behind (cancellation removed its Job)"
else
  echo "FAIL $left Job(s) left in $jobs_ns"
  failures=$((failures + 1))
fi

[ "$failures" = 0 ] || fail "$failures assertion(s) failed"
log "e2e passed"
