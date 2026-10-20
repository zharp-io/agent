package dockerlogsreceiver

import (
	"os"
	"regexp"
	"strings"
)

// The collector may itself run in a container (the published image does). Reading its own
// output would feed its log lines back into itself, so that container is skipped. Inside a
// container its ID shows up in the mounts Docker makes (/etc/hostname, /etc/hosts come from
// /var/lib/docker/containers/<id>/) and, on cgroup v1 hosts, in /proc/self/cgroup. The short
// hostname Docker gives by default is the fallback.

var (
	// Only the files Docker bind-mounts into a container's /etc count: on the host itself,
	// mountinfo lists every container's shm mount under the same directory.
	mountIDRe  = regexp.MustCompile(`/containers/([0-9a-f]{64})/(?:hostname|hosts|resolv\.conf) /etc/(?:hostname|hosts|resolv\.conf) `)
	cgroupIDRe = regexp.MustCompile(`(?:docker[-/]|/docker/)([0-9a-f]{64})`)
	shortIDRe  = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

type selfMatcher struct {
	ids      map[string]bool
	hostname string
}

func detectSelf() selfMatcher {
	m := selfMatcher{ids: map[string]bool{}}
	if b, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		for _, s := range mountIDRe.FindAllStringSubmatch(string(b), -1) {
			m.ids[s[1]] = true
		}
	}
	if b, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		for _, s := range cgroupIDRe.FindAllStringSubmatch(string(b), -1) {
			m.ids[s[1]] = true
		}
	}
	if h, err := os.Hostname(); err == nil {
		m.hostname = strings.ToLower(h)
	}
	return m
}

func (m selfMatcher) is(id string) bool {
	if m.ids[id] {
		return true
	}
	return len(m.ids) == 0 && shortIDRe.MatchString(m.hostname) && strings.HasPrefix(id, m.hostname)
}
