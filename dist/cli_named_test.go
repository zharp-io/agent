package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	envprovider "go.opentelemetry.io/collector/confmap/provider/envprovider"
	fileprovider "go.opentelemetry.io/collector/confmap/provider/fileprovider"
	"go.opentelemetry.io/collector/otelcol"
)

// validateConfig runs the collector's own dry run over a config file: every component id,
// pipeline and setting must be one this build accepts.
func validateConfig(t *testing.T, path string) {
	t.Helper()
	for _, env := range []string{"ZHARP_API_KEY", "PG_PASSWORD", "PG_PASSWORD_ORDERS", "MYSQL_PASSWORD", "MYSQL_PASSWORD_BILLING", "REDIS_PASSWORD_CACHE", "MONGO_PASSWORD_EVENTS"} {
		t.Setenv(env, "secret")
	}
	col, err := otelcol.NewCollector(otelcol.CollectorSettings{
		BuildInfo: component.BuildInfo{Command: "zharp-collector-test", Version: "test"},
		Factories: components,
		ConfigProviderSettings: otelcol.ConfigProviderSettings{
			ResolverSettings: confmap.ResolverSettings{
				URIs:              []string{"file:" + path},
				ProviderFactories: []confmap.ProviderFactory{envprovider.NewFactory(), fileprovider.NewFactory()},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := col.DryRun(context.Background()); err != nil {
		data, _ := os.ReadFile(path)
		t.Fatalf("config does not validate: %v\n%s", err, data)
	}
}

func TestNamedIDs(t *testing.T) {
	if id, env := namedIDs("postgresql", "PG_PASSWORD", ""); id != "postgresql" || env != "PG_PASSWORD" {
		t.Errorf("unnamed: %s %s", id, env)
	}
	if id, env := namedIDs("postgresql", "PG_PASSWORD", "Orders-EU"); id != "postgresql/orders-eu" || env != "PG_PASSWORD_ORDERS_EU" {
		t.Errorf("named: %s %s", id, env)
	}
	if checkDatabaseName("orders") != nil || checkDatabaseName("") != nil {
		t.Error("valid names refused")
	}
	for _, bad := range []string{"a b", "../x", "x:y", "-x", strings.Repeat("a", 41)} {
		if checkDatabaseName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if got := databaseNameFlag("pg", []string{"--host", "x", "--name", "Orders"}); got != "orders" {
		t.Errorf("--name NAME: %q", got)
	}
	if got := databaseNameFlag("redis", []string{"--name=cache"}); got != "cache" {
		t.Errorf("--name=NAME: %q", got)
	}
	if got := databaseNameFlag("docker", []string{"--name", "x"}); got != "" {
		t.Errorf("non-database type: %q", got)
	}
}

func TestSecondPostgresGetsItsOwnPipeline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(buildConfigYAML([]string{"hostmetrics"}, nil, nil, false, "https://zharp.example/api/v1")), 0o644); err != nil {
		t.Fatal(err)
	}

	// The first database: unchanged default receiver in the main pipeline.
	receiver, block, _, env, err := parseAddArgs("pg", []string{"--password", "x", "--db", "app"})
	if err != nil || receiver != "postgresql" || env["PG_PASSWORD"] != "x" {
		t.Fatalf("default: %v %s %v", err, receiver, env)
	}
	if err := addReceiverToConfig(path, receiver, block); err != nil {
		t.Fatal(err)
	}

	// The second: named.
	receiver, block, _, env, err = parseAddArgs("pg", []string{"--name", "orders", "--host", "10.0.0.12", "--password", "y", "--db", "orders"})
	if err != nil || receiver != "postgresql/orders" || env["PG_PASSWORD_ORDERS"] != "y" {
		t.Fatalf("named: %v %s %v", err, receiver, env)
	}
	data, _ := os.ReadFile(path)
	content, existed, err := addNamedDatabase(string(data), receiver, block)
	if err != nil || existed {
		t.Fatalf("add named: %v existed=%v", err, existed)
	}
	for _, want := range []string{
		"\n  postgresql/orders:\n    endpoint: 10.0.0.12:5432",
		`password: "${env:PG_PASSWORD_ORDERS}"`,
		"\n  zharp/postgresql_orders:\n",
		`database: "postgresql/orders"`,
		"metrics/postgresql_orders:\n      receivers: [postgresql/orders]",
		"exporters: [zharp/postgresql_orders]",
		"receivers: [hostmetrics, postgresql]", // the first database is untouched
	} {
		if !strings.Contains(content, want) {
			t.Errorf("missing %q in:\n%s", want, content)
		}
	}
	// The extra exporter shares key and endpoint but not the inventory (one heartbeat per host).
	exp := content[strings.Index(content, "  zharp/postgresql_orders:"):]
	exp = exp[:strings.Index(exp, "database:")]
	if !strings.Contains(exp, "api_key:") || !strings.Contains(exp, "zharp.example") || strings.Contains(exp, "inventory_file") {
		t.Errorf("named exporter settings: %s", exp)
	}

	// Adding the same name again keeps what is there.
	again, existed, err := addNamedDatabase(content, receiver, block)
	if err != nil || !existed || again != content {
		t.Errorf("re-adding a name changed the config (existed=%v, err=%v)", existed, err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	validateConfig(t, path)
}

func TestNamedDatabasesOfEveryEngineValidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := buildConfigYAML([]string{"hostmetrics"}, nil, nil, false, "")
	for _, c := range []struct {
		typ  string
		args []string
	}{
		{"mysql", []string{"--name", "billing", "--password", "x"}},
		{"redis", []string{"--name", "cache", "--password", "x"}},
		{"mongo", []string{"--name", "events", "--password", "x"}},
	} {
		receiver, block, _, _, err := parseAddArgs(c.typ, c.args)
		if err != nil {
			t.Fatal(err)
		}
		if content, _, err = addNamedDatabase(content, receiver, block); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(content, "receivers: [mysql/billing, sqlquery/mysql_billing]") {
		t.Errorf("mysql pipeline missing its connection-limit query:\n%s", content)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	validateConfig(t, path)
}
