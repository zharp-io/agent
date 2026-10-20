// Package logrules holds what every Zharp log source shares: joining multi-line records (stack
// traces), the exclude/mask processing rules with their built-in masks, the label and
// annotation contract that names a container's service, environment, version and source, and
// level detection.
//
// It has no dependencies so the Docker receiver, the zharp_logs processor and the CLI that
// writes collector configs can all use the same rules.
package logrules

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Bounds on one joined record. A trace longer than this is cut into several records; the
// lines after the cut keep being joined into the next one.
const (
	DefaultMaxLines = 1000
	DefaultMaxBytes = 256 * 1024
	// DefaultIdle is how long a record waits for another line before it is sent.
	DefaultIdle = time.Second
)

// The "auto" rules. A line that matches autoStart begins a new record. Otherwise it continues
// the record before it when it matches autoContinuation: it is indented, blank, or has one of
// the shapes stack traces print without indentation. Anything else is a record of its own.
//
// These two expressions are the single source of the rules: the Go joiner uses them, and the
// CLI writes them into the stanza recombine operator for file and journal sources
// (RecombineFirstEntryExpr). They avoid "$" (written as \z) so they survive the collector's
// ${...} config expansion unchanged.
const (
	AutoStartPattern = `^(?:` +
		// ISO 8601 and friends: 2026-10-19T12:00:00, [2026-10-19 12:00:00, 2026/10/19 12:00:00
		`\[?\d{4}[-/]\d{2}[-/]\d{2}[T ]\d{2}:\d{2}` +
		// 19.10.2026 12:00, 10/19/2026 12:00
		`|\[?\d{2}[-/.]\d{2}[-/.]\d{4}[ T]\d{2}:\d{2}` +
		// time only: 12:00:00
		`|\[?\d{2}:\d{2}:\d{2}` +
		// syslog: Oct 19 12:00:00
		`|[A-Z][a-z]{2} {1,2}\d{1,2} \d{2}:\d{2}:\d{2}` +
		// Apache/CLF: [19/Oct/2026:12:00:00, Tomcat: 19-Oct-2026 12:00:00
		`|\[?\d{1,2}[/-][A-Z][a-z]{2}[/-]\d{4}[: ]\d{2}:` +
		// RFC 1123: Mon, 19 Oct 2026
		`|\[?[A-Z][a-z]{2}, \d{1,2} [A-Z][a-z]{2} \d{4}` +
		// Redis: 1:M 19 Oct 2026 12:00:00.123
		`|\d+:[XCSM] \d{1,2} [A-Z][a-z]{2} \d{4}` +
		// klog/glog: I1019 12:00:00.000000
		`|[IWEF]\d{4} \d{2}:\d{2}:\d{2}` +
		// Ruby Logger: I, [2026-10-19T12:00:00
		`|[DIWEFA], \[\d{4}-` +
		// a level word, upper case or bracketed: ERROR, WARN:, [INFO], [error]
		`|\[?(?:TRACE|DEBUG|INFO|NOTICE|WARN|WARNING|ERROR|ERR|CRIT|CRITICAL|SEVERE|FATAL|PANIC)(?:\]|:|\s|\z)` +
		`|\[(?:trace|debug|info|notice|warn|warning|error|crit|critical|fatal)\]` +
		// logfmt
		`|(?:time|ts|level|lvl|msg)=` +
		// a JSON object
		`|\{` +
		// Go and Java crashes printed without a logger
		`|panic: |fatal error: |Exception in thread ` +
		`)`

	AutoContinuationPattern = `^(?:` +
		// indented (Java "\tat", Node "    at", Python "  File", Go "\t/path.go:12") or blank
		`[ \t]|\z` +
		// Java
		`|Caused by: |Suppressed: |\.\.\. \d+ (?:more|common frames omitted)` +
		// Python
		`|Traceback \(most recent call last\):|During handling of the above exception|The above exception was the direct cause` +
		// an exception line: java.lang.IllegalStateException: boom, ZeroDivisionError: ..., TypeError: ...
		`|(?:[A-Za-z_][\w\x24]*\.)*[\w\x24]*(?:Exception|Error|Throwable|Warning|Exit|Interrupt|Failure)(?::|\z)` +
		// Go
		`|goroutine \d+ \[|\[signal |created by ` +
		`|(?:[\w.\-]+/)*[\w\-]+\.(?:\(\*?\w+\)\.)?[\w.\-]+\(.*\)\z` +
		`)`
)

var (
	autoStart        = regexp.MustCompile(AutoStartPattern)
	autoContinuation = regexp.MustCompile(AutoContinuationPattern)
)

