package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"devops-allinone/shared/store"
)

const commandPoll = 25 * time.Second

// chatops calls the API. The allowlist lives there, so every chat is
// forwarded and a rejected chat gets the API's error back.
type chatops struct {
	baseURL string
	token   string
	client  *http.Client
}

func newChatops(baseURL, token string) *chatops {
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(token) == "" {
		return nil
	}
	return &chatops{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *chatops) call(ctx context.Context, method, path, chatID string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Opsora-Chat-Id", chatID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("api unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return fmt.Errorf("api: read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return &apiError{status: resp.StatusCode, msg: e.Error}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

// parseCommand splits "/rerun@opsora_bot 17" into ("rerun", "17").
func parseCommand(text string) (name, arg string, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	head, rest, _ := strings.Cut(text, " ")
	name = strings.TrimPrefix(head, "/")
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	return strings.ToLower(name), strings.TrimSpace(rest), name != ""
}

func (n *notifier) commandLoop(ctx context.Context) {
	offset := n.skipPendingCommands(ctx)
	log.Printf("chat commands listening")
	for ctx.Err() == nil {
		pollCtx, cancel := context.WithTimeout(ctx, commandPoll+10*time.Second)
		updates, err := n.telegram.Updates(pollCtx, offset, int(commandPoll.Seconds()))
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("telegram updates: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			n.handleCommand(ctx, u)
		}
	}
}

// skipPendingCommands advances past updates that arrived while the notifier
// was down, so an old /rerun is not run on startup.
func (n *notifier) skipPendingCommands(ctx context.Context) int64 {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	updates, err := n.telegram.Updates(ctx, 0, 0)
	if err != nil {
		log.Printf("telegram updates: skip backlog: %v", err)
		return 0
	}
	var offset int64
	for _, u := range updates {
		if u.UpdateID >= offset {
			offset = u.UpdateID + 1
		}
	}
	if offset > 0 {
		log.Printf("skipped %d pending telegram updates", len(updates))
	}
	return offset
}

func (n *notifier) handleCommand(ctx context.Context, u tgUpdate) {
	name, arg, ok := parseCommand(u.Message.Text)
	if !ok {
		return
	}
	chatID := strconv.FormatInt(u.Message.Chat.ID, 10)
	reply := n.runCommand(ctx, name, arg, chatID)
	sendCtx, cancel := context.WithTimeout(ctx, sendBudget)
	defer cancel()
	if err := n.telegram.SendTo(sendCtx, chatID, reply); err != nil {
		log.Printf("chat=%s reply /%s: %v", chatID, name, err)
	}
}

func (n *notifier) runCommand(ctx context.Context, name, arg, chatID string) string {
	if n.commands == nil {
		return "Chat commands are not configured."
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	switch name {
	case "help", "start":
		return helpText()
	case "status":
		return n.cmdStatus(ctx, chatID, arg)
	case "logs":
		return n.cmdLogs(ctx, chatID, arg)
	case "rerun":
		return n.cmdRerun(ctx, chatID, arg)
	case "rollback":
		return n.cmdRollback(ctx, chatID, arg)
	default:
		return "Unknown command. " + helpText()
	}
}

func helpText() string {
	return "Commands:\n" +
		"/status — latest builds\n" +
		"/status &lt;id&gt; — one build\n" +
		"/logs &lt;id&gt; — last log lines\n" +
		"/rerun &lt;id&gt; — run that commit again\n" +
		"/rollback [repository] — restore the active release"
}

func (n *notifier) cmdStatus(ctx context.Context, chatID, arg string) string {
	if arg == "" {
		var resp struct {
			Builds []store.Build `json:"builds"`
		}
		if err := n.commands.call(ctx, http.MethodGet, "/api/chatops/builds?limit=5", chatID, nil, &resp); err != nil {
			return esc(err.Error())
		}
		if len(resp.Builds) == 0 {
			return "No builds yet."
		}
		var sb strings.Builder
		for _, b := range resp.Builds {
			fmt.Fprintf(&sb, "#%d <b>%s</b> %s %s <code>%s</code>\n",
				b.ID, esc(b.Status), esc(b.Repository), esc(shortRef(b.Ref)), esc(short(b.CommitSHA)))
		}
		return strings.TrimRight(sb.String(), "\n")
	}
	id, err := parseID(arg)
	if err != nil {
		return err.Error()
	}
	var b store.Build
	if err := n.commands.call(ctx, http.MethodGet, "/api/chatops/builds/"+id, chatID, nil, &b); err != nil {
		return esc(err.Error())
	}
	return fmt.Sprintf("#%d <b>%s</b> %s %s <code>%s</code>",
		b.ID, esc(b.Status), esc(b.Repository), esc(shortRef(b.Ref)), esc(short(b.CommitSHA)))
}

func (n *notifier) cmdLogs(ctx context.Context, chatID, arg string) string {
	id, err := parseID(arg)
	if err != nil {
		return "Usage: /logs &lt;build id&gt;"
	}
	var resp struct {
		Lines []store.LogLine `json:"lines"`
	}
	if err := n.commands.call(ctx, http.MethodGet, "/api/chatops/builds/"+id+"/logs", chatID, nil, &resp); err != nil {
		return esc(err.Error())
	}
	if len(resp.Lines) == 0 {
		return "Build #" + id + " has no log lines."
	}
	lines := make([]string, len(resp.Lines))
	for i, l := range resp.Lines {
		lines[i] = esc(clip(l.Line, maxLine))
	}
	return fmt.Sprintf("Build #%s\n<pre>%s</pre>", id, strings.Join(lines, "\n"))
}

func (n *notifier) cmdRerun(ctx context.Context, chatID, arg string) string {
	id, err := parseID(arg)
	if err != nil {
		return "Usage: /rerun &lt;build id&gt;"
	}
	var b store.Build
	if err := n.commands.call(ctx, http.MethodPost, "/api/chatops/builds/"+id+"/rerun", chatID, nil, &b); err != nil {
		return esc(err.Error())
	}
	return fmt.Sprintf("Build #%d queued, rerun of #%s (%s <code>%s</code>).",
		b.ID, id, esc(b.Repository), esc(short(b.CommitSHA)))
}

func (n *notifier) cmdRollback(ctx context.Context, chatID, arg string) string {
	body := map[string]string{}
	if arg != "" {
		body["repository"] = arg
	}
	var job store.DeployJob
	if err := n.commands.call(ctx, http.MethodPost, "/api/chatops/rollback", chatID, body, &job); err != nil {
		return esc(err.Error())
	}
	job, err := n.waitJob(ctx, chatID, job.ID)
	if err != nil {
		return esc(err.Error())
	}
	if job.Status == store.JobFailed {
		return fmt.Sprintf("Rollback of %s failed: %s", esc(job.Repository), esc(job.Error))
	}
	return fmt.Sprintf("Rollback of %s restored the active release.", esc(job.Repository))
}

func (n *notifier) waitJob(ctx context.Context, chatID string, id int64) (store.DeployJob, error) {
	deadline := time.Now().Add(45 * time.Second)
	path := fmt.Sprintf("/api/chatops/jobs/%d", id)
	for {
		var job store.DeployJob
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := n.commands.call(callCtx, http.MethodGet, path, chatID, nil, &job)
		cancel()
		if err != nil {
			return store.DeployJob{}, err
		}
		if job.Status == store.JobDone || job.Status == store.JobFailed {
			return job, nil
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			return job, errors.New("rollback is still running; check the deploy container in a moment")
		}
		select {
		case <-ctx.Done():
			return job, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func parseID(arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil || id < 1 {
		return "", errors.New("build id must be a positive number")
	}
	return strconv.FormatInt(id, 10), nil
}
