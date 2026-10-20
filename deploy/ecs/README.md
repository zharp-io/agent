# Zharp agent on Amazon ECS

Two task definitions, as plain JSON ready for `aws ecs register-task-definition --cli-input-json`
(AWS rejects unknown keys, so the notes live here rather than in the files).

| File | Use it for |
|---|---|
| `ec2-daemon-task.json` | ECS on EC2: one agent per instance, collecting every container's logs and metrics plus the instance's own metrics. |
| `fargate-sidecar-task.json` | ECS on Fargate: the agent as a sidecar next to your app, logs routed through FireLens. See https://www.zharp.io/docs/agent/serverless |

## ec2-daemon-task.json — one agent per EC2 instance

1. Store your API key in Secrets Manager (an Ingest-only key from Integrations & API → Main API keys is enough):

   ```sh
   aws secretsmanager create-secret --name zharp/api-key --secret-string 'zh_live_...'
   ```

   Then put the secret's ARN, and your region, into `secrets` and `logConfiguration` in the file.

2. Put the agent's config on every instance at `/etc/zharp-collector/config.yaml` — the task mounts
   that directory. Use `deploy/docker/collector-config.yaml` from this repository (it reads every
   container's logs through the Docker API, plus container and host metrics); for example, in
   the instances' user data:

   ```sh
   mkdir -p /etc/zharp-collector /var/lib/zharp-collector
   curl -fsSL https://raw.githubusercontent.com/zharp-io/agent/main/deploy/docker/collector-config.yaml \
     -o /etc/zharp-collector/config.yaml
   ```

3. Register the task definition and run it as a daemon service (one copy per instance):

   ```sh
   aws ecs register-task-definition --cli-input-json file://ec2-daemon-task.json
   aws ecs create-service \
     --cluster YOUR_CLUSTER \
     --service-name zharp-collector \
     --task-definition zharp-collector \
     --scheduling-strategy DAEMON
   ```

The agent runs as root inside its container (`"user": "0:0"`) because the Docker socket on the
instance is root-only. It keeps its read positions in `/var/lib/zharp-collector` on the instance,
so a restart neither replays nor skips container logs. Containers labelled `zharp.logs=false`
are skipped; `zharp.service`, `zharp.env`, `zharp.version` and `zharp.source` labels name them —
see https://www.zharp.io/docs/agent/configuration
