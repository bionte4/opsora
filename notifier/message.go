package main

import (
	"fmt"
	"html"
	"strings"
	"time"

	"devops-allinone/shared/store"
)

// Telegram rejects messages over 4096 characters; the log tail gets whatever
// the header leaves, minus room for the closing tags and the link.
const (
	maxMessage          = 4096
	maxTail             = 2500
	maxTailWithAnalysis = 1500
	maxLine             = 300
	maxCitedLine        = 200
)

func formatMessage(b store.Build, failed *store.Step, tail []store.LogLine, analysis *store.Analysis, publicURL string) string {
	var sb strings.Builder
	result := "passed"
	if b.Status != store.StatusSuccess {
		result = "failed"
	}
	fmt.Fprintf(&sb, "<b>Build #%d %s</b>\n", b.ID, result)
	fmt.Fprintf(&sb, "Repo: <b>%s</b>\n", esc(b.Repository))
	if ref := shortRef(b.Ref); ref != "" {
		fmt.Fprintf(&sb, "Ref: <code>%s</code>\n", esc(ref))
	}
	if b.CommitSHA != "" {
		commitURL := "https://github.com/" + b.Repository + "/commit/" + b.CommitSHA
		fmt.Fprintf(&sb, "Commit: <a href=\"%s\">%s</a>", esc(commitURL), esc(short(b.CommitSHA)))
		if b.Actor != "" {
			fmt.Fprintf(&sb, " by %s", esc(b.Actor))
		}
		sb.WriteString("\n")
	}
	if d := duration(b); d != "" {
		fmt.Fprintf(&sb, "Duration: %s\n", d)
	}
	if failed != nil {
		fmt.Fprintf(&sb, "Failed step: <b>%s</b> (%s)\n", esc(failed.Name), esc(failed.Status))
	}
	if b.Error != "" {
		fmt.Fprintf(&sb, "Error: %s\n", esc(clip(b.Error, 500)))
	}

	tailBudget := maxTail
	if analysis != nil {
		writeAnalysis(&sb, analysis)
		tailBudget = maxTailWithAnalysis
	}

	if text := escapedTail(tail, tailBudget); text != "" {
		fmt.Fprintf(&sb, "<pre>%s</pre>\n", text)
	}

	if publicURL != "" {
		logsURL := fmt.Sprintf("%s/api/builds/%d/logs?format=text", publicURL, b.ID)
		fmt.Fprintf(&sb, "<a href=\"%s\">Full logs</a>", esc(logsURL))
	}

	msg := strings.TrimRight(sb.String(), "\n")
	if len([]rune(msg)) > maxMessage {
		// Escaping can grow text past its budget; drop whole sections rather
		// than cut through a tag. The log tail goes first, then the analysis.
		if len(tail) > 0 {
			return formatMessage(b, failed, nil, analysis, publicURL)
		}
		if analysis != nil {
			return formatMessage(b, failed, nil, nil, publicURL)
		}
	}
	return msg
}

// writeAnalysis quotes the log lines the model cited, so a reader can check
// the explanation against the log instead of trusting it.
func writeAnalysis(sb *strings.Builder, a *store.Analysis) {
	fmt.Fprintf(sb, "\n<b>AI analysis</b> <i>(%s, may be wrong)</i>\n", esc(a.Model))
	fmt.Fprintf(sb, "%s\n", esc(a.Summary))
	if a.Cause != "" {
		fmt.Fprintf(sb, "Likely cause: %s\n", esc(a.Cause))
	}
	if len(a.Lines) > 0 {
		quoted := make([]string, len(a.Lines))
		for i, l := range a.Lines {
			quoted[i] = esc(clip(l.Line, maxCitedLine))
		}
		fmt.Fprintf(sb, "<blockquote>%s</blockquote>\n", strings.Join(quoted, "\n"))
	}
	sb.WriteString("\n")
}

// escapedTail keeps the newest lines whose escaped form fits in budget, so
// entities are never cut in half.
func escapedTail(tail []store.LogLine, budget int) string {
	var kept []string
	size := 0
	for i := len(tail) - 1; i >= 0; i-- {
		line := esc(clip(tail[i].Line, maxLine))
		if size+len(line)+1 > budget {
			break
		}
		kept = append(kept, line)
		size += len(line) + 1
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return strings.Join(kept, "\n")
}

func esc(s string) string { return html.EscapeString(s) }

func shortRef(ref string) string {
	if branch, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return branch
	}
	if tag, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
		return "tag " + tag
	}
	return ref
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "..."
}

func duration(b store.Build) string {
	if b.StartedAt == nil || b.FinishedAt == nil {
		return ""
	}
	return b.FinishedAt.Sub(*b.StartedAt).Round(time.Second).String()
}
