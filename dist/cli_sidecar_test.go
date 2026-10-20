package main

import (
	"encoding/json"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/confmap/xconfmap"
)

func envOf(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestDetectPlatform(t *testing.T) {
	cases := []struct {
		env       map[string]string
		name      string
		detectors []string
		service   string
	}{
		{map[string]string{"ECS_CONTAINER_METADATA_URI_V4": "http://169.254.170.2/v4/x"}, "ecs", []string{"env", "ecs"}, ""},
		{map[string]string{"K_SERVICE": "api", "K_REVISION": "api-00001"}, "cloudrun", []string{"env", "gcp"}, "api"},
		{map[string]string{"CONTAINER_APP_NAME": "web", "CONTAINER_APP_REVISION": "web--r1"}, "containerapps", []string{"env"}, "web"},
		{map[string]string{}, "none", []string{"env"}, ""},
		{map[string]string{"ZHARP_PLATFORM": "none", "K_SERVICE": "api"}, "none", []string{"env"}, ""},
	}
	for _, c := range cases {
		p := detectPlatform(envOf(c.env))
		if p.Name != c.name || strings.Join(p.Detectors, ",") != strings.Join(c.detectors, ",") || p.ServiceLiteral != c.service {
			t.Errorf("%v: got %+v", c.env, p)
		}
	}
}

func TestSidecarRequiresAPIKey(t *testing.T) {
	if _, err := loadSidecarSettings(envOf(nil)); err == nil {
		t.Fatal("expected an error without ZHARP_API_KEY")
	}
}

func TestOTTLStringEscaping(t *testing.T) {
	if got := ottlStr(`a"b\c$d`); got != `"a\"b\\c$$d"` {
		t.Fatalf("got %s", got)
	}
}

// Every component in the generated config must unmarshal into its factory's config and
// validate — that is where OTTL statements are parsed.
func TestSidecarConfigValidates(t *testing.T) {
	for _, env := range []map[string]string{
		{"ZHARP_API_KEY": "k", "ZHARP_SERVICE": "checkout", "ZHARP_ENV": "prod", "ZHARP_VERSION": "1.2.3", "ZHARP_SOURCE": "nodejs", "ECS_CONTAINER_METADATA_URI_V4": "http://x"},
		{"ZHARP_API_KEY": "k", "K_SERVICE": "api", "K_REVISION": "api-1", "ZHARP_ENDPOINT": "http://sink", "ZHARP_DOGSTATSD": "false", "ZHARP_LOGS_PATH": "/logs/*.log, /logs/*.json"},
		{"ZHARP_API_KEY": "k", "CONTAINER_APP_NAME": `we"ird$name`, "ZHARP_FLUENT_FORWARD": "off"},
	} {
		s, err := loadSidecarSettings(envOf(env))
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(s.configYAML()), &raw); err != nil {
			t.Fatal(err)
		}
		f, err := components()
		if err != nil {
			t.Fatal(err)
		}
		check := func(section string, factory func(component.Type) (component.Factory, bool)) {
			for id, body := range raw[section].(map[string]any) {
				var cid component.ID
				if err := cid.UnmarshalText([]byte(id)); err != nil {
					t.Fatal(err)
				}
				fac, ok := factory(cid.Type())
				if !ok {
					t.Fatalf("%s %s: not in this build", section, id)
				}
				cfg := fac.CreateDefaultConfig()
				// The resolver would expand ${env:…}; it isn't running here.
				conf := confmap.NewFromStringMap(map[string]any{"c": body})
				sub, _ := conf.Sub("c")
				if err := sub.Unmarshal(cfg); err != nil {
					t.Fatalf("%s %s: %v", section, id, err)
				}
				if err := xconfmap.Validate(cfg); err != nil {
					t.Fatalf("%s %s: %v", section, id, err)
				}
			}
		}
		check("receivers", func(t component.Type) (component.Factory, bool) { v, ok := f.Receivers[t]; return v, ok })
		check("processors", func(t component.Type) (component.Factory, bool) { v, ok := f.Processors[t]; return v, ok })
		check("extensions", func(t component.Type) (component.Factory, bool) { v, ok := f.Extensions[t]; return v, ok })
		check("exporters", func(t component.Type) (component.Factory, bool) { v, ok := f.Exporters[t]; return v, ok })

		pipes := raw["service"].(map[string]any)["pipelines"].(map[string]any)
		_, hasFluent := pipes["logs/firelens"]
		if hasFluent != (env["ZHARP_FLUENT_FORWARD"] != "off") {
			t.Errorf("logs/firelens pipeline present=%v for %v", hasFluent, env)
		}
		if _, ok := pipes["logs/files"]; ok != (env["ZHARP_LOGS_PATH"] != "") {
			t.Errorf("logs/files pipeline present=%v for %v", ok, env)
		}
		if _, ok := raw["receivers"].(map[string]any)["hostmetrics"]; ok {
			t.Error("sidecar must not collect host metrics")
		}
	}
}
