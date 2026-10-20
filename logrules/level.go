package logrules

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Level detection is deliberately conservative: a level is set only when the line states one —
// a JSON "level"/"severity" field, a logfmt level=..., or a level word leading the line. Anything
// else is left unspecified, which the backend reads as info. stderr is not treated as error:
// plenty of programs write all their logging there.

// Severity is an OpenTelemetry severity number (plog.SeverityNumber has the same values).
type Severity int32

const (
	SeverityUnspecified Severity = 0
	SeverityTrace       Severity = 1
	SeverityDebug       Severity = 5
	SeverityInfo        Severity = 9
	SeverityInfo2       Severity = 10
	SeverityWarn        Severity = 13
	SeverityError       Severity = 17
	SeverityFatal       Severity = 21
)

var levelWords = map[string]Severity{
	"trace": SeverityTrace, "trc": SeverityTrace,
	"debug": SeverityDebug, "dbg": SeverityDebug,
	"info": SeverityInfo, "inf": SeverityInfo, "information": SeverityInfo,
	"notice": SeverityInfo2,
	"warn":   SeverityWarn, "warning": SeverityWarn, "wrn": SeverityWarn,
	"error": SeverityError, "err": SeverityError, "eror": SeverityError,
	"crit": SeverityFatal, "critical": SeverityFatal,
	"fatal": SeverityFatal, "ftl": SeverityFatal,
	"panic": SeverityFatal, "emerg": SeverityFatal, "alert": SeverityFatal,
}

// Leading words are matched more strictly than JSON fields: "Error:" at the start of a line is a
// level, but short forms like "err" or "inf" are only trusted inside a field.
var leadingWords = map[string]bool{
	"trace": true, "debug": true, "info": true, "notice": true, "warn": true, "warning": true,
	"error": true, "crit": true, "critical": true, "fatal": true, "panic": true,
}

var (
	ansiRe   = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	logfmtRe = regexp.MustCompile(`(?i)(?:^|\s)(?:level|lvl|severity)=["']?([a-z]+)`)
	dateRe   = regexp.MustCompile(`^\[?\d{4}[-/]\d{2}[-/]\d{2}`)
	timeRe   = regexp.MustCompile(`^\d{2}:\d{2}:\d{2}`)
	// klog/glog (Kubernetes components): I1019 12:00:00.000000 — the letter is the level.
	klogRe = regexp.MustCompile(`^([IWEF])\d{4} \d{2}:\d{2}:\d{2}`)
)

// jsonLevelKeys are the fields logging libraries put the level in (pino/bunyan "level",
// zap/logrus "level", GCP-style "severity", Serilog "@l", ECS "log.level").
var jsonLevelKeys = []string{"level", "severity", "lvl", "loglevel", "log.level", "@l", "levelname", "log_level"}

// ParseLevel returns the severity a line states, or Unspecified.
func ParseLevel(line string) (Severity, string) {
	s := strings.TrimSpace(line)
	if strings.ContainsRune(s, 0x1b) {
		s = strings.TrimSpace(ansiRe.ReplaceAllString(s, ""))
	}
	if s == "" {
		return SeverityUnspecified, ""
	}
	if s[0] == '{' {
		if sev, txt, ok := jsonLevel(s); ok {
			return sev, txt
		}
		return SeverityUnspecified, ""
	}
	if m := klogRe.FindStringSubmatch(s); m != nil {
		sev := map[string]Severity{"I": SeverityInfo, "W": SeverityWarn, "E": SeverityError, "F": SeverityFatal}[m[1]]
		return sev, CanonicalText(sev)
	}
	if m := logfmtRe.FindStringSubmatch(s); m != nil {
		if sev, ok := levelWords[strings.ToLower(m[1])]; ok {
			return sev, CanonicalText(sev)
		}
	}
	// A level word first, or right after a leading date and time.
	fields := strings.Fields(s)
	for i := 0; i < len(fields) && i < 3; i++ {
		f := fields[i]
		w := strings.ToLower(strings.Trim(f, "[]()<>:|-"))
		if leadingWords[w] {
			sev := levelWords[w]
			return sev, CanonicalText(sev)
		}
		if !dateRe.MatchString(f) && !timeRe.MatchString(f) {
			break
		}
	}
	return SeverityUnspecified, ""
}

func jsonLevel(s string) (Severity, string, bool) {
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) != nil {
		return 0, "", false
	}
	lower := make(map[string]any, len(obj))
	for k, v := range obj {
		lower[strings.ToLower(k)] = v
	}
	if nested, ok := lower["log"].(map[string]any); ok {
		if v, ok := nested["level"]; ok {
			lower["log.level"] = v
		}
	}
	for _, k := range jsonLevelKeys {
		v, ok := lower[k]
		if !ok {
			continue
		}
		switch val := v.(type) {
		case string:
			if sev, ok := levelWords[strings.ToLower(strings.TrimSpace(val))]; ok {
				return sev, CanonicalText(sev), true
			}
		case float64:
			if sev, ok := numericLevel(val); ok {
				return sev, CanonicalText(sev), true
			}
		}
	}
	return 0, "", false
}

// numericLevel understands pino/bunyan levels (10 trace … 60 fatal).
func numericLevel(v float64) (Severity, bool) {
	switch int(v) {
	case 10:
		return SeverityTrace, true
	case 20:
		return SeverityDebug, true
	case 30:
		return SeverityInfo, true
	case 40:
		return SeverityWarn, true
	case 50:
		return SeverityError, true
	case 60:
		return SeverityFatal, true
	}
	return 0, false
}

// CanonicalText is the level name for a severity (FATAL, ERROR, WARN, INFO, DEBUG, TRACE).
func CanonicalText(sev Severity) string {
	switch {
	case sev >= SeverityFatal:
		return "FATAL"
	case sev >= SeverityError:
		return "ERROR"
	case sev >= SeverityWarn:
		return "WARN"
	case sev >= SeverityInfo:
		return "INFO"
	case sev >= SeverityDebug:
		return "DEBUG"
	case sev >= SeverityTrace:
		return "TRACE"
	}
	return ""
}
