package logrules

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// join runs lines through a joiner and returns the record texts.
func join(t *testing.T, mode Multiline, text string) []string {
	t.Helper()
	j := NewJoiner[int](mode)
	now := time.Unix(0, 0)
	var out []string
	for i, l := range strings.Split(strings.TrimPrefix(text, "\n"), "\n") {
		for _, r := range j.Add(i, l, now) {
			out = append(out, r.Text)
		}
	}
	for _, r := range j.Flush() {
		out = append(out, r.Text)
	}
	return out
}

func expectRecords(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d:\n%s", len(got), len(want), strings.Join(got, "\n-----\n"))
	}
	for i := range want {
		if got[i] != strings.TrimPrefix(want[i], "\n") {
			t.Errorf("record %d:\n got: %q\nwant: %q", i, got[i], want[i])
		}
	}
}

const javaTrace = `2026-10-19 12:00:01.123 ERROR 1 --- [nio-8080-exec-1] o.a.c.c.C.[.[.[/].[dispatcherServlet]    : Servlet.service() for servlet [dispatcherServlet] threw exception
java.lang.IllegalStateException: Order 42 has no lines
	at com.example.shop.OrderService.total(OrderService.java:57)
	at com.example.shop.OrderController.checkout(OrderController.java:31)
	at java.base/jdk.internal.reflect.DirectMethodHandleAccessor.invoke(DirectMethodHandleAccessor.java:103)
	at java.base/java.lang.reflect.Method.invoke(Method.java:580)
Caused by: java.sql.SQLException: Connection is not available, request timed out after 30000ms.
	at com.zaxxer.hikari.pool.HikariPool.createTimeoutException(HikariPool.java:696)
	at com.zaxxer.hikari.pool.HikariPool.getConnection(HikariPool.java:181)
	... 52 common frames omitted
Caused by: java.net.ConnectException: Connection refused
	at java.base/sun.nio.ch.Net.pollConnect(Native Method)
	... 3 more`

func TestJavaStackTrace(t *testing.T) {
	got := join(t, AutoMultiline, javaTrace+"\n2026-10-19 12:00:02.000  INFO 1 --- [main] c.e.shop.App : recovered")
	expectRecords(t, got, javaTrace, "2026-10-19 12:00:02.000  INFO 1 --- [main] c.e.shop.App : recovered")
}

func TestJavaUncaughtWithoutLogger(t *testing.T) {
	trace := `Exception in thread "main" java.lang.NullPointerException: Cannot invoke "String.length()" because "s" is null
	at Main.main(Main.java:5)`
	expectRecords(t, join(t, AutoMultiline, "Starting\n"+trace), "Starting", trace)
}

const pythonTrace = `2026-10-19 12:00:03,456 ERROR [app.views] payment failed
Traceback (most recent call last):
  File "/app/app/views.py", line 41, in charge
    resp = client.charge(order.total)
  File "/usr/local/lib/python3.12/site-packages/stripe/api.py", line 88, in charge
    raise CardError("declined")
stripe.error.CardError: declined

During handling of the above exception, another exception occurred:

Traceback (most recent call last):
  File "/app/app/views.py", line 44, in charge
    log_failure(order)
ZeroDivisionError: division by zero`

func TestPythonTraceback(t *testing.T) {
	got := join(t, AutoMultiline, pythonTrace+"\nINFO:werkzeug:127.0.0.1 - - \"GET /health HTTP/1.1\" 200 -")
	expectRecords(t, got, pythonTrace, `INFO:werkzeug:127.0.0.1 - - "GET /health HTTP/1.1" 200 -`)
}

func TestPythonUncaught(t *testing.T) {
	trace := `Traceback (most recent call last):
  File "/app/main.py", line 3, in <module>
    1/0
ZeroDivisionError: division by zero`
	expectRecords(t, join(t, AutoMultiline, trace), trace)
}

const nodeTrace = `TypeError: Cannot read properties of undefined (reading 'id')
    at getUser (/app/src/users.js:12:21)
    at Layer.handle [as handle_request] (/app/node_modules/express/lib/router/layer.js:95:5)
    at next (/app/node_modules/express/lib/router/route.js:149:13)
    at process.processTicksAndRejections (node:internal/process/task_queues:95:5)`

func TestNodeTrace(t *testing.T) {
	// Uncaught: the error line opens the record.
	expectRecords(t, join(t, AutoMultiline, "Server listening on 3000\n"+nodeTrace),
		"Server listening on 3000\n"+nodeTrace) // a plain line with no timestamp can't be told apart
	// Through a logger with a timestamp, the error belongs to the log line.
	logged := "[2026-10-19T12:00:04.789Z] ERROR: request failed\n" + nodeTrace
	expectRecords(t, join(t, AutoMultiline, "2026-10-19T12:00:04.700Z INFO listening\n"+logged),
		"2026-10-19T12:00:04.700Z INFO listening", logged)
}

