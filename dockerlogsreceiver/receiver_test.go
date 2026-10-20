package dockerlogsreceiver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
)

// fakeDocker is just enough of the Engine API, on a unix socket, to drive the receiver.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]*fakeContainer
	events     chan dockerEvent
	sinces     map[string][]string // since= values each container's logs were requested with
	sock       string
	srv        *http.Server
}

type fakeContainer struct {
	info  containerInfo
	lines []fakeLine
}

type fakeLine struct {
	ts     time.Time
	stream byte
	text   string
}

func newFakeDocker(t *testing.T) (*fakeDocker, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "dl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	f := &fakeDocker{containers: map[string]*fakeContainer{}, events: make(chan dockerEvent, 10), sinces: map[string][]string{}}
	f.sock = sock
	f.srv = &http.Server{Handler: http.HandlerFunc(f.serve)}
	srv := f.srv
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { f.srv.Close() })
	return f, "unix://" + sock
}

func (f *fakeDocker) add(id, name string, labels map[string]string, tty bool, lines ...fakeLine) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &fakeContainer{lines: lines}
	c.info.ID = id
	c.info.Name = "/" + name
	c.info.Config.Tty = tty
	c.info.Config.Image = "example/" + name + ":1.0"
	c.info.Config.Labels = labels
	c.info.State.Running = true
	c.info.State.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	f.containers[id] = c
}

func (f *fakeDocker) write(id string, l fakeLine) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[id].lines = append(f.containers[id].lines, l)
}

