package dockerlogsreceiver

import (
	"go.opentelemetry.io/collector/pdata/plog"

	"github.com/zharp-io/agent/logrules"
)

// parseLevel returns the severity a line states, or Unspecified. The rules live in logrules so
// every log source detects levels the same way.
func parseLevel(line string) (plog.SeverityNumber, string) {
	sev, txt := logrules.ParseLevel(line)
	return plog.SeverityNumber(sev), txt
}
