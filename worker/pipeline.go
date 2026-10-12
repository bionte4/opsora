package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"time"

	"devops-allinone/shared/store"
)

const (
	checkoutTimeout = 10 * time.Minute
	maxConfigBytes  = 64 << 10
)

// cloneScript fetches exactly one commit. The auth header is passed with -c so
// it is never written to .git/config, where build steps could read it.
const cloneScript = `set -eu
git init -q .
git remote add origin "$REPO_URL"
if [ -n "${GIT_AUTH_HEADER:-}" ]; then
  git -c http.extraHeader="$GIT_AUTH_HEADER" fetch --depth 1 origin "$COMMIT_SHA"
else
  git fetch --depth 1 origin "$COMMIT_SHA"
fi
git checkout -q --detach FETCH_HEAD
git log -1 --format='checked out %H %s'
`

type buildResult struct {
	status      string
	err         string
	githubState string
}

func failure(format string, args ...any) buildResult {
	return buildResult{status: store.StatusFailed, err: fmt.Sprintf(format, args...), githubState: statusFailure}
}

func infraError(format string, args ...any) buildResult {
	return buildResult{status: store.StatusFailed, err: fmt.Sprintf(format, args...), githubState: statusError}
}

// errInterrupted means the build stopped because the worker is shutting down
// or lost its lease, not because the pipeline failed.
var errInterrupted = errors.New("build interrupted")

func (w *worker) execute(ctx context.Context, b store.Build) (buildResult, error) {
	if err := w.runner.cleanup(detached, b.ID); err != nil {
		log.Printf("build=%d cleanup before start: %v", b.ID, err)
	}
	defer func() {
		if err := w.runner.cleanup(detached, b.ID); err != nil {
			log.Printf("build=%d cleanup: %v", b.ID, err)
		}
	}()

	if err := w.store.ResetSteps(detached, b.ID, []string{checkoutStepName}); err != nil {
		return infraError("could not record steps: %v", err), nil
	}

	cfg, volume, res, err := w.checkout(ctx, b)
	if err != nil || res.status != "" {
		w.cancelPending(b.ID)
		return res, err
	}

	names := make([]string, len(cfg.Steps))
	for i, s := range cfg.Steps {
		names[i] = s.Name
	}
	if err := w.store.AppendSteps(detached, b.ID, 2, names); err != nil {
		return infraError("could not record steps: %v", err), nil
	}

	buildCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	for i, step := range cfg.Steps {
		res, err := w.runStep(ctx, buildCtx, b, cfg, step, i+2, volume)
		if err != nil || res.status != "" {
			w.cancelPending(b.ID)
			return res, err
		}
	}
	return buildResult{status: store.StatusSuccess, githubState: statusSuccess}, nil
}

func (w *worker) checkout(ctx context.Context, b store.Build) (pipelineConfig, string, buildResult, error) {
	const position = 1
	w.setStep(b.ID, position, store.StepRunning)
	slog := newStepLog(w.store, b.ID, position)
	defer slog.Close()

	fail := func(stepStatus string, res buildResult) (pipelineConfig, string, buildResult, error) {
		w.setStep(b.ID, position, stepStatus)
		slog.Systemf("%s", res.err)
		return pipelineConfig{}, "", res, nil
	}

	stepCtx, cancel := context.WithTimeout(ctx, checkoutTimeout)
	defer cancel()

	if err := w.runner.ensureImage(stepCtx, gitImage, slog.Systemf); err != nil {
		if ctx.Err() != nil {
			w.setStep(b.ID, position, store.StepCancelled)
			return pipelineConfig{}, "", buildResult{}, errInterrupted
		}
		return fail(store.StepFailed, infraError("checkout: %v", err))
	}
	volume, err := w.runner.createWorkspace(stepCtx, b.ID)
	if err != nil {
		return fail(store.StepFailed, infraError("checkout: create workspace: %v", err))
	}

	repoURL := "https://github.com/" + b.Repository + ".git"
	env := []string{"REPO_URL=" + repoURL, "COMMIT_SHA=" + b.CommitSHA}
	if w.githubToken != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + w.githubToken))
		env = append(env, "GIT_AUTH_HEADER=Authorization: Basic "+basic)
	}
	slog.Systemf("fetching %s at %s", repoURL, b.CommitSHA)
	code, err := w.runner.run(stepCtx, containerSpec{
		BuildID:    b.ID,
		Image:      gitImage,
		Entrypoint: []string{"sh", "-c", cloneScript},
		Env:        env,
		Volume:     volume,
	}, slog.Writer(store.StreamStdout), slog.Writer(store.StreamStderr))
	switch {
	case ctx.Err() != nil:
		w.setStep(b.ID, position, store.StepCancelled)
		return pipelineConfig{}, "", buildResult{}, errInterrupted
	case errors.Is(err, context.DeadlineExceeded):
		return fail(store.StepTimedOut, failure("checkout timed out after %s", checkoutTimeout))
	case err != nil:
		return fail(store.StepFailed, infraError("checkout: %v", err))
	case code != 0:
		return fail(store.StepFailed, failure("checkout failed with exit code %d", code))
	}

	var out bytes.Buffer
	var errOut bytes.Buffer
	code, err = w.runner.run(stepCtx, containerSpec{
		BuildID:    b.ID,
		Image:      gitImage,
		Entrypoint: []string{"cat", configFile},
		Volume:     volume,
	}, &limitedBuffer{buf: &out, limit: maxConfigBytes + 1}, &errOut)
	switch {
	case ctx.Err() != nil:
		w.setStep(b.ID, position, store.StepCancelled)
		return pipelineConfig{}, "", buildResult{}, errInterrupted
	case err != nil:
		return fail(store.StepFailed, infraError("read %s: %v", configFile, err))
	case code != 0:
		return fail(store.StepFailed, failure("no %s found at commit %s", configFile, short(b.CommitSHA)))
	case out.Len() > maxConfigBytes:
		return fail(store.StepFailed, failure("%s is larger than %d KB", configFile, maxConfigBytes>>10))
	}

	cfg, err := parseConfig(out.Bytes())
	if err != nil {
		return fail(store.StepFailed, failure("%v", err))
	}
	slog.Systemf("loaded %s with %d steps, build timeout %s", configFile, len(cfg.Steps), cfg.Timeout)
	w.setStep(b.ID, position, store.StepSuccess)
	return cfg, volume, buildResult{}, nil
}

