package main

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// ServiceOption describes a detectable/configurable service.
type ServiceOption struct {
	Label    string
	Type     string
	Detail   string
	Detected bool
}

// detectServices detects running services on the current platform.
func detectServices() []ServiceOption {
	switch runtime.GOOS {
	case "linux":
		return detectLinux()
	case "darwin":
		return detectDarwin()
	case "windows":
		return detectWindows()
	default:
		return nil
	}
}

// allOptions returns detected options first, then non-detected manual options
// (excluding types already in detected).
func allOptions(detected []ServiceOption) []ServiceOption {
	detectedTypes := map[string]bool{}
	for _, d := range detected {
		detectedTypes[d.Type] = true
	}

	manual := []ServiceOption{
		{Label: "nginx logs", Type: "nginx_log", Detail: "tail nginx access/error log"},
		{Label: "Apache logs", Type: "apache_log", Detail: "tail Apache access log"},
		{Label: "System logs", Type: "syslog", Detail: "/var/log/syslog, auth.log"},
		{Label: "PostgreSQL", Type: "pg", Detail: "database metrics"},
		{Label: "MySQL / MariaDB", Type: "mysql", Detail: "database metrics"},
		{Label: "Redis", Type: "redis", Detail: "in-memory store metrics"},
		{Label: "MongoDB", Type: "mongo", Detail: "database metrics"},
		{Label: "Docker", Type: "docker", Detail: "container metrics"},
	}
	if runtime.GOOS == "windows" {
		manual = append(manual, ServiceOption{Label: "IIS logs", Type: "iis_log", Detail: "tail IIS W3C logs"})
	}
	manual = append(manual, ServiceOption{Label: "Custom log file", Type: "custom_log", Detail: "any log file path or glob"})
	manual = append(manual, ServiceOption{Label: "Application traces", Type: "apm", Detail: "receive OpenTelemetry traces from your apps"})

	result := make([]ServiceOption, 0, len(detected)+len(manual))
	result = append(result, detected...)
	for _, m := range manual {
		if !detectedTypes[m.Type] {
			result = append(result, m)
		}
	}
	return result
}

// hasBin reports whether name can be found on PATH.
func hasBin(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// fileExists reports whether path exists on disk.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// svcActiveLinux returns true if the systemd unit is active.
func svcActiveLinux(name string) bool {
	cmd := exec.Command("systemctl", "is-active", "--quiet", name)
	return cmd.Run() == nil
}

// winSvcRunning returns true if the named Windows service is RUNNING.
func winSvcRunning(name string) bool {
	out, err := exec.Command("sc.exe", "query", name).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "RUNNING")
}

// dockerInfo checks whether docker is available and returns the container count.
func dockerInfo() (count int, ok bool) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		return 0, false
	}
	out, err := exec.Command("docker", "ps", "-q").Output()
	if err != nil {
		return 0, true
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	n := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n, true
}

// detectLinux detects services on Linux.
func detectLinux() []ServiceOption {
	var opts []ServiceOption

	// nginx
	if fileExists("/var/log/nginx/access.log") || svcActiveLinux("nginx") || hasBin("nginx") {
		detail := "logs at /var/log/nginx/"
		if fileExists("/var/log/nginx/error.log") {
			detail += " + error.log"
		}
		opts = append(opts, ServiceOption{Label: "nginx", Type: "nginx_log", Detail: detail, Detected: true})
	}

	// Apache
	if fileExists("/var/log/apache2/access.log") || fileExists("/var/log/httpd/access_log") ||
		svcActiveLinux("apache2") || svcActiveLinux("httpd") {
		dir := "/var/log/apache2/"
		if fileExists("/var/log/httpd/access_log") {
			dir = "/var/log/httpd/"
		}
		opts = append(opts, ServiceOption{Label: "Apache", Type: "apache_log", Detail: "logs at " + dir, Detected: true})
	}

	// syslog
	if fileExists("/var/log/syslog") || fileExists("/var/log/messages") {
		opts = append(opts, ServiceOption{Label: "System logs", Type: "syslog", Detail: "/var/log/syslog, auth.log", Detected: true})
	}

	// PostgreSQL
	if svcActiveLinux("postgresql") || svcActiveLinux("postgres") || hasBin("psql") {
		opts = append(opts, ServiceOption{Label: "PostgreSQL", Type: "pg", Detail: "detected on localhost:5432", Detected: true})
	}

	// MySQL / MariaDB
	if svcActiveLinux("mysql") || svcActiveLinux("mysqld") || svcActiveLinux("mariadb") || hasBin("mysql") {
		opts = append(opts, ServiceOption{Label: "MySQL / MariaDB", Type: "mysql", Detail: "detected on localhost:3306", Detected: true})
	}

	// Redis
	if svcActiveLinux("redis") || svcActiveLinux("redis-server") || hasBin("redis-cli") {
		opts = append(opts, ServiceOption{Label: "Redis", Type: "redis", Detail: "detected on localhost:6379", Detected: true})
	}

	// MongoDB
	if svcActiveLinux("mongod") || hasBin("mongosh") || hasBin("mongo") {
		opts = append(opts, ServiceOption{Label: "MongoDB", Type: "mongo", Detail: "detected on localhost:27017", Detected: true})
	}

	// Docker
	if cnt, ok := dockerInfo(); ok {
		detail := "Docker available"
		if cnt > 0 {
			detail = "Docker — containers running"
		}
		_ = cnt
		opts = append(opts, ServiceOption{Label: "Docker", Type: "docker", Detail: detail, Detected: true})
	}

	return opts
}

