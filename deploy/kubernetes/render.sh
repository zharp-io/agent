#!/bin/sh
# Regenerates zharp-agent.yaml from the Helm chart, so the plain manifests and the chart stay
# the same. Run from the repository root after changing deploy/helm/zharp-agent.
set -eu
out=deploy/kubernetes/zharp-agent.yaml
{
  echo "# Generated from deploy/helm/zharp-agent by deploy/kubernetes/render.sh — don't edit by hand."
  echo "# The API key comes from a Secret you create first:"
  echo "#   kubectl create namespace zharp-system"
  echo "#   kubectl -n zharp-system create secret generic zharp-agent --from-literal=api-key=YOUR_API_KEY"
  helm template zharp-agent deploy/helm/zharp-agent --namespace zharp-system \
    --set existingSecret=zharp-agent "$@"
} > "$out"
echo "wrote $out"
