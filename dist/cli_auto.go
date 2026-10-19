package main

import (
	"bufio"
	"fmt"
	"net"
	"runtime"
	"strings"
	"time"
)

// autoPlan is what an unattended install sets up: everything it found that needs nothing
// from a person, and a note of everything that does.
type autoPlan struct {
	metricsReceivers []string
	receiverBlocks   []string
	logSources       []LogSource
	envVars          map[string]string
	hasDocker        bool
	tracing          bool
	inventory        []InventoryItem
}

// planAutomatic turns what detectServices found into a working config without asking anything.
//
// The rule is the one Datadog's agent follows: collect whatever can be collected with the
// defaults, and leave anything that needs a secret for the dashboard to ask about. Log files are
// tailed where they exist, Docker is read from its socket, Redis is read when it answers without
// a password, and application traces are accepted on localhost. PostgreSQL, MySQL and MongoDB
// need a monitoring user, so they are reported as waiting rather than guessed at.
func planAutomatic(detected []ServiceOption) autoPlan {
	p := autoPlan{
		metricsReceivers: []string{"hostmetrics"},
		envVars:          map[string]string{},
		tracing:          true,
		inventory: []InventoryItem{
			{Type: "host", Label: "Host metrics", Status: invCollecting, Detail: "CPU, memory, disk, network and load"},
			{Type: "apm", Label: "Application traces", Status: invCollecting, Detail: "OpenTelemetry on 127.0.0.1:4317 (gRPC) and :4318 (HTTP)"},
		},
	}

	for _, s := range detected {
		switch s.Type {
		case "nginx_log":
			p.addLogs(s.Type, "nginx logs", "nginx", existing(nginxLogPaths()...))
		case "apache_log":
			p.addLogs(s.Type, "Apache logs", "apache", existing(
				"/var/log/apache2/access.log", "/var/log/apache2/error.log",
				"/var/log/httpd/access_log", "/var/log/httpd/error_log"))
		case "syslog":
			p.addLogs(s.Type, "System logs", "syslog", existing("/var/log/syslog", "/var/log/messages", "/var/log/auth.log"))
		case "iis_log":
			// IIS writes one folder per site under LogFiles; the glob picks up every one of them.
			p.addLogs(s.Type, "IIS logs", "iis", []string{`C:\inetpub\logs\LogFiles\**\*.log`})
		case "docker":
			p.metricsReceivers = append(p.metricsReceivers, "docker_stats")
			p.receiverBlocks = append(p.receiverBlocks, dockerBlock())
			p.hasDocker = true
			p.inventory = append(p.inventory, InventoryItem{Type: s.Type, Label: "Docker", Status: invCollecting, Detail: "Container CPU, memory and network"})
		case "redis":
			switch redisProbe("localhost:6379") {
			case redisOpen:
				p.metricsReceivers = append(p.metricsReceivers, "redis")
				p.receiverBlocks = append(p.receiverBlocks, "\n  redis:\n    endpoint: localhost:6379\n    collection_interval: 30s\n")
				p.inventory = append(p.inventory, InventoryItem{Type: s.Type, Label: "Redis", Status: invCollecting, Detail: "localhost:6379"})
			case redisNeedsPassword:
				p.inventory = append(p.inventory, InventoryItem{Type: s.Type, Label: "Redis", Status: invNeedsSetup, Detail: "Password required on localhost:6379"})
			}
			// Not answering at all: installed but not running, so there is nothing to report.
		case "pg":
			p.inventory = append(p.inventory, InventoryItem{Type: s.Type, Label: "PostgreSQL", Status: invNeedsSetup, Detail: "Needs a read-only monitoring user"})
		case "mysql":
			p.inventory = append(p.inventory, InventoryItem{Type: s.Type, Label: "MySQL / MariaDB", Status: invNeedsSetup, Detail: "Needs a read-only monitoring user"})
		case "mongo":
			p.inventory = append(p.inventory, InventoryItem{Type: s.Type, Label: "MongoDB", Status: invNeedsSetup, Detail: "Needs a read-only monitoring user"})
		}
	}
	return p
}

