#!/usr/bin/env bash
# The agent's container deploys, as customers run them, on a Linux machine with Docker: the
# Docker Compose file (deploy/docker) and the Helm chart (deploy/helm) on a kind cluster. Each
# sends to a stand-in for Zharp's ingest (fake-ingest.py), and the test waits until what's using
# the host arrives: each program's CPU and memory, each program's traffic, each drive's folders.
#
# Usage: containers.sh <image> — an agent image already built locally (docker build).
# Needs: docker with compose, python3, kind and helm (CI installs kind; helm is on GitHub's runners).
set -euo pipefail

IMAGE="$1"
ROOT="$(pwd)"
WORK="$(mktemp -d)"
INGEST_COMPOSE="$WORK/compose.bin"
INGEST_KIND="$WORK/kind.bin"
# What the three features send (OTLP metric names, plain in the protobuf body).
WANT=(process.cpu.utilization zharp.network.io zharp.disk.folder.size)

python3 "$ROOT/.github/scripts/fake-ingest.py" 4400 "$INGEST_COMPOSE" &
python3 "$ROOT/.github/scripts/fake-ingest.py" 4401 "$INGEST_KIND" &
trap 'kill $(jobs -p) 2>/dev/null || true' EXIT

# wait_for <file> <minutes>: until every wanted metric has arrived in file.
wait_for() {
  local file="$1" deadline=$(( $(date +%s) + $2 * 60 ))
  while :; do
    local missing=()
    for name in "${WANT[@]}"; do
      grep -aq "$name" "$file" 2>/dev/null || missing+=("$name")
    done
    if [ ${#missing[@]} -eq 0 ]; then
      echo "arrived: ${WANT[*]}"
      return 0
    fi
    if [ "$(date +%s)" -gt "$deadline" ]; then
      echo "never arrived: ${missing[*]}" >&2
      return 1
    fi
    sleep 5
  done
}

# Shorter waits than production (the disk scan's first run, the network report), same features.
quick() {
  sed -e 's/^\( *\)zharp_disk_usage:\r\?$/&\n\1  initial_delay: 10s\n\1  timeout: 90s/' \
      -e 's/^\( *\)zharp_network_usage:\r\?$/&\n\1  sample_interval: 5s/' \
      -e 's/^\( *\)collection_interval: 1m\r\?$/\1collection_interval: 30s/'
}

echo "::group::Docker Compose"
mkdir -p "$WORK/compose"
quick < "$ROOT/deploy/docker/collector-config.yaml" > "$WORK/compose/collector-config.yaml"
sed -e "s#image: ghcr.io/zharp-io/agent:latest#image: $IMAGE#" \
    -e 's#      ZHARP_API_KEY: ${ZHARP_API_KEY}#      ZHARP_API_KEY: zh_ci_dummy\n      ZHARP_ENDPOINT: http://127.0.0.1:4400/api/v1#' \
    "$ROOT/deploy/docker/docker-compose.yml" > "$WORK/compose/docker-compose.yml"
(cd "$WORK/compose" && docker compose -p zharpci up -d)
if ! wait_for "$INGEST_COMPOSE" 5; then
  docker logs zharp-collector 2>&1 | tail -40
  exit 1
fi
# Healthy by its own health check (the image has no shell).
test "$(docker inspect -f '{{.State.Health.Status}}' zharp-collector)" = healthy || {
  sleep 30
  test "$(docker inspect -f '{{.State.Health.Status}}' zharp-collector)" = healthy
}
(cd "$WORK/compose" && docker compose -p zharpci down -v)
echo "::endgroup::"

echo "::group::Kubernetes (kind, Helm chart)"
kind create cluster --name zharp-ci --wait 120s
kind load docker-image "$IMAGE" --name zharp-ci
# The machine running kind, as its node sees it: the kind network's gateway.
GATEWAY="$(docker network inspect kind -f '{{range .IPAM.Config}}{{if .Gateway}}{{.Gateway}} {{end}}{{end}}' | tr ' ' '\n' | grep -E '^[0-9.]+$' | head -1)"
kubectl create namespace zharp-system
helm template zharp-agent "$ROOT/deploy/helm/zharp-agent" --namespace zharp-system \
  --set apiKey=zh_ci_dummy --set endpoint="http://$GATEWAY:4401/api/v1" \
  --set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE##*:}" --set image.pullPolicy=Never \
  --set clusterName=kind-ci | quick | kubectl apply -f -
kubectl -n zharp-system rollout status daemonset/zharp-agent --timeout=180s
if ! wait_for "$INGEST_KIND" 6; then
  kubectl -n zharp-system describe pods | tail -30
  kubectl -n zharp-system logs daemonset/zharp-agent --tail=60
  exit 1
fi
kind delete cluster --name zharp-ci
echo "::endgroup::"

echo "container deploys passed"
