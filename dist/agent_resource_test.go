package main

import "testing"

func TestWithAgentVersion(t *testing.T) {
	cases := []struct{ current, want string }{
		{"", "zharp.agent.version=v1.7.0"},
		{"deployment.environment.name=prod", "deployment.environment.name=prod,zharp.agent.version=v1.7.0"},
		{"zharp.agent.version=custom", "zharp.agent.version=custom"},
	}
	for _, c := range cases {
		if got := withAgentVersion(c.current, "v1.7.0"); got != c.want {
			t.Errorf("withAgentVersion(%q) = %q, want %q", c.current, got, c.want)
		}
	}
}
