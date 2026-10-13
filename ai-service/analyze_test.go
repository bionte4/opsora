package main

import (
	"slices"
	"strings"
	"testing"

	"devops-allinone/shared/store"
)

func TestFailurePromptNumbersAndCleansLines(t *testing.T) {
	b := store.Build{Repository: "o/r", Error: `step "test" exited with code 1`}
	step := store.Step{Name: "test", Status: store.StepFailed}
	lines := []store.LogLine{
		{ID: 40, Line: "\x1b[31m--- FAIL: TestQueue\x1b[0m"},
		{ID: 41, Line: strings.Repeat("x", 500)},
	}
	p := failurePrompt(b, step, lines)
	for _, want := range []string{
		`Failed step: "test" (status failed)`,
		`Build error: step "test" exited with code 1`,
		"1| --- FAIL: TestQueue\n",
		"2| " + strings.Repeat("x", maxLineChars) + "...\n",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q\n%s", want, p)
		}
	}
	if strings.Contains(p, "\x1b") {
		t.Errorf("prompt still has ANSI escapes:\n%q", p)
	}
}

func TestParseAnswerMapsCitedLines(t *testing.T) {
	lines := []store.LogLine{{ID: 10}, {ID: 11}, {ID: 12}, {ID: 13}}
	raw := `{"summary":" Step test failed. ","cause":"want 3, got 2","lines":[2, 0, 9, 2, 4, 1, 3]}`
	summary, cause, ids, err := parseAnswer(raw, lines)
	if err != nil {
		t.Fatal(err)
	}
	if summary != "Step test failed." || cause != "want 3, got 2" {
		t.Fatalf("summary=%q cause=%q", summary, cause)
	}
	if want := []int64{11, 13, 10}; !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v (in range, deduplicated, at most %d)", ids, want, maxCited)
	}
}

func TestParseAnswerRejectsBadOutput(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":      "The build failed because...",
		"empty summary": `{"summary":"  ","cause":"x","lines":[]}`,
	} {
		if _, _, _, err := parseAnswer(raw, nil); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
