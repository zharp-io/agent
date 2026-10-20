package dockerlogsreceiver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// dockerClient is the handful of Engine API calls the receiver needs, over the local socket.
// The API is versionless here on purpose: an unversioned path is served at the daemon's own
// version, so there is nothing to negotiate and nothing to go stale.
type dockerClient struct {
	http    *http.Client
	timeout time.Duration
}

func newDockerClient(endpoint string, timeout time.Duration) (*dockerClient, error) {
	dial, err := dialerFor(endpoint)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &dockerClient{
		http: &http.Client{Transport: &http.Transport{
			DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     30 * time.Second,
		}},
		timeout: timeout,
	}, nil
}

// dialerFor turns unix:///path or npipe:////./pipe/name into a dial function.
func dialerFor(endpoint string) (func(ctx context.Context) (net.Conn, error), error) {
	switch {
	case strings.HasPrefix(endpoint, "unix://"):
		path := strings.TrimPrefix(endpoint, "unix://")
		return func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}, nil
	case strings.HasPrefix(endpoint, "npipe://"):
		// npipe:////./pipe/docker_engine -> \\.\pipe\docker_engine
		path := strings.ReplaceAll(strings.TrimPrefix(endpoint, "npipe://"), "/", `\`)
		return func(ctx context.Context) (net.Conn, error) { return dialPipe(ctx, path) }, nil
	}
	return nil, fmt.Errorf("unsupported docker endpoint %q", endpoint)
}

type containerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
	State  string            `json:"State"`
}

type containerInfo struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Tty    bool              `json:"Tty"`
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
		Env    []string          `json:"Env"`
	} `json:"Config"`
	State struct {
		Running   bool   `json:"Running"`
		StartedAt string `json:"StartedAt"`
	} `json:"State"`
	HostConfig struct {
		LogConfig struct {
			Type string `json:"Type"`
		} `json:"LogConfig"`
	} `json:"HostConfig"`
}

type dockerEvent struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

func (c *dockerClient) getJSON(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func apiError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	var msg struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &msg) == nil && msg.Message != "" {
		return fmt.Errorf("docker API %s: %s", resp.Status, msg.Message)
	}
	return fmt.Errorf("docker API %s: %s", resp.Status, strings.TrimSpace(string(body)))
}

func (c *dockerClient) listRunning(ctx context.Context) ([]containerSummary, error) {
	var out []containerSummary
	err := c.getJSON(ctx, "/containers/json", &out)
	return out, err
}

func (c *dockerClient) inspect(ctx context.Context, id string) (*containerInfo, error) {
	var out containerInfo
	if err := c.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// stream opens a long-lived GET; the caller closes the body. It ends when ctx is cancelled,
// the container stops (logs) or the daemon goes away.
func (c *dockerClient) stream(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, apiError(resp)
	}
	return resp, nil
}

// logs follows a container's stdout and stderr with timestamps, from since onwards (zero means
// from the beginning).
func (c *dockerClient) logs(ctx context.Context, id string, since time.Time) (*http.Response, error) {
	q := url.Values{}
	q.Set("follow", "1")
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	q.Set("timestamps", "1")
	if !since.IsZero() {
		q.Set("since", fmt.Sprintf("%d.%09d", since.Unix(), since.Nanosecond()))
	}
	return c.stream(ctx, "/containers/"+url.PathEscape(id)+"/logs?"+q.Encode())
}

// events streams container start and destroy events from now on.
func (c *dockerClient) events(ctx context.Context) (*http.Response, error) {
	filters := `{"type":["container"],"event":["start","destroy"]}`
	return c.stream(ctx, "/events?filters="+url.QueryEscape(filters))
}