// detectDarwin detects services on macOS.
func detectDarwin() []ServiceOption {
	var opts []ServiceOption

	// nginx
	if fileExists("/usr/local/var/log/nginx/access.log") || hasBin("nginx") {
		opts = append(opts, ServiceOption{Label: "nginx", Type: "nginx_log", Detail: "logs detected", Detected: true})
	}

	// PostgreSQL
	if hasBin("psql") {
		opts = append(opts, ServiceOption{Label: "PostgreSQL", Type: "pg", Detail: "detected on localhost:5432", Detected: true})
	}

	// MySQL
	if hasBin("mysql") {
		opts = append(opts, ServiceOption{Label: "MySQL / MariaDB", Type: "mysql", Detail: "detected on localhost:3306", Detected: true})
	}

	// Redis
	if hasBin("redis-cli") {
		opts = append(opts, ServiceOption{Label: "Redis", Type: "redis", Detail: "detected on localhost:6379", Detected: true})
	}

	// MongoDB
	if hasBin("mongosh") || hasBin("mongo") {
		opts = append(opts, ServiceOption{Label: "MongoDB", Type: "mongo", Detail: "detected on localhost:27017", Detected: true})
	}

	// Docker
	if cnt, ok := dockerInfo(); ok {
		_ = cnt
		opts = append(opts, ServiceOption{Label: "Docker", Type: "docker", Detail: "Docker container metrics", Detected: true})
	}

	return opts
}

// detectWindows detects services on Windows.
func detectWindows() []ServiceOption {
	var opts []ServiceOption

	// nginx
	if fileExists(`C:\nginx\logs\access.log`) || hasBin("nginx") || winSvcRunning("nginx") {
		opts = append(opts, ServiceOption{Label: "nginx", Type: "nginx_log", Detail: `logs at C:\nginx\logs\`, Detected: true})
	}

	// IIS
	if winSvcRunning("W3SVC") {
		opts = append(opts, ServiceOption{Label: "IIS (Web Server)", Type: "iis_log", Detail: `logs at C:\inetpub\logs\`, Detected: true})
	}

	// PostgreSQL
	if hasBin("psql") || winSvcRunning("postgresql") || winSvcRunning("postgresql-x64-15") {
		opts = append(opts, ServiceOption{Label: "PostgreSQL", Type: "pg", Detail: "detected on localhost:5432", Detected: true})
	}

	// MySQL
	if hasBin("mysql") || winSvcRunning("MySQL") || winSvcRunning("MySQL80") {
		opts = append(opts, ServiceOption{Label: "MySQL / MariaDB", Type: "mysql", Detail: "detected on localhost:3306", Detected: true})
	}

	// Redis
	if hasBin("redis-cli") || winSvcRunning("Redis") {
		opts = append(opts, ServiceOption{Label: "Redis", Type: "redis", Detail: "detected on localhost:6379", Detected: true})
	}

	// MongoDB
	if hasBin("mongosh") || hasBin("mongo") || winSvcRunning("MongoDB") {
		opts = append(opts, ServiceOption{Label: "MongoDB", Type: "mongo", Detail: "detected on localhost:27017", Detected: true})
	}

	// Docker
	if cnt, ok := dockerInfo(); ok {
		_ = cnt
		opts = append(opts, ServiceOption{Label: "Docker", Type: "docker", Detail: "Docker container metrics", Detected: true})
	}

	return opts
}
