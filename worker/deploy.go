package main

import (
	"context"
	"fmt"
	"log"

	"devops-allinone/shared/store"
)

// deploy builds the release image after the pipeline succeeds and starts it.
// A failed deploy is recorded on its own and does not fail the build: the
// tests passed, and the previous release stays active so it can be restored.
func (w *worker) deploy(ctx context.Context, b store.Build, cfg pipelineConfig, volume string) error {
	d := cfg.Deploy
	tag := imageTag(d.Image, b.CommitSHA)
	log.Printf("build=%d deploying %s", b.ID, tag)

	rel := store.Release{
		Repository: b.Repository,
		Image:      d.Image,
		CommitSHA:  b.CommitSHA,
		BuildID:    b.ID,
	}
	fail := func(err error) error {
		if ctx.Err() != nil {
			return errInterrupted
		}
		rel.Status = store.ReleaseFailed
		rel.Error = err.Error()
		if _, saveErr := w.store.SaveRelease(detached, rel); saveErr != nil {
			log.Printf("build=%d save failed release: %v", b.ID, saveErr)
		}
		deploys.WithLabelValues(store.ReleaseFailed).Inc()
		log.Printf("build=%d deploy failed: %v", b.ID, err)
		return nil
	}

	if err := w.runner.buildImage(ctx, b.ID, volume, d.Dockerfile, tag); err != nil {
		return fail(err)
	}
	if err := w.runner.replaceDeployContainer(ctx, b.Repository, tag); err != nil {
		return fail(err)
	}
	st, err := w.runner.waitDeployed(ctx, b.Repository, d.Settle)
	if err != nil {
		return fail(err)
	}
	if st.missing || (!st.running && st.exit != 0) {
		_ = w.runner.removeDeployContainer(detached, b.Repository)
		return fail(fmt.Errorf("container %s exited with code %d", deployContainerName(b.Repository), st.exit))
	}
	if err := w.runner.tagImage(ctx, tag, d.Image+":current"); err != nil {
		return fail(err)
	}
	rel.Status = store.ReleaseActive
	if _, err := w.store.SaveRelease(detached, rel); err != nil {
		return fail(fmt.Errorf("record release: %w", err))
	}
	deploys.WithLabelValues(store.ReleaseActive).Inc()
	log.Printf("build=%d deploy active %s", b.ID, tag)
	return nil
}

func imageTag(image, sha string) string {
	if sha == "" {
		return image + ":untagged"
	}
	return image + ":" + sha
}

// runDeployJob claims one queued rollback and runs it. It reports whether it
// found a job, so the worker loop can look again before taking a build.
func (w *worker) runDeployJob(ctx context.Context) bool {
	job, ok, err := w.store.ClaimDeployJob(detached)
	if err != nil {
		log.Printf("claim deploy job: %v", err)
		return false
	}
	if !ok {
		return false
	}
	status, errText := store.JobDone, ""
	if err := w.rollback(ctx, job); err != nil {
		status, errText = store.JobFailed, err.Error()
		log.Printf("job=%d rollback %s: %v", job.ID, job.Repository, err)
	} else {
		log.Printf("job=%d rollback %s restored", job.ID, job.Repository)
	}
	if err := w.store.FinishDeployJob(detached, job.ID, status, errText); err != nil {
		log.Printf("job=%d finish: %v", job.ID, err)
	}
	return true
}

func (w *worker) rollback(ctx context.Context, job store.DeployJob) error {
	if job.Action != store.ActionRollback {
		return fmt.Errorf("unknown deploy action %q", job.Action)
	}
	active, err := w.store.ActiveRelease(detached, job.Repository)
	if err != nil {
		return err
	}
	tag := imageTag(active.Image, active.CommitSHA)
	if err := w.runner.replaceDeployContainer(ctx, job.Repository, tag); err != nil {
		return err
	}
	st, err := w.runner.waitDeployed(ctx, job.Repository, defaultSettle)
	if err != nil {
		return err
	}
	if st.missing || (!st.running && st.exit != 0) {
		return fmt.Errorf("restored container exited with code %d", st.exit)
	}
	if err := w.runner.tagImage(ctx, tag, active.Image+":current"); err != nil {
		return err
	}
	return w.store.MarkRolledBack(detached, job.Repository)
}