// runStep returns a zero buildResult when the step succeeded and the build
// should continue.
func (w *worker) runStep(ctx, buildCtx context.Context, b store.Build, cfg pipelineConfig, step stepConfig, position int, volume string) (buildResult, error) {
	w.setStep(b.ID, position, store.StepRunning)
	log.Printf("build=%d step=%s status=running image=%s", b.ID, step.Name, step.Image)
	slog := newStepLog(w.store, b.ID, position)
	defer slog.Close()

	finish := func(stepStatus string, res buildResult) (buildResult, error) {
		w.setStep(b.ID, position, stepStatus)
		if res.err != "" {
			slog.Systemf("%s", res.err)
		}
		log.Printf("build=%d step=%s status=%s", b.ID, step.Name, stepStatus)
		return res, nil
	}
	interrupted := func() (buildResult, error) {
		w.setStep(b.ID, position, store.StepCancelled)
		log.Printf("build=%d step=%s status=cancelled", b.ID, step.Name)
		return buildResult{}, errInterrupted
	}

	if err := w.runner.ensureImage(buildCtx, step.Image, slog.Systemf); err != nil {
		if ctx.Err() != nil {
			return interrupted()
		}
		if buildCtx.Err() != nil {
			return finish(store.StepTimedOut, failure("build timed out after %s while pulling %s", cfg.Timeout, step.Image))
		}
		return finish(store.StepFailed, infraError("step %q: %v", step.Name, err))
	}

	stepCtx, cancel := context.WithTimeout(buildCtx, step.Timeout)
	defer cancel()
	started := time.Now()
	code, err := w.runner.run(stepCtx, containerSpec{
		BuildID:    b.ID,
		Image:      step.Image,
		Entrypoint: []string{"/bin/sh", "-c"},
		Cmd:        []string{step.Run},
		Env:        stepEnv(b, cfg, step),
		Volume:     volume,
	}, slog.Writer(store.StreamStdout), slog.Writer(store.StreamStderr))
	elapsed := time.Since(started).Round(time.Millisecond)

	switch {
	case ctx.Err() != nil:
		return interrupted()
	case buildCtx.Err() != nil:
		return finish(store.StepTimedOut, failure("build timed out after %s during step %q", cfg.Timeout, step.Name))
	case errors.Is(err, context.DeadlineExceeded):
		return finish(store.StepTimedOut, failure("step %q timed out after %s", step.Name, step.Timeout))
	case err != nil:
		return finish(store.StepFailed, infraError("step %q: %v", step.Name, err))
	case code != 0:
		return finish(store.StepFailed, failure("step %q exited with code %d", step.Name, code))
	}
	slog.Systemf("step %q finished in %s", step.Name, elapsed)
	return finish(store.StepSuccess, buildResult{})
}

func stepEnv(b store.Build, cfg pipelineConfig, step stepConfig) []string {
	merged := map[string]string{}
	for k, v := range cfg.Env {
		merged[k] = v
	}
	for k, v := range step.Env {
		merged[k] = v
	}
	merged["CI"] = "true"
	merged["OPSORA"] = "true"
	merged["OPSORA_BUILD_ID"] = strconv.FormatInt(b.ID, 10)
	merged["OPSORA_REPOSITORY"] = b.Repository
	merged["OPSORA_REF"] = b.Ref
	merged["OPSORA_COMMIT_SHA"] = b.CommitSHA
	merged["OPSORA_STEP"] = step.Name

	env := make([]string, 0, len(merged))
	for k, v := range merged {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	return env
}

func (w *worker) cancelPending(buildID int64) {
	if err := w.store.CancelPendingSteps(detached, buildID); err != nil {
		log.Printf("build=%d cancel pending steps: %v", buildID, err)
	}
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

type limitedBuffer struct {
	buf   *bytes.Buffer
	limit int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.limit - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}
