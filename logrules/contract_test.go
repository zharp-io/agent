package logrules

import "testing"

func TestResolveDockerPrecedence(t *testing.T) {
	// Label over env over Compose service over container name.
	m := ResolveDocker(map[string]string{ComposeServiceLabel: "web", LabelService: "storefront"},
		[]string{"ZHARP_SERVICE=from-env", "ZHARP_ENV=staging", "PATH=/bin"}, "/shop-web-1", "nginx:1.27")
	if m.Service != "storefront" || m.Env != "staging" || m.Source != "nginx" {
		t.Fatalf("%+v", m)
	}
	m = ResolveDocker(map[string]string{ComposeServiceLabel: "web"}, []string{"ZHARP_SERVICE=from-env"}, "/shop-web-1", "x")
	if m.Service != "from-env" {
		t.Fatalf("env should win over compose: %+v", m)
	}
	m = ResolveDocker(map[string]string{ComposeServiceLabel: "web"}, nil, "/shop-web-1", "x")
	if m.Service != "web" {
		t.Fatalf("compose: %+v", m)
	}
	m = ResolveDocker(nil, nil, "/lonely", "x")
	if m.Service != "lonely" || m.Source != "" {
		t.Fatalf("name: %+v", m)
	}
	m = ResolveDocker(map[string]string{LabelLogs: "false", LabelSource: "java", LabelEnv: "prod", LabelVersion: "1.2.3",
		LabelMultiline: "off", LabelExclude: "health", LabelMask: `[{"pattern":"x"}]`},
		[]string{"ZHARP_VERSION=0.9"}, "/a", "postgres:16")
	if !m.Disabled || m.Source != "java" || m.Env != "prod" || m.Version != "1.2.3" || m.Multiline != "off" {
		t.Fatalf("%+v", m)
	}
	s, errs := m.Compile()
	if len(errs) != 0 || !s.Multiline.Off() || s.Rules == nil || !s.Rules.Excluded("GET /health") {
		t.Fatalf("compile: %+v %v", s, errs)
	}
}

func TestResolveK8s(t *testing.T) {
	p := K8sPod{
		Annotations: map[string]string{
			"zharp.io/service":         "checkout",
			"zharp.io/env":             "prod",
			"zharp.io/sidecar.service": "checkout-proxy",
			"zharp.io/sidecar.logs":    "false",
		},
		Labels:    map[string]string{K8sLabelName: "shop", K8sLabelVersion: "2.0.1"},
		Container: "app",
		Image:     "eclipse-temurin:21-jre",
		Workload:  "checkout-deploy",
	}
	m := ResolveK8s(p)
	if m.Service != "checkout" || m.Env != "prod" || m.Version != "2.0.1" || m.Source != "java" || m.Disabled {
		t.Fatalf("app: %+v", m)
	}
	p.Container = "sidecar"
	p.Image = "envoyproxy/envoy:v1.31"
	m = ResolveK8s(p)
	if m.Service != "checkout-proxy" || !m.Disabled || m.Source != "" {
		t.Fatalf("sidecar: %+v", m)
	}
	// Without annotations: an SDK's service.name, then the name label, then the workload.
	p = K8sPod{Labels: map[string]string{}, Workload: "api", Container: "c", Service: "sdk-name"}
	if m := ResolveK8s(p); m.Service != "sdk-name" {
		t.Fatalf("%+v", m)
	}
	p.Service = ""
	if m := ResolveK8s(p); m.Service != "api" {
		t.Fatalf("%+v", m)
	}
}

func TestSourceForImage(t *testing.T) {
	cases := map[string]string{
		"nginx":             "nginx",
		"nginx:1.27-alpine": "nginx",
		"docker.io/nginxinc/nginx-unprivileged:1.27": "nginx",
		"bitnami/postgresql:16":                      "postgresql",
		"postgres@sha256:abcd":                       "postgresql",
		"httpd:2.4":                                  "apache",
		"mariadb:11":                                 "mysql",
		"redis:7":                                    "redis",
		"mongo:7":                                    "mongodb",
		"node:22-slim":                               "node",
		"python:3.12":                                "python",
		"eclipse-temurin:21":                         "java",
		"openjdk:17":                                 "java",
		"golang:1.23":                                "go",
		"ghcr.io/acme/nodejs-api:1":                  "",
		"localhost:5000/myapp:1":                     "",
	}
	for in, want := range cases {
		if got := SourceForImage(in); got != want {
			t.Errorf("SourceForImage(%q) = %q, want %q", in, got, want)
		}
	}
}
