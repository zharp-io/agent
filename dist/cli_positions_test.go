package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The file is the one file_storage keeps for receiver filelog/pm2: "<kind>_<type>_<name>".
func TestForgetReadPositionsRemovesTheReceiversFile(t *testing.T) {
	stateDirOverride = t.TempDir()
	defer func() { stateDirOverride = "" }()
	dir := filepath.Join(stateDirOverride, "storage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"receiver_filelog_pm2", "receiver_journald_"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	forgetReadPositions("filelog/pm2")
	if fileExists(filepath.Join(dir, "receiver_filelog_pm2")) {
		t.Error("filelog/pm2's positions are still there")
	}
	if !fileExists(filepath.Join(dir, "receiver_journald_")) {
		t.Error("another receiver's positions went too")
	}
}

func TestReceiverChanged(t *testing.T) {
	content := buildConfig(configSpec{
		metricsReceivers: []string{"hostmetrics"},
		logReceivers:     []LogReceiver{pm2Receiver([]string{"/home/a/.pm2/logs"})},
	})
	if existed, changed := receiverChanged(content, pm2Receiver([]string{"/home/a/.pm2/logs"})); !existed || changed {
		t.Errorf("same receiver: existed=%v changed=%v", existed, changed)
	}
	more := pm2ReceiverWith([]string{"/home/a/.pm2/logs"}, []pm2App{{Name: "api", Paths: []string{"/srv/api/logs/out.log"}}})
	if existed, changed := receiverChanged(content, more); !existed || !changed {
		t.Errorf("receiver with more files: existed=%v changed=%v", existed, changed)
	}
	if existed, _ := receiverChanged(content, journaldReceiver()); existed {
		t.Error("journald isn't in this config")
	}
	if block := receiverBlockOf(content, "filelog/pm2"); !strings.HasPrefix(block, "\n  filelog/pm2:\n") || strings.Contains(block, "processors:") {
		t.Errorf("block: %q", block)
	}
}

// Update asks PM2 again when PM2 logs are already collected: a newly found app joins the
// receiver, its files start at their end, and the summary calls it a change, not an addition.
func TestUpdateAddsNewPM2AppsAndStartsTheirFilesAtTheEnd(t *testing.T) {
	stateDirOverride = t.TempDir()
	defer func() { stateDirOverride = "" }()
	defer func(g func(string) error, d func() []pm2Dir, a func() []pm2App) {
		grantPM2Access, pm2DirsFn, pm2CustomAppsFn = g, d, a
	}(grantPM2Access, pm2DirsFn, pm2CustomAppsFn)
	grantPM2Access = func(string) error { return nil }
	pm2DirsFn = func() []pm2Dir { return []pm2Dir{{Path: "/home/a/.pm2/logs", Apps: []string{"web"}}} }
	pm2CustomAppsFn = func() []pm2App { return nil }

	content := buildConfig(configSpec{
		metricsReceivers: []string{"hostmetrics"},
		logReceivers:     []LogReceiver{pm2Receiver([]string{"/home/a/.pm2/logs"})},
	})
	detected := []ServiceOption{{Type: "pm2", Detected: true}}

	// Nothing new: the config is what update makes of it when it finds nothing at all.
	baseline, err := mergeSources(content, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	res, err := mergeSources(content, nil, detected, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.config != baseline.config || len(res.resetPositions) != 0 || len(res.migrated) != len(baseline.migrated) {
		t.Errorf("unchanged PM2 still touched the config: reset=%v migrated=%v", res.resetPositions, res.migrated)
	}

	// An app with its own log files appears.
	pm2CustomAppsFn = func() []pm2App { return []pm2App{{Name: "api", Paths: []string{"/srv/api/logs/out.log"}}} }
	res, err = mergeSources(content, nil, detected, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.config, "'/srv/api/logs/out.log'") {
		t.Error("the app's file isn't in the receiver")
	}
	if strings.Join(res.resetPositions, ",") != "filelog/pm2" {
		t.Errorf("positions to reset: %v", res.resetPositions)
	}
	if strings.Join(res.migrated, "|") != "PM2 app logs: now api, web" {
		t.Errorf("summary: %v", res.migrated)
	}
	for _, it := range res.added {
		if it.Type == "pm2" {
			t.Error("PM2 was already collected; it is a change, not an addition")
		}
	}
}

// PM2 writes its date in front of every line; it has to come off before lines are joined, or each
// stack frame becomes its own record.
func TestPM2ReceiverTakesPM2sDateOffBeforeJoining(t *testing.T) {
	block := pm2Receiver([]string{"/home/a/.pm2/logs"}).Block
	prefix := strings.Index(block, "PM2's log_date_format prefix")
	recombine := strings.Index(block, "type: recombine")
	if prefix < 0 || recombine < 0 || prefix > recombine {
		t.Fatalf("the PM2 date must be handled before recombine:\n%s", block)
	}
	if !strings.Contains(block, "layout: '2006-01-02 15:04:05.999999999 Z07:00'") {
		t.Error("PM2's date isn't parsed as the record's time")
	}
}
