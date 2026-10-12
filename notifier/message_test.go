package main

import (
	"strings"
	"testing"
	"time"

	"devops-allinone/shared/store"
)

func TestFormatMessageFailure(t *testing.T) {
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	end := start.Add(83 * time.Second)
	b := store.Build{
		ID: 7, Repository: "bionte4/opsora", Ref: "refs/heads/main",
		CommitSHA: "4a68cfd0123456789", Actor: "bionte4", Status: store.StatusFailed,
		Error: `step "test" exited with code 1`, StartedAt: &start, FinishedAt: &end,
	}
	step := &store.Step{Position: 3, Name: "test", Status: store.StepFailed}
	tail := []store.LogLine{{Line: "--- FAIL: TestX"}, {Line: "want <nil>, got &errors.errorString"}}

	msg := formatMessage(b, step, tail, "https://ci.example.com")
	for _, want := range []string{
		"<b>Build #7 failed</b>",
		"Repo: <b>bionte4/opsora</b>",
		"Ref: <code>main</code>",
		`<a href="https://github.com/bionte4/opsora/commit/4a68cfd0123456789">4a68cfd</a> by bionte4`,
		"Duration: 1m23s",
		"Failed step: <b>test</b> (failed)",
		"Error: step &#34;test&#34; exited with code 1",
		"<pre>--- FAIL: TestX\nwant &lt;nil&gt;, got &amp;errors.errorString</pre>",
		`<a href="https://ci.example.com/api/builds/7/logs?format=text">Full logs</a>`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q\n%s", want, msg)
		}
	}
}

func TestFormatMessageSuccess(t *testing.T) {
	b := store.Build{ID: 8, Repository: "o/r", Ref: "refs/tags/v1.0", CommitSHA: "abc", Status: store.StatusSuccess}
	msg := formatMessage(b, nil, nil, "")
	if !strings.Contains(msg, "<b>Build #8 passed</b>") || !strings.Contains(msg, "tag v1.0") {
		t.Fatalf("unexpected message:\n%s", msg)
	}
	if strings.Contains(msg, "Full logs") || strings.Contains(msg, "<pre>") {
		t.Fatalf("success without public URL should have no links or tail:\n%s", msg)
	}
}

func TestFormatMessageFitsTelegramLimit(t *testing.T) {
	b := store.Build{ID: 1, Repository: "o/r", Status: store.StatusFailed}
	line := strings.Repeat("<&>", 200)
	tail := make([]store.LogLine, 15)
	for i := range tail {
		tail[i].Line = line
	}
	msg := formatMessage(b, &store.Step{Name: "x", Status: store.StepFailed}, tail, "")
	if n := len([]rune(msg)); n > maxMessage {
		t.Fatalf("message is %d characters, limit %d", n, maxMessage)
	}
	if !strings.Contains(msg, "<pre>&lt;&amp;&gt;") || !strings.HasSuffix(msg, "...</pre>") {
		t.Fatalf("expected a clipped, escaped tail:\n%s", msg)
	}
}