const goPanic = `panic: runtime error: invalid memory address or nil pointer dereference
[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x4a2b3c]

goroutine 1 [running]:
main.(*Server).handle(0x0, {0x5c1e40, 0xc000014090})
	/app/server.go:42 +0x1c
main.main()
	/app/main.go:17 +0x85
created by net/http.(*Server).Serve in goroutine 6
	/usr/local/go/src/net/http/server.go:3285 +0x4b4`

func TestGoPanic(t *testing.T) {
	got := join(t, AutoMultiline, "2026/10/19 12:00:05 serving on :8080\n"+goPanic+"\nexit status 2")
	expectRecords(t, got, "2026/10/19 12:00:05 serving on :8080", goPanic, "exit status 2")
}

func TestJSONLinesStayApart(t *testing.T) {
	got := join(t, AutoMultiline, `{"level":"info","msg":"a"}
{"level":"error","msg":"b","stack":"x"}`)
	expectRecords(t, got, `{"level":"info","msg":"a"}`, `{"level":"error","msg":"b","stack":"x"}`)
}

func TestPlainLinesStayApart(t *testing.T) {
	got := join(t, AutoMultiline, "127.0.0.1 - - [19/Oct/2026:12:00:00 +0000] \"GET / HTTP/1.1\" 200 612\n10.0.0.2 - - [19/Oct/2026:12:00:01 +0000] \"GET /x HTTP/1.1\" 404 0\nready")
	if len(got) != 3 {
		t.Fatalf("access log lines were joined: %q", got)
	}
}

func TestCustomRegexAndOff(t *testing.T) {
	m, err := ParseMultiline(`^\d{4}-`)
	if err != nil {
		t.Fatal(err)
	}
	got := join(t, m, "2026- a\nnot indented\n  x\n2026- b")
	expectRecords(t, got, "2026- a\nnot indented\n  x", "2026- b")

	off, _ := ParseMultiline("off")
	if got := join(t, off, javaTrace); len(got) != strings.Count(javaTrace, "\n")+1 {
		t.Fatalf("off joined lines: %d records", len(got))
	}
	if _, err := ParseMultiline("(["); err == nil {
		t.Fatal("bad regex accepted")
	}
}

func TestBounds(t *testing.T) {
	j := NewJoiner[int](AutoMultiline)
	j.SetLimits(3, 0)
	now := time.Now()
	var recs []Record[int]
	recs = append(recs, j.Add(0, "ERROR boom", now)...)
	for i := 0; i < 5; i++ {
		recs = append(recs, j.Add(i+1, "\tat frame", now)...)
	}
	recs = append(recs, j.Flush()...)
	if len(recs) != 2 || recs[0].Lines != 3 || recs[1].Lines != 3 {
		t.Fatalf("bounds not applied: %+v", recs)
	}

	j = NewJoiner[int](AutoMultiline)
	j.SetLimits(0, 20)
	recs = append(recs[:0], j.Add(0, "ERROR 1234567890", now)...)
	recs = append(recs, j.Add(1, "\tat 1234567890", now)...)
	recs = append(recs, j.Flush()...)
	if len(recs) != 2 {
		t.Fatalf("byte bound not applied: %+v", recs)
	}
}

func TestIdleFlush(t *testing.T) {
	j := NewJoiner[int](AutoMultiline)
	t0 := time.Unix(100, 0)
	j.Add(0, "ERROR boom", t0)
	j.Add(1, "\tat x", t0.Add(500*time.Millisecond))
	if r := j.FlushIdle(t0.Add(time.Second), time.Second); r != nil {
		t.Fatal("flushed before idle")
	}
	r := j.FlushIdle(t0.Add(1600*time.Millisecond), time.Second)
	if len(r) != 1 || r[0].Text != "ERROR boom\n\tat x" || r[0].Meta != 0 {
		t.Fatalf("idle flush: %+v", r)
	}
	if j.Pending() {
		t.Fatal("still pending")
	}
}

// The recombine expression carries the same two patterns, escaped for expr.
func TestRecombineExpr(t *testing.T) {
	e := RecombineFirstEntryExpr("body")
	if strings.Contains(e, "$") {
		t.Fatal("a $ would be expanded by the collector config")
	}
	re := regexp.MustCompile(`^body matches "(.*)" or not \(body matches "(.*)"\)$`)
	m := re.FindStringSubmatch(e)
	if m == nil {
		t.Fatalf("unexpected shape: %s", e)
	}
	unq := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, `\"`, `"`), `\\`, `\`) }
	if unq(m[1]) != AutoStartPattern || unq(m[2]) != AutoContinuationPattern {
		t.Fatal("expression patterns differ from the Go ones")
	}
}

func TestKlogLevel(t *testing.T) {
	for line, want := range map[string]string{
		`I1019 19:08:40.385039       1 replica_set.go:679] "Finished syncing"`: "INFO",
		`E1019 19:08:40.385039       1 controller.go:12] failed`:               "ERROR",
		`W1019 19:08:40.385039       1 x.go:1] slow`:                           "WARN",
	} {
		if _, got := ParseLevel(line); got != want {
			t.Errorf("%q: %s, want %s", line, got, want)
		}
	}
}