func (f *fakeDocker) serve(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	switch {
	case path == "/containers/json":
		f.mu.Lock()
		var out []containerSummary
		for id, c := range f.containers {
			out = append(out, containerSummary{ID: id, Names: []string{c.info.Name}})
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	case path == "/events":
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for {
			select {
			case <-req.Context().Done():
				return
			case e := <-f.events:
				_ = json.NewEncoder(w).Encode(e)
				w.(http.Flusher).Flush()
			}
		}
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
		f.mu.Lock()
		c := f.containers[id]
		f.mu.Unlock()
		if c == nil {
			http.Error(w, `{"message":"no such container"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(c.info)
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/logs"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/logs")
		since := time.Time{}
		if s := req.URL.Query().Get("since"); s != "" {
			sec, frac, _ := strings.Cut(s, ".")
			n, _ := strconv.ParseInt(sec, 10, 64)
			ns, _ := strconv.ParseInt(frac, 10, 64)
			since = time.Unix(n, ns)
		}
		f.mu.Lock()
		f.sinces[id] = append(f.sinces[id], req.URL.Query().Get("since"))
		tty := f.containers[id].info.Config.Tty
		f.mu.Unlock()
		if tty {
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		} else {
			w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
		}
		w.WriteHeader(http.StatusOK)
		sent := 0
		for {
			f.mu.Lock()
			pending := append([]fakeLine(nil), f.containers[id].lines[sent:]...)
			f.mu.Unlock()
			for _, l := range pending {
				sent++
				if l.ts.Before(since) { // Docker's since is inclusive
					continue
				}
				msg := l.ts.UTC().Format("2006-01-02T15:04:05.000000000Z07:00") + " " + l.text + "\n"
				if tty {
					_, _ = w.Write([]byte(strings.TrimSuffix(msg, "\n") + "\r\n"))
				} else {
					_, _ = w.Write(frame(l.stream, msg))
				}
			}
			w.(http.Flusher).Flush()
			select {
			case <-req.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	default:
		http.NotFound(w, req)
	}
}

type got struct{ service, container, stream, body, level string }

func received(sink *consumertest.LogsSink) []got {
	var out []got
	for _, ld := range sink.AllLogs() {
		for i := 0; i < ld.ResourceLogs().Len(); i++ {
			rl := ld.ResourceLogs().At(i)
			svc, _ := rl.Resource().Attributes().Get("service.name")
			for j := 0; j < rl.ScopeLogs().Len(); j++ {
				recs := rl.ScopeLogs().At(j).LogRecords()
				for k := 0; k < recs.Len(); k++ {
					r := recs.At(k)
					name, _ := r.Attributes().Get("container.name")
					stream, _ := r.Attributes().Get("stream")
					out = append(out, got{svc.Str(), name.Str(), stream.Str(), r.Body().Str(), r.SeverityText()})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].body < out[j].body })
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func startReceiver(t *testing.T, endpoint, state string, sink *consumertest.LogsSink, self string) *dockerLogsReceiver {
	t.Helper()
	cfg := createDefaultConfig().(*Config)
	cfg.Endpoint = endpoint
	cfg.StateFile = state
	r, err := newReceiver(cfg, zap.NewNop(), sink)
	if err != nil {
		t.Fatal(err)
	}
	r.findSelf = func() selfMatcher { return selfMatcher{ids: map[string]bool{self: true}} }
	if err := r.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReceiverEndToEnd(t *testing.T) {
	fake, endpoint := newFakeDocker(t)
	state := filepath.Join(t.TempDir(), "docker_logs.json")
	past := time.Now().Add(-time.Hour)

	fake.add("api0000000000000000000000000000000000000000000000000000000000001", "proj-api-1",
		map[string]string{"com.docker.compose.service": "api"}, false,
		fakeLine{past, 1, "old history must not be replayed"})
	fake.add("tty0000000000000000000000000000000000000000000000000000000000002", "console", nil, true)
	fake.add("off0000000000000000000000000000000000000000000000000000000000003", "noisy",
		map[string]string{"zharp.logs": "false"}, false)
	fake.add("self000000000000000000000000000000000000000000000000000000000004", "zharp-agent", nil, false)

	sink := new(consumertest.LogsSink)
	r := startReceiver(t, endpoint, state, sink, "self000000000000000000000000000000000000000000000000000000000004")
	waitFor(t, "tails to start", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.sinces) >= 2
	})

	now := time.Now()
	fake.write("api0000000000000000000000000000000000000000000000000000000000001", fakeLine{now, 1, `{"level":"error","msg":"a1 db down"}`})
	fake.write("api0000000000000000000000000000000000000000000000000000000000001", fakeLine{now.Add(time.Millisecond), 2, "a2 plain stderr line"})
	fake.write("tty0000000000000000000000000000000000000000000000000000000000002", fakeLine{now, 1, "WARN t1 from a terminal"})
	fake.write("off0000000000000000000000000000000000000000000000000000000000003", fakeLine{now, 1, "o1 excluded"})
	fake.write("self000000000000000000000000000000000000000000000000000000000004", fakeLine{now, 1, "s1 my own output"})

	// A container that starts mid-run is read from its first line.
	fake.add("new0000000000000000000000000000000000000000000000000000000000005", "late", nil, false,
		fakeLine{time.Now(), 1, "n1 first line of a new container"})
	fake.events <- dockerEvent{Type: "container", Action: "start", Actor: struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	}{ID: "new0000000000000000000000000000000000000000000000000000000000005"}}

	waitFor(t, "four lines", func() bool { return len(received(sink)) >= 4 })
	time.Sleep(200 * time.Millisecond)
	lines := received(sink)
	want := []got{
		{"api", "proj-api-1", "stdout", `{"level":"error","msg":"a1 db down"}`, "ERROR"},
		{"api", "proj-api-1", "stderr", "a2 plain stderr line", ""},
		{"late", "late", "stdout", "n1 first line of a new container", ""},
		{"console", "console", "stdout", "WARN t1 from a terminal", "WARN"},
	}
	sort.Slice(want, func(i, j int) bool { return want[i].body < want[j].body })
	if fmt.Sprint(lines) != fmt.Sprint(want) {
		t.Fatalf("first run:\n got  %v\n want %v", lines, want)
	}
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	// While the collector is down the API container writes two more lines; after the restart
	// exactly those arrive, and nothing from before is sent again.
	later := time.Now()
	fake.write("api0000000000000000000000000000000000000000000000000000000000001", fakeLine{later, 1, "a3 written while down"})
	fake.write("api0000000000000000000000000000000000000000000000000000000000001", fakeLine{later, 1, "a4 same timestamp"})

	sink2 := new(consumertest.LogsSink)
	r2 := startReceiver(t, endpoint, state, sink2, "self000000000000000000000000000000000000000000000000000000000004")
	defer r2.Shutdown(context.Background()) //nolint:errcheck
	waitFor(t, "resumed lines", func() bool { return len(received(sink2)) >= 2 })
	time.Sleep(300 * time.Millisecond)
	var bodies []string
	for _, g := range received(sink2) {
		bodies = append(bodies, g.body)
	}
	if strings.Join(bodies, "|") != "a3 written while down|a4 same timestamp" {
		t.Fatalf("after restart got %q", bodies)
	}
}

func TestDescribeAndImageName(t *testing.T) {
	var info containerInfo
	info.ID = "abc"
	info.Name = "/web-1"
	info.Config.Image = "ghcr.io/acme/web:2.3@sha256:deadbeef"
	r := &dockerLogsReceiver{cfg: createDefaultConfig().(*Config)}
	if m := r.describe(&info); m.contract.Service != "web-1" || m.name != "web-1" || m.image != "ghcr.io/acme/web" {
		t.Errorf("no compose label: %+v", m)
	}
	info.Config.Labels = map[string]string{"com.docker.compose.service": "web"}
	if m := r.describe(&info); m.contract.Service != "web" {
		t.Errorf("compose label: %+v", m)
	}
	if imageName("localhost:5000/app") != "localhost:5000/app" || imageName("redis:7") != "redis" {
		t.Error("image name parsing")
	}
}

func TestExclusion(t *testing.T) {
	r := &dockerLogsReceiver{cfg: &Config{ExcludeLabel: "zharp.logs"}, self: selfMatcher{hostname: "0123456789ab"}}
	var c containerInfo
	c.ID = "0123456789abffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if r.excluded(&c) == "" {
		t.Error("own container (matched by hostname) should be excluded")
	}
	c.ID = "ffff"
	if r.excluded(&c) != "" {
		t.Error("ordinary container excluded")
	}
	c.Config.Labels = map[string]string{"zharp.logs": "False"}
	if r.excluded(&c) == "" {
		t.Error("zharp.logs=false should exclude")
	}
	c.Config.Labels = map[string]string{"zharp.logs": "true"}
	if r.excluded(&c) != "" {
		t.Error("zharp.logs=true should not exclude")
	}
}

func TestSelfDetectionIgnoresHostShmMounts(t *testing.T) {
	host := "100 90 0:50 / /var/lib/docker/containers/" + strings.Repeat("a", 64) + "/mounts/shm rw - tmpfs shm rw\n"
	inside := "200 190 8:1 /var/lib/docker/containers/" + strings.Repeat("b", 64) + "/hostname /etc/hostname rw - ext4 /dev/sda1 rw\n"
	if m := mountIDRe.FindAllStringSubmatch(host, -1); len(m) != 0 {
		t.Errorf("host shm mount matched: %v", m)
	}
	if m := mountIDRe.FindAllStringSubmatch(inside, -1); len(m) != 1 || m[0][1] != strings.Repeat("b", 64) {
		t.Errorf("container hostname mount not matched: %v", m)
	}
}

var _ = plog.SeverityNumberUnspecified

// TestLabelsContract follows a container whose labels and environment name it, join its stack
// trace, drop excluded lines and mask secrets — on top of what the pipeline's processor does.
func TestLabelsContract(t *testing.T) {
	fake, endpoint := newFakeDocker(t)
	id := "lbl0000000000000000000000000000000000000000000000000000000000001"
	fake.add(id, "shop-api-1", map[string]string{
		"com.docker.compose.service": "api",
		"zharp.service":              "checkout",
		"zharp.logs.exclude":         `GET /health`,
		"zharp.logs.mask":            `[{"pattern":"(user=)\\w+","replace":"${1}***"}]`,
	}, false)
	fake.mu.Lock()
	fake.containers[id].info.Config.Env = []string{"ZHARP_ENV=staging", "ZHARP_VERSION=1.4.2", "ZHARP_SERVICE=ignored"}
	fake.containers[id].info.Config.Image = "eclipse-temurin:21-jre"
	fake.mu.Unlock()

	sink := new(consumertest.LogsSink)
	r := startReceiver(t, endpoint, "", sink, "none")
	defer r.Shutdown(context.Background()) //nolint:errcheck
	waitFor(t, "tail", func() bool { fake.mu.Lock(); defer fake.mu.Unlock(); return len(fake.sinces[id]) == 1 })

	now := time.Now()
	for i, l := range []string{
		"2026-10-19 12:00:01.123 ERROR [main] c.e.Checkout - failed for user=bob",
		"java.lang.IllegalStateException: Order 42 has no lines",
		"\tat com.example.shop.OrderService.total(OrderService.java:57)",
		"\t... 3 more",
		"2026-10-19 12:00:02.000 INFO [main] GET /health 200",
		"2026-10-19 12:00:03.000 INFO [main] done",
	} {
		fake.write(id, fakeLine{now.Add(time.Duration(i) * time.Millisecond), 2, l})
	}
	waitFor(t, "two records", func() bool { return sink.LogRecordCount() >= 2 })
	time.Sleep(200 * time.Millisecond)
	if n := sink.LogRecordCount(); n != 2 {
		t.Fatalf("want 2 records (trace joined, health excluded), got %d", n)
	}
	var bodies []string
	for _, ld := range sink.AllLogs() {
		res := ld.ResourceLogs().At(0).Resource().Attributes()
		for k, want := range map[string]string{"service.name": "checkout", "deployment.environment.name": "staging", "service.version": "1.4.2", "zharp.source": "java"} {
			if v, _ := res.Get(k); v.Str() != want {
				t.Errorf("%s = %q, want %q", k, v.Str(), want)
			}
		}
		recs := ld.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords()
		for i := 0; i < recs.Len(); i++ {
			bodies = append(bodies, recs.At(i).Body().Str())
			if strings.HasPrefix(recs.At(i).Body().Str(), "2026-10-19 12:00:01") && recs.At(i).SeverityText() != "ERROR" {
				t.Errorf("level of the joined record: %q", recs.At(i).SeverityText())
			}
		}
	}
	want := "2026-10-19 12:00:01.123 ERROR [main] c.e.Checkout - failed for user=***\njava.lang.IllegalStateException: Order 42 has no lines\n\tat com.example.shop.OrderService.total(OrderService.java:57)\n\t... 3 more"
	if len(bodies) != 2 || bodies[0] != want || bodies[1] != "2026-10-19 12:00:03.000 INFO [main] done" {
		t.Fatalf("bodies:\n%q", bodies)
	}
}

// restart stops the daemon (every open stream ends) and brings it back on the same socket.
func (f *fakeDocker) restart(t *testing.T, down time.Duration, whileDown func()) {
	t.Helper()
	f.srv.Close()
	os.Remove(f.sock)
	whileDown()
	time.Sleep(down)
	ln, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	f.srv = &http.Server{Handler: http.HandlerFunc(f.serve)}
	go f.srv.Serve(ln) //nolint:errcheck
}

func TestReconnectsWhenDockerRestarts(t *testing.T) {
	fake, endpoint := newFakeDocker(t)
	id := "web0000000000000000000000000000000000000000000000000000000000001"
	fake.add(id, "web", nil, false)
	sink := new(consumertest.LogsSink)
	r := startReceiver(t, endpoint, "", sink, "none")
	defer r.Shutdown(context.Background()) //nolint:errcheck
	waitFor(t, "tail", func() bool { fake.mu.Lock(); defer fake.mu.Unlock(); return len(fake.sinces[id]) == 1 })

	fake.write(id, fakeLine{time.Now(), 1, "w1 before restart"})
	waitFor(t, "w1", func() bool { return len(received(sink)) == 1 })

	fake.restart(t, 300*time.Millisecond, func() {
		fake.write(id, fakeLine{time.Now(), 1, "w2 while docker was down"})
	})
	fake.write(id, fakeLine{time.Now(), 1, "w3 after restart"})
	waitFor(t, "reconnect", func() bool { return len(received(sink)) >= 3 })
	time.Sleep(300 * time.Millisecond)
	var bodies []string
	for _, g := range received(sink) {
		bodies = append(bodies, g.body)
	}
	if strings.Join(bodies, "|") != "w1 before restart|w2 while docker was down|w3 after restart" {
		t.Fatalf("got %q", bodies)
	}
}
