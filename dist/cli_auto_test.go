package main

import (
	"bufio"
	"net"
	"os"
	"strings"
	"testing"
)

// fakeRedis answers the first command it reads with reply, as a Redis server would.
func fakeRedis(t *testing.T, reply string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadString('\n')
		_, _ = conn.Write([]byte(reply))
	}()
	return ln.Addr().String()
}

func TestRedisProbe(t *testing.T) {
	if got := redisProbe(fakeRedis(t, "+PONG\r\n")); got != redisOpen {
		t.Fatalf("PONG: got %v, want open", got)
	}
	if got := redisProbe(fakeRedis(t, "-NOAUTH Authentication required.\r\n")); got != redisNeedsPassword {
		t.Fatalf("NOAUTH: got %v, want needs password", got)
	}
	if got := redisProbe("127.0.0.1:1"); got != redisDown {
		t.Fatalf("closed port: got %v, want down", got)
	}
}

func TestPlanAutomaticLeavesDatabasesForTheDashboard(t *testing.T) {
	plan := planAutomatic([]ServiceOption{
		{Type: "docker", Detected: true},
		{Type: "pg", Detected: true},
		{Type: "mysql", Detected: true},
		{Type: "mongo", Detected: true},
	})

	status := map[string]string{}
	for _, item := range plan.inventory {
		status[item.Type] = item.Status
	}
	for typ, want := range map[string]string{
		"host": invCollecting, "apm": invCollecting, "docker": invCollecting,
		"pg": invNeedsSetup, "mysql": invNeedsSetup, "mongo": invNeedsSetup,
	} {
		if status[typ] != want {
			t.Errorf("%s: got %q, want %q", typ, status[typ], want)
		}
	}

	if strings.Join(plan.metricsReceivers, ",") != "hostmetrics,docker_stats" {
		t.Errorf("receivers: %v", plan.metricsReceivers)
	}
	if !plan.tracing || !plan.hasDocker {
		t.Errorf("tracing=%v docker=%v, want both on", plan.tracing, plan.hasDocker)
	}
	// Nothing that needs a password is guessed at.
	for k := range plan.envVars {
		t.Errorf("unexpected secret %s", k)
	}
}

func TestAddMySQLAddsBothReceiversOnce(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte(buildConfigYAML([]string{"hostmetrics"}, nil, nil, false, "")), 0o644); err != nil {
		t.Fatal(err)
	}
	receivers, block := mysqlReceivers("localhost", "3306", "zharp_monitor")
	for i := 0; i < 2; i++ { // adding twice must not duplicate anything
		if err := addReceiverToConfig(path, receivers, block); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(path)
	config := string(data)
	if !strings.Contains(config, "receivers: [hostmetrics, mysql, sqlquery/mysql]") {
		t.Errorf("metrics pipeline: %s", config[strings.Index(config, "metrics:"):])
	}
	if n := strings.Count(config, "\n  sqlquery/mysql:"); n != 1 {
		t.Errorf("sqlquery/mysql block appears %d times", n)
	}
	if !strings.Contains(config, "SELECT @@max_connections") || !strings.Contains(config, "mysql.connection.errors:") {
		t.Error("missing the max_connections query or the connection.errors metric")
	}
}

func TestBuildConfigCarriesInventoryAndEndpoint(t *testing.T) {
	yaml := buildConfigYAML([]string{"hostmetrics"}, nil, nil, false, "https://zharp.example.com/api/v1")
	for _, want := range []string{
		`endpoint: "https://zharp.example.com/api/v1"`,
		"inventory_file: ",
	} {
		if !strings.Contains(yaml, want) {
			t.Errorf("config is missing %q", want)
		}
	}
	if strings.Contains(buildConfigYAML([]string{"hostmetrics"}, nil, nil, false, ""), "    endpoint: \"") {
		t.Error("an empty endpoint should leave the exporter on its default")
	}
}