// addLogs tails the paths that exist. A web server found by its binary may log somewhere else,
// so with nothing at the default paths it is reported as waiting instead of tailing nothing.
func (p *autoPlan) addLogs(svcType, label, name string, paths []string) {
	if len(paths) == 0 {
		p.inventory = append(p.inventory, InventoryItem{Type: svcType, Label: label, Status: invNeedsSetup, Detail: "No log files at the default location"})
		return
	}
	p.logSources = append(p.logSources, LogSource{Name: name, Paths: paths})
	p.inventory = append(p.inventory, InventoryItem{Type: svcType, Label: label, Status: invCollecting, Detail: strings.Join(paths, ", ")})
}

func nginxLogPaths() []string {
	switch runtime.GOOS {
	case "windows":
		return []string{`C:\nginx\logs\access.log`, `C:\nginx\logs\error.log`}
	case "darwin":
		return []string{"/usr/local/var/log/nginx/access.log", "/usr/local/var/log/nginx/error.log",
			"/opt/homebrew/var/log/nginx/access.log", "/opt/homebrew/var/log/nginx/error.log"}
	default:
		return []string{"/var/log/nginx/access.log", "/var/log/nginx/error.log"}
	}
}

func existing(paths ...string) []string {
	var out []string
	for _, p := range paths {
		if fileExists(p) {
			out = append(out, p)
		}
	}
	return out
}

// mysqlReceivers is how every install path watches MySQL. The mysql receiver reads SHOW GLOBAL
// STATUS, which has the connections in use but not the limit — max_connections is a server
// variable — so a one-line query reports it alongside, and the dashboard can show usage as a
// percentage. connection.errors carries Connection_errors_max_connections, the count of
// connections refused because the limit was hit. The receiver names come back comma-separated,
// ready for the metrics pipeline.
func mysqlReceivers(host, port, user string) (receivers, block string) {
	block = fmt.Sprintf(`
  mysql:
    endpoint: %[1]s:%[2]s
    username: %[3]s
    password: "${env:MYSQL_PASSWORD}"
    collection_interval: 30s
    metrics:
      mysql.connection.errors:
        enabled: true
      mysql.max_used_connections:
        enabled: true
  sqlquery/mysql:
    driver: mysql
    datasource: "%[3]s:${env:MYSQL_PASSWORD}@tcp(%[1]s:%[2]s)/"
    collection_interval: 30s
    queries:
      - sql: "SELECT @@max_connections AS max_connections"
        metrics:
          - metric_name: mysql.connection.max
            value_column: max_connections
            data_type: gauge
            value_type: int
`, host, port, user)
	return "mysql, sqlquery/mysql", block
}

func dockerBlock() string {
	endpoint := "unix:///var/run/docker.sock"
	if runtime.GOOS == "windows" {
		endpoint = "npipe:////./pipe/docker_engine"
	}
	return fmt.Sprintf("\n  docker_stats:\n    endpoint: %s\n    collection_interval: 30s\n    timeout: 20s\n", endpoint)
}

type redisState int

const (
	redisDown redisState = iota
	redisOpen
	redisNeedsPassword
)

// redisProbe sends PING and reads the first reply line: +PONG means it answers without a
// password, -NOAUTH means it wants one. Anything else is treated as not running.
func redisProbe(addr string) redisState {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return redisDown
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("PING\r\n")); err != nil {
		return redisDown
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return redisDown
	}
	switch {
	case strings.HasPrefix(line, "+PONG"):
		return redisOpen
	case strings.HasPrefix(line, "-NOAUTH"), strings.Contains(line, "Authentication required"):
		return redisNeedsPassword
	}
	return redisDown
}
