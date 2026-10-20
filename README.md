# Zharp Collector

The **Zharp Collector** is an OpenTelemetry-based agent that ships logs and metrics from your servers and applications to the [Zharp](https://zharp.io) monitoring platform. It is a purpose-built distribution of the [OpenTelemetry Collector](https://opentelemetry.io/docs/collector/), pre-packaged with receivers for log files, host metrics, Docker, and databases (PostgreSQL, MySQL, Redis, MongoDB).

---

## What it collects

| Source | What it sends |
|---|---|
| Log files (nginx, Apache, app logs, etc.) | Tailed log lines with parsed level and timestamp |
| Host | CPU, memory, disk, network, load |
| Docker | Per-container CPU, memory, and network metrics |
| PostgreSQL | Connections, commits, rollbacks, query latency, cache hit ratio |
| MySQL / MariaDB | Connections, queries, InnoDB buffer pool, table I/O |
| Redis | Memory, connected clients, keyspace hits/misses, command stats |
| MongoDB | Connections, operations, document counts, replication lag |
| Any OTLP app | Traces, metrics, logs via gRPC `:4317` or HTTP `:4318` |

---

## Installation

### Linux / macOS

```bash
curl -fsSL https://raw.githubusercontent.com/zharp-io/agent/main/install.sh | sudo bash
```

The installer:
1. Downloads the correct binary for your OS and architecture
2. Scans for running services (nginx, PostgreSQL, Docker, etc.)
3. Asks for your API key — get one at [zharp.io/settings/api-keys](https://zharp.io/settings/api-keys)
4. Walks you through configuring each detected service
5. Writes the config to `/etc/zharp-collector/config.yaml`
6. Installs and starts the collector as a system service (systemd on Linux, launchd on macOS)

Data appears in your Zharp dashboard within a minute.

**Useful commands after install:**

```bash
# Linux
sudo systemctl status zharp-collector
sudo journalctl -fu zharp-collector
sudo systemctl restart zharp-collector

# macOS
sudo launchctl list io.zharp.collector
sudo tail -f /var/log/zharp-collector.log
```

---

### Windows

Open PowerShell **as Administrator** and run:

```powershell
irm https://raw.githubusercontent.com/zharp-io/agent/main/setup.ps1 | iex
```

The script does the same as the Linux installer — detects running services, asks for your API key, generates the config, and installs the collector as a Windows Service that starts automatically on boot.

**Useful commands after install:**

```powershell
Get-Service ZharpCollector
Restart-Service ZharpCollector
Stop-Service ZharpCollector
```

> **Git Bash users:** you can also run the Linux one-liner — it detects Windows and automatically launches the PowerShell setup.

---

### Docker

The collector tails logs from **every container on the host** automatically — no per-service configuration needed.

```bash
cd deploy/docker
cp .env.example .env
# Edit .env and paste your API key
docker compose up -d
```

Every container's stdout/stderr appears in Zharp within seconds. Host metrics and per-container stats are also collected automatically.

**Send OTLP from your app containers:**

```yaml
environment:
  OTEL_EXPORTER_OTLP_ENDPOINT: http://zharp-collector:4317
```

---

### Docker Swarm

Runs one collector per Swarm node (global service) so every node's logs and metrics are covered.

```bash
# Store the config as a Swarm config object (run once on manager)
docker config create zharp_collector_config deploy/swarm/collector-config.yaml

# Deploy
ZHARP_API_KEY=your_api_key docker stack deploy -c deploy/swarm/docker-stack.yml zharp
```

To update the config after a change:

```bash
docker config rm zharp_collector_config
docker config create zharp_collector_config deploy/swarm/collector-config.yaml
docker service update --force zharp_zharp-collector
```

---

### Kubernetes / EKS / GKE / AKS

Runs as a **DaemonSet** — one pod per node, control plane included — collecting every container's logs (`/var/log/pods`, multi-line stack traces joined), kubelet node/pod/container metrics and node metrics, and receiving OTLP from your apps. Everything is tagged with pod, namespace, node and workload, and named by the `zharp.io/service`, `zharp.io/env`, `zharp.io/version` and `zharp.io/source` pod annotations (`zharp.io/logs: "false"` opts a pod out; `zharp.io/logs-exclude` and `zharp.io/logs-mask` add per-pod rules).

**Helm**

```bash
helm install zharp-agent deploy/helm/zharp-agent \
  --namespace zharp-system --create-namespace \
  --set apiKey=YOUR_API_KEY --set environment=production --set clusterName=prod-eu
```

See `deploy/helm/zharp-agent/values.yaml` for `existingSecret`, `endpoint`, logs/metrics/traces switches, `rules` (exclude, mask, built-in masks), tolerations and resources.

**Plain manifests** (rendered from the chart by `deploy/kubernetes/render.sh`)

```bash
kubectl create namespace zharp-system
kubectl -n zharp-system create secret generic zharp-agent --from-literal=api-key=YOUR_API_KEY
kubectl apply -k https://github.com/zharp-io/agent//deploy/kubernetes
```

**Verify**

```bash
kubectl -n zharp-system get pods -l app.kubernetes.io/name=zharp-agent -o wide
kubectl -n zharp-system logs -l app.kubernetes.io/name=zharp-agent --follow
```

**Send OTLP from your application pods** (the Service keeps traffic on the pod's own node):

```yaml
env:
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: "http://zharp-agent.zharp-system.svc:4318"
```

---

### ECS on EC2

Runs as a **DAEMON** service — one task per EC2 instance — with access to the host Docker socket and container logs.

**1. Place the config on each EC2 instance** (via launch template User Data):

```bash
#!/bin/bash
mkdir -p /etc/zharp-collector
# paste the contents of deploy/docker/collector-config.yaml
cat > /etc/zharp-collector/config.yaml << 'EOF'
...
EOF
```

**2. Store your API key in Secrets Manager**

```bash
aws secretsmanager create-secret \
  --name zharp/api-key \
  --secret-string 'your_api_key_here'
```

**3. Register and deploy**

Each instance also needs the agent's config at `/etc/zharp-collector/config.yaml` — see [deploy/ecs/README.md](deploy/ecs/README.md) for a user-data snippet.

Edit `deploy/ecs/ec2-daemon-task.json` — replace `REGION` and `ACCOUNT_ID` — then:

```bash
aws ecs register-task-definition \
  --cli-input-json file://deploy/ecs/ec2-daemon-task.json

aws ecs create-service \
  --cluster YOUR_CLUSTER \
  --service-name zharp-collector \
  --task-definition zharp-collector \
  --scheduling-strategy DAEMON
```

> **Fargate / Lambda / Render / Railway / Vercel** — these platforms don't expose the host filesystem so the agent cannot be installed. Collect logs from these environments through the **Integrations** section in your Zharp dashboard instead.

---

## Database monitoring

Database receivers are configured automatically when you run `install.sh` or `setup.ps1` and select a database. To add one manually, edit `/etc/zharp-collector/config.yaml`:

### PostgreSQL

```yaml
receivers:
  postgresql:
    endpoint: localhost:5432
    username: zharp_monitor
    password: "${env:PG_PASSWORD}"
    databases:
      - myapp_db
    collection_interval: 30s
    tls:
      insecure: true
```

Required database user:

```sql
CREATE USER zharp_monitor WITH PASSWORD 'your_password';
GRANT pg_monitor TO zharp_monitor;
```

### MySQL / MariaDB

```yaml
receivers:
  mysql:
    endpoint: localhost:3306
    username: zharp_monitor
    password: "${env:MYSQL_PASSWORD}"
    collection_interval: 30s
```

Required database user:

```sql
CREATE USER 'zharp_monitor'@'localhost' IDENTIFIED BY 'your_password';
GRANT PROCESS, REPLICATION CLIENT, SELECT ON *.* TO 'zharp_monitor'@'localhost';
FLUSH PRIVILEGES;
```

### Redis

```yaml
receivers:
  redis:
    endpoint: localhost:6379
    password: "${env:REDIS_PASSWORD}"
    collection_interval: 30s
```

### MongoDB

```yaml
receivers:
  mongodb:
    hosts:
      - endpoint: localhost:27017
    username: zharp_monitor
    password: "${env:MONGO_PASSWORD}"
    collection_interval: 30s
    tls:
      insecure: true
```

Required MongoDB user:

```javascript
db.createUser({
  user: "zharp_monitor",
  pwd: "your_password",
  roles: [{ role: "clusterMonitor", db: "admin" }]
})
```

---

## Secrets

Never hard-code credentials in the config file. Reference them as environment variables:

```yaml
exporters:
  zharp:
    api_key: "${env:ZHARP_API_KEY}"
```

**Linux** — secrets live in `/etc/zharp-collector/env` (chmod 600, written by the installer):

```
ZHARP_API_KEY=zh_...
PG_PASSWORD=...
```

**macOS** — same file, referenced via the launchd plist `EnvironmentVariables`.

**Windows** — stored in the Windows Service registry key (`HKLM:\SYSTEM\CurrentControlSet\Services\ZharpCollector\Environment`), set automatically by `setup.ps1`.

After editing the config or secrets, restart the service:

```bash
# Linux
sudo systemctl restart zharp-collector

# macOS
sudo launchctl unload /Library/LaunchDaemons/io.zharp.collector.plist
sudo launchctl load -w /Library/LaunchDaemons/io.zharp.collector.plist

# Windows (PowerShell as Administrator)
Restart-Service ZharpCollector
```

---

## Health check

The collector exposes a health endpoint at `http://localhost:13133`:

```bash
curl http://localhost:13133
# {"status":"Server available","upSince":"...","uptime":"..."}
```

Use this for Docker health checks, Kubernetes readiness probes, or load balancer health checks.

---

## Building from source

Requirements: Go 1.22+

```bash
git clone https://github.com/zharp-io/agent.git
cd agent/dist
go build -o ../zharp-collector .
```

Cross-compile for Linux from any OS:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -ldflags="-s -w" -o ../zharp-collector-linux-amd64 .
```

---

## Support

- **Docs**: [zharp.io/docs](https://zharp.io/docs)
- **Issues**: [GitHub Issues](https://github.com/zharp-io/agent/issues)
- **Community**: [zharp.io/discord](https://zharp.io/discord)