// RecombineFirstEntryExpr is the stanza recombine operator's is_first_entry expression for the
// auto rules, applied to field (e.g. "body"). Backslashes are doubled for the expr string
// literal; the result goes in a single-quoted YAML scalar.
func RecombineFirstEntryExpr(field string) string {
	q := func(p string) string {
		p = strings.ReplaceAll(p, `\`, `\\`)
		return strings.ReplaceAll(p, `"`, `\"`)
	}
	return fmt.Sprintf(`%s matches "%s" or not (%s matches "%s")`, field, q(AutoStartPattern), field, q(AutoContinuationPattern))
}

// RecombineFirstEntryRegexExpr is is_first_entry for a custom line-start regex.
func RecombineFirstEntryRegexExpr(field, pattern string) string {
	p := strings.ReplaceAll(strings.ReplaceAll(pattern, `\`, `\\`), `"`, `\"`)
	return fmt.Sprintf(`%s matches "%s"`, field, p)
}

// Multiline says how lines are joined into records.
type Multiline struct {
	off   bool
	start *regexp.Regexp // custom line-start regex; nil with off=false means auto
	raw   string
}

// AutoMultiline is the default.
var AutoMultiline = Multiline{raw: "auto"}

// ParseMultiline reads a zharp.logs.multiline value: "" or "auto" for the auto rules,
// "off" (or "none", "false") for one record per line, anything else a regex matching the
// first line of each record.
func ParseMultiline(s string) (Multiline, error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "auto", "true", "on":
		return AutoMultiline, nil
	case "off", "none", "false", "no":
		return Multiline{off: true, raw: "off"}, nil
	}
	re, err := regexp.Compile(s)
	if err != nil {
		return Multiline{}, fmt.Errorf("multiline: %w", err)
	}
	return Multiline{start: re, raw: s}, nil
}

func (m Multiline) String() string {
	if m.raw == "" {
		return "auto"
	}
	return m.raw
}

// Off reports whether every line is its own record.
func (m Multiline) Off() bool { return m.off }

// IsAuto reports whether the auto rules apply.
func (m Multiline) IsAuto() bool { return !m.off && m.start == nil }

// StartsRecord reports whether line begins a new record given that a record is pending.
func (m Multiline) StartsRecord(line string) bool {
	switch {
	case m.off:
		return true
	case m.start != nil:
		return m.start.MatchString(line)
	}
	if autoStart.MatchString(line) {
		return true
	}
	return !autoContinuation.MatchString(line)
}

// Record is one joined record: the metadata of its first line and the text of all of them.
type Record[T any] struct {
	Meta  T
	Text  string
	Lines int
}

// Joiner joins the lines of one stream (one container's stdout, one file) into records. It is
// not safe for concurrent use; keep one per stream.
type Joiner[T any] struct {
	mode     Multiline
	maxLines int
	maxBytes int

	pending *Record[T]
	sb      strings.Builder
	last    time.Time
}

// NewJoiner returns a joiner with the default bounds.
func NewJoiner[T any](mode Multiline) *Joiner[T] {
	return &Joiner[T]{mode: mode, maxLines: DefaultMaxLines, maxBytes: DefaultMaxBytes}
}

// SetLimits changes the bounds (zero keeps the default).
func (j *Joiner[T]) SetLimits(maxLines, maxBytes int) {
	if maxLines > 0 {
		j.maxLines = maxLines
	}
	if maxBytes > 0 {
		j.maxBytes = maxBytes
	}
}

// SetMode switches the rules; a pending record is kept.
func (j *Joiner[T]) SetMode(m Multiline) { j.mode = m }

// Add feeds one line. It returns the records it completed: the one before when line starts a
// new record, or the current one when it has reached its bounds.
func (j *Joiner[T]) Add(meta T, line string, now time.Time) []Record[T] {
	var out []Record[T]
	if j.mode.off {
		return append(out, Record[T]{Meta: meta, Text: line, Lines: 1})
	}
	if j.pending != nil && !j.mode.StartsRecord(line) {
		if j.pending.Lines+1 <= j.maxLines && j.sb.Len()+1+len(line) <= j.maxBytes {
			j.sb.WriteByte('\n')
			j.sb.WriteString(line)
			j.pending.Lines++
			j.last = now
			return nil
		}
		// Over the bounds: send what there is and carry on in a new record.
	}
	if r, ok := j.take(); ok {
		out = append(out, r)
	}
	if len(line) > j.maxBytes {
		line = line[:j.maxBytes]
	}
	j.pending = &Record[T]{Meta: meta, Lines: 1}
	j.sb.WriteString(line)
	j.last = now
	return out
}

// Flush returns the pending record, if any.
func (j *Joiner[T]) Flush() []Record[T] {
	if r, ok := j.take(); ok {
		return []Record[T]{r}
	}
	return nil
}

// FlushIdle returns the pending record when no line was added to it for idle.
func (j *Joiner[T]) FlushIdle(now time.Time, idle time.Duration) []Record[T] {
	if j.pending == nil || now.Sub(j.last) < idle {
		return nil
	}
	return j.Flush()
}

// Pending reports whether a record is waiting for more lines.
func (j *Joiner[T]) Pending() bool { return j.pending != nil }

// LastAt is when a line was last added.
func (j *Joiner[T]) LastAt() time.Time { return j.last }

func (j *Joiner[T]) take() (Record[T], bool) {
	if j.pending == nil {
		return Record[T]{}, false
	}
	r := *j.pending
	r.Text = strings.TrimRight(j.sb.String(), " \t\r\n")
	j.pending = nil
	j.sb.Reset()
	return r, true
}
