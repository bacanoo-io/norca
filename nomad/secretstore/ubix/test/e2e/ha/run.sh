#!/usr/bin/env bash
# HA failover test: three replicas on kind, a client reading through the
# Service the whole time, and every way the active replica can go away.
#
# Planned events (deleting the active pod, draining its node, a rolling
# restart, sys/step-down) must cost clients nothing: zero failed requests.
# An unplanned one (force-killing the active pod) may fail requests until the
# lock lease lapses, but must recover within the bound.
#
# Needs: a kind cluster from kind.yaml, the image loaded as ubixvault:e2e,
# kubectl, helm. Run from the repo root.
set -euo pipefail

NS=ubixvault-e2e
REL=vault
SVC=http://${REL}-ubixvault.${NS}.svc:8200
HERE=$(cd "$(dirname "$0")" && pwd)
CHART=deploy/charts/ubixvault
LOG=$(mktemp -d)
FAILED=0

k() { kubectl -n "$NS" "$@"; }
step() { printf '\n=== %s\n' "$*"; }
fail() { echo "FAIL: $*"; FAILED=1; }

leader_pod() { # the pod whose IP sys/leader reports
  local ip
  # -m: with a node frozen, the Service still routes some requests to its pod.
  ip=$(k exec loadgen -- curl -s -m 3 "$SVC/v1/sys/leader" | sed -n 's/.*"leader_address":"http:\/\/\([0-9.]*\):.*/\1/p')
  k get pods -l app.kubernetes.io/name=ubixvault -o jsonpath="{range .items[?(@.status.podIP==\"$ip\")]}{.metadata.name}{end}"
}

wait_all_ready() { # three ready replicas and none terminating
  local ready terminating
  for _ in $(seq 1 300); do
    ready=$(k get statefulset ${REL}-ubixvault -o jsonpath='{.status.readyReplicas}')
    terminating=$(k get pods -l app.kubernetes.io/name=ubixvault \
      -o jsonpath='{range .items[?(@.metadata.deletionTimestamp)]}x{end}')
    if [ "${ready:-0}" = 3 ] && [ -z "$terminating" ]; then return 0; fi
    sleep 1
  done
  echo "replicas never all ready"; k get pods -o wide; return 1
}

wait_active() { # until sys/leader names an active replica
  for _ in $(seq 1 120); do
    [ -n "$(leader_pod 2>/dev/null)" ] && return 0
    sleep 0.5
  done
  return 1
}

step "infrastructure"
kubectl create namespace "$NS"
k apply -f "$HERE/infra.yaml"
k wait --for=condition=Ready pod -l app=mariadb --timeout=300s
k wait --for=condition=Ready pod/loadgen --timeout=300s
k create secret generic ubixvault-dsn --from-literal=dsn='root:root@tcp(mariadb:3306)/ubixvault?parseTime=true'
k create secret generic ubixvault-kek --from-literal=auto-unseal-key="$(openssl rand -hex 32)"

step "install: 3 replicas, HA"
helm install "$REL" "$CHART" -n "$NS" \
  --set image.repository=ubixvault --set image.tag=e2e --set image.pullPolicy=Never \
  --set tls.enabled=false --set devNoTLS=true \
  --set storage.type=mysql --set storage.mysql.dsnSecret=ubixvault-dsn \
  --set autoUnseal.existingSecret=ubixvault-kek \
  --set ha.enabled=true --set replicaCount=3
