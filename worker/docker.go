package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

const (
	workspaceDir = "/workspace"
	gitImage     = "alpine/git:latest"
	labelBuild   = "opsora.build"

	stepMemoryBytes = 2 << 30
	stepNanoCPUs    = 2_000_000_000
	stepPidsLimit   = 1024
)

type runner struct {
	cli *client.Client
}

type containerSpec struct {
	BuildID    int64
	Image      string
	Entrypoint []string
	Cmd        []string
	Env        []string
	Volume     string
}

func newRunner(ctx context.Context) (*runner, error) {
	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		cli.Close()
		return nil, err
	}
	return &runner{cli: cli}, nil
}

func (r *runner) Close() error {
	return r.cli.Close()
}

func buildLabels(buildID int64) map[string]string {
	return map[string]string{labelBuild: strconv.FormatInt(buildID, 10)}
}

func (r *runner) createWorkspace(ctx context.Context, buildID int64) (string, error) {
	name := fmt.Sprintf("opsora-build-%d", buildID)
	_, err := r.cli.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name:   name,
		Labels: buildLabels(buildID),
	})
	return name, err
}

// cleanup force-removes every container and volume labelled with the build,
// including ones left behind by a worker that crashed mid-build.
func (r *runner) cleanup(ctx context.Context, buildID int64) error {
	filter := client.Filters{}.Add("label", labelBuild+"="+strconv.FormatInt(buildID, 10))

	containers, err := r.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filter})
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	var errs []error
	for _, c := range containers.Items {
		if _, err := r.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove container %s: %w", c.ID[:12], err))
		}
	}

	volumes, err := r.cli.VolumeList(ctx, client.VolumeListOptions{Filters: filter})
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("list volumes: %w", err))...)
	}
	for _, v := range volumes.Items {
		if _, err := r.cli.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove volume %s: %w", v.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (r *runner) ensureImage(ctx context.Context, image string, logf func(string, ...any)) error {
	if _, err := r.cli.ImageInspect(ctx, image); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect image %s: %w", image, err)
	}

	logf("pulling image %s", image)
	resp, err := r.cli.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", image, err)
	}
	for msg, err := range resp.JSONMessages(ctx) {
		if err != nil {
			return fmt.Errorf("pull image %s: %w", image, err)
		}
		if msg.Error != nil {
			return fmt.Errorf("pull image %s: %s", image, msg.Error.Message)
		}
	}
	logf("pulled image %s", image)
	return nil
}

// run starts a container, streams its output until it exits, and returns the
// exit code. When ctx ends first the container is killed and ctx.Err() is
// returned.
func (r *runner) run(ctx context.Context, spec containerSpec, stdout, stderr io.Writer) (int64, error) {
	init := true
	pids := int64(stepPidsLimit)
	created, err := r.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      spec.Image,
			Entrypoint: spec.Entrypoint,
			Cmd:        spec.Cmd,
			Env:        spec.Env,
			WorkingDir: workspaceDir,
			Labels:     buildLabels(spec.BuildID),
		},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{{
				Type:   mount.TypeVolume,
				Source: spec.Volume,
				Target: workspaceDir,
			}},
			Resources: container.Resources{
				Memory:    stepMemoryBytes,
				NanoCPUs:  stepNanoCPUs,
				PidsLimit: &pids,
			},
			SecurityOpt: []string{"no-new-privileges"},
			Init:        &init,
		},
	})
	if err != nil {
		return -1, fmt.Errorf("create container: %w", err)
	}
	id := created.ID
	defer func() {
		if _, err := r.cli.ContainerRemove(detached, id, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			log.Printf("remove container %s: %v", id[:12], err)
		}
	}()

	if _, err := r.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return -1, fmt.Errorf("start container: %w", err)
	}

	logs, err := r.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: true})
	if err != nil {
		r.kill(id)
		return -1, fmt.Errorf("attach logs: %w", err)
	}
	defer logs.Close()
	copied := make(chan struct{})
	go func() {
		stdcopy.StdCopy(stdout, stderr, logs)
		close(copied)
	}()

	wait := r.cli.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case res := <-wait.Result:
		<-copied
		if res.Error != nil && res.Error.Message != "" {
			return res.StatusCode, fmt.Errorf("container: %s", res.Error.Message)
		}
		return res.StatusCode, nil
	case err := <-wait.Error:
		r.kill(id)
		logs.Close()
		<-copied
		if ctx.Err() != nil {
			return -1, ctx.Err()
		}
		return -1, fmt.Errorf("wait container: %w", err)
	}
}

func (r *runner) kill(id string) {
	if _, err := r.cli.ContainerKill(detached, id, client.ContainerKillOptions{}); err != nil && !cerrdefs.IsNotFound(err) && !cerrdefs.IsConflict(err) {
		log.Printf("kill container %s: %v", id[:12], err)
	}
}
