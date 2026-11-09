#!/usr/bin/env sh
# Zharp Collector on every Swarm node, with the node's network: the same global service as
# docker-stack.yml, plus each program's network traffic (network by program), which needs the
# node's network — something Docker only gives a service created this way, not from a stack file.
#
# Usage (on a manager node, next to collector-config.yaml):
#   ZHARP_API_KEY=your_api_key sh deploy.sh
# Self-hosted Zharp: also set ZHARP_ENDPOINT=https://zharp.example.com/api/v1
# To update the config, change collector-config.yaml and run it again.
set -eu

: "${ZHARP_API_KEY:?Set ZHARP_API_KEY to your API key}"
IMAGE="${ZHARP_IMAGE:-ghcr.io/zharp-io/agent:latest}"
CONFIG="zharp_collector_config_$(date +%s)"

docker config create "$CONFIG" "$(dirname "$0")/collector-config.yaml" >/dev/null

if docker service inspect zharp_collector >/dev/null 2>&1; then
  OLD="$(docker service inspect zharp_collector --format '{{range .Spec.TaskTemplate.ContainerSpec.Configs}}{{.ConfigName}} {{end}}')"
  docker service update --quiet \
    ${OLD:+--config-rm $OLD} \
    --config-add "source=$CONFIG,target=/etc/zharp-collector/config.yaml,mode=0444" \
    --image "$IMAGE" \
    --env-add "ZHARP_API_KEY=$ZHARP_API_KEY" \
    ${ZHARP_ENDPOINT:+--env-add "ZHARP_ENDPOINT=$ZHARP_ENDPOINT"} \
    zharp_collector
  for c in $OLD; do docker config rm "$c" >/dev/null 2>&1 || true; done
  echo "zharp_collector updated"
  exit 0
fi

docker service create --quiet --name zharp_collector --mode global \
  --user 0:0 \
  --network host \
  --cap-add SYS_PTRACE \
  --restart-condition on-failure --restart-delay 5s \
  --limit-cpu 0.5 --limit-memory 256M --reserve-cpu 0.1 --reserve-memory 64M \
  --env "ZHARP_API_KEY=$ZHARP_API_KEY" \
  --env HOST_ETC=/hostfs/etc \
  ${ZHARP_ENDPOINT:+--env "ZHARP_ENDPOINT=$ZHARP_ENDPOINT"} \
  --config "source=$CONFIG,target=/etc/zharp-collector/config.yaml,mode=0444" \
  --mount type=bind,source=/var/lib/docker/containers,target=/var/lib/docker/containers,readonly \
  --mount type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock,readonly \
  --mount type=bind,source=/,target=/hostfs,readonly \
  --mount type=bind,source=/etc/passwd,target=/etc/passwd,readonly \
  --mount type=bind,source=/etc/group,target=/etc/group,readonly \
  --mount type=volume,source=zharp-state,target=/var/lib/zharp-collector \
  "$IMAGE"
echo "zharp_collector created on every node"