k wait --for=jsonpath='{.status.phase}'=Running pod/${REL}-ubixvault-0 --timeout=300s
sleep 3
ROOT=$(k exec ${REL}-ubixvault-0 -- ubixvault operator init -address http://127.0.0.1:8200 | sed -n 's/^Initial Root Token: //p')
[ -n "$ROOT" ] || { echo "init produced no root token"; exit 1; }
wait_all_ready
k get pods -o wide
wait_active
echo "active: $(leader_pod)"

k exec loadgen -- curl -sf -m 10 -o /dev/null -H "X-Vault-Token: $ROOT" -X POST \
  -d '{"data":{"k":"v"}}' "$SVC/v1/secret/data/e2e"

step "load: one read every 50ms through the Service, no retries"
k exec loadgen -- sh -c "rm -f /tmp/load.log; echo \$\$ > /tmp/load.pid; while :; do
  c=\$(curl -s -o /dev/null -m 10 -w '%{http_code}' -H 'X-Vault-Token: $ROOT' $SVC/v1/secret/data/e2e)
  echo \"\$(date +%s) \$c\" >> /tmp/load.log; sleep 0.05; done" &
LOADPID=$!
sleep 5
started=$(k exec loadgen -- sh -c 'wc -l < /tmp/load.log' 2>/dev/null || echo 0)
echo "load generator: $started requests in the first 5s"
[ "${started:-0}" -gt 20 ] || { echo "load generator is not running"; exit 1; }

phases=() # "name start end planned"
phase() { # name planned command...
  local name=$1 planned=$2; shift 2
  step "$name"
  local start; start=$(date +%s)
  "$@"
  wait_all_ready; wait_active
  sleep 3 # let the load run on the settled cluster
  phases+=("$name $start $(date +%s) $planned")
  echo "active now: $(leader_pod)"
}

delete_active() { k delete pod "$(leader_pod)" --wait=false; sleep 2; }
drain_active_node() {
  local node; node=$(k get pod "$(leader_pod)" -o jsonpath='{.spec.nodeName}')
  echo "draining $node"
  kubectl drain "$node" --ignore-daemonsets --delete-emptydir-data --timeout=300s
  wait_all_ready
  kubectl uncordon "$node"
}
rolling_restart() {
  k rollout restart statefulset/${REL}-ubixvault
  k rollout status statefulset/${REL}-ubixvault --timeout=600s
}
step_down() {
  k exec loadgen -- curl -sf -m 10 -o /dev/null -H "X-Vault-Token: $ROOT" -X PUT "$SVC/v1/sys/step-down"
}
# kubectl's "force" delete still sends SIGTERM, so it is a fast graceful exit,
# not a crash: the replica steps down and it must cost nothing either.
force_delete_active() { k delete pod "$(leader_pod)" --grace-period=0 --force --wait=false; sleep 2; }

# A real crash: freeze the active pod's whole node (docker pause on the kind
# node), as a hung machine or a partition would. No SIGTERM, no step-down: a
# standby must take over once the lock lease lapses, and when the node thaws the
# old active — which still believes it is active — must find its lock gone and
# step down (its writes are fenced meanwhile).
freeze_active_node() {
  local old ip node start took new health
  old=$(leader_pod)
  ip=$(k get pod "$old" -o jsonpath='{.status.podIP}')
  node=$(k get pod "$old" -o jsonpath='{.spec.nodeName}')
  echo "freezing $node (active: $old)"
  start=$(date +%s)
  docker pause "$node"
  for _ in $(seq 1 120); do
    new=$(leader_pod 2>/dev/null || true)
    if [ -n "$new" ] && [ "$new" != "$old" ]; then break; fi
    sleep 0.5
  done
  took=$(( $(date +%s) - start ))
  echo "new active: ${new:-none} after ${took}s"
  if [ -z "$new" ] || [ "$new" = "$old" ]; then fail "no failover while the active's node was frozen"; fi
  if [ "$took" -gt 25 ]; then fail "failover took ${took}s (bound: lock TTL 15s + retry)"; fi
  FREEZE_FAILOVER=$took
  sleep 5
  docker unpause "$node"
  # The thawed replica must step down, not carry on as a second active.
  for _ in $(seq 1 60); do
    health=$(k exec loadgen -- curl -s -o /dev/null -m 3 -w '%{http_code}' "http://$ip:8200/v1/sys/health" || true)
    [ "$health" = 429 ] && break
    sleep 1
  done
  echo "thawed $old answers health $health (429 = standby)"
  if [ "$health" != 429 ]; then fail "thawed former active did not step down (health $health)"; fi
}

phase "delete the active pod (graceful)" yes delete_active
phase "drain the active pod's node" yes drain_active_node
phase "rolling restart (an upgrade)" yes rolling_restart
phase "sys/step-down" yes step_down
phase "force-delete the active pod" yes force_delete_active
phase "freeze the active pod's node (unplanned)" no freeze_active_node

k exec loadgen -- sh -c 'kill "$(cat /tmp/load.pid)"' || true
kill "$LOADPID" 2>/dev/null || true
k exec loadgen -- cat /tmp/load.log > "$LOG/load.log"

step "results"
total=$(wc -l < "$LOG/load.log")
bad_total=$(awk '$2 != "200"' "$LOG/load.log" | wc -l)
echo "requests: $total, failed: $bad_total"
{
  echo "### HA failover (kind, 3 replicas, one read / 50ms through the Service, no client retries)"
  echo
  echo "| Event | Planned | Requests | Failed | Longest outage |"
  echo "|---|---|---|---|---|"
} > "$LOG/summary.md"
for p in "${phases[@]}"; do
  # "name with spaces start end planned": split from the right
  planned=${p##* }; rest=${p% *}; end=${rest##* }; rest=${rest% *}; start=${rest##* }; name=${rest% *}
  reqs=$(awk -v s="$start" -v e="$end" '$1>=s && $1<=e' "$LOG/load.log" | wc -l)
  bad=$(awk -v s="$start" -v e="$end" '$1>=s && $1<=e && $2!="200"' "$LOG/load.log" | wc -l)
  # longest run of consecutive failed seconds
  outage=$(awk -v s="$start" -v e="$end" '$1>=s && $1<=e && $2!="200" {print $1}' "$LOG/load.log" | sort -un |
    awk 'NR==1{run=1;best=1;prev=$1;next} {run=($1==prev+1)?run+1:1; if(run>best)best=run; prev=$1} END{print (NR?best:0)}')
  echo "| $name | $planned | $reqs | $bad | ${outage}s |" >> "$LOG/summary.md"
  # Unplanned: requests the Service still routes to the frozen pod fail until
  # Kubernetes marks its node NotReady — outside the vault's control, so they
  # are reported, not asserted. The failover itself is asserted above.
  echo "$name: $bad of $reqs failed (longest outage ${outage}s)"
  if [ "$reqs" -eq 0 ]; then fail "$name: no requests recorded"; fi
  if [ "$planned" = yes ] && [ "$bad" -ne 0 ]; then
    fail "$name: planned event failed $bad requests"
    awk -v s="$start" -v e="$end" '$1>=s && $1<=e && $2!="200"' "$LOG/load.log" | sort | uniq -c | head
  fi
done
echo >> "$LOG/summary.md"
echo "Unplanned failover (node frozen): a new active after ${FREEZE_FAILOVER:-?}s; the thawed former active stepped down." >> "$LOG/summary.md"
cat "$LOG/summary.md"
[ -n "${GITHUB_STEP_SUMMARY:-}" ] && cat "$LOG/summary.md" >> "$GITHUB_STEP_SUMMARY"

if [ "$FAILED" -ne 0 ]; then
  step "diagnostics"
  k get pods -o wide
  for pod in $(k get pods -l app.kubernetes.io/name=ubixvault -o name); do
    echo "--- $pod"; k logs "$pod" --tail=40 || true
  done
  exit 1
fi
echo "PASS"
