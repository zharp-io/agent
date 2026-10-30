package dockerlogsreceiver

import (
	"testing"
	"time"
)

func TestExitAfterOOMIsReportedAsOOM(t *testing.T) {
	runs := newRunTracker()
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	runs.started("abc", start)
	runs.oom("abc", start.Add(4*time.Minute))

	var e stateEvent
	e.Action = "die"
	e.Actor.ID = "abc"
	e.Actor.Attributes = map[string]string{"name": "pricing", "image": "pricing:1", "exitCode": "137"}
	e.TimeNano = start.Add(4*time.Minute + time.Second).UnixNano()

	began, oom := runs.died("abc", e.time())
	if !began.Equal(start) || !oom {
		t.Fatalf("died: start %v oom %v", began, oom)
	}
	got := statesByName(t, exitMetrics(e, began, oom))["pricing"]
	want := map[string]any{
		"container.state": "exited", "container.exit_code": "137", "container.oom_killed": "true",
		"container.started_at": "2026-10-30T12:00:00Z", "container.finished_at": "2026-10-30T12:04:01Z",
		"container.created_at": "", "container.ports": "",
	}
	for k, v := range want {
		if got.state[k] != v {
			t.Errorf("%s = %v, want %v", k, got.state[k], v)
		}
	}
	if got.resource["container.id"] != "abc" || got.resource["container.image.name"] != "pricing:1" {
		t.Errorf("resource: %v", got.resource)
	}

	// The OOM is used up: the next exit, a minute later with no oom event, is a plain exit.
	if _, oom := runs.died("abc", e.time().Add(time.Minute)); oom {
		t.Error("an old oom event counted for a later exit")
	}
}
