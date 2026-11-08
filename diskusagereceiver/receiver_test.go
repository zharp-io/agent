package diskusagereceiver

import (
	"testing"
	"time"

	"github.com/zharp-io/agent/diskusagereceiver/scan"
)

func TestMetricsCarryFoldersOwnersAndTheScan(t *testing.T) {
	res := scan.Result{Privileged: true, Mounts: []scan.Mount{{
		Mount: "/", Bytes: 1000, Files: 7, Complete: true, Seconds: 2.5,
		Folders: []scan.Folder{{Path: "/var", Bytes: 800, Owner: "root", Children: []scan.Folder{{Path: "/var/lib", Bytes: 700, Owner: "root"}}}},
		Owners:  []scan.Owner{{Owner: "postgres", Bytes: 600}, {Owner: "root", Bytes: 400}},
	}}}
	md := Metrics(res, time.Unix(1700000000, 0))
	ms := md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()

	byName := map[string]int{}
	for i := 0; i < ms.Len(); i++ {
		byName[ms.At(i).Name()] = ms.At(i).Gauge().DataPoints().Len()
	}
	if byName["zharp.disk.folder.size"] != 2 || byName["zharp.disk.owner.size"] != 2 || byName["zharp.disk.scan"] != 1 {
		t.Fatalf("points per metric: %v", byName)
	}
	child := ms.At(0).Gauge().DataPoints().At(1).Attributes()
	if p, _ := child.Get("parent"); p.Str() != "/var" {
		t.Fatalf("a child names its parent: %v", child.AsRaw())
	}
	scanAttrs := ms.At(2).Gauge().DataPoints().At(0).Attributes()
	if v, _ := scanAttrs.Get("privileged"); !v.Bool() {
		t.Fatal("the scan says it could read everything")
	}
}
