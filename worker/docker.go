package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"time"

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
	labelDeploy  = "opsora.deploy"
	archiveImage = "alpine:3.22"

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

// workspaceArchive returns a tar of the repository root. The container is only
// created so the volume can be read; it is never started. close must run
// after the tar has been consumed.
func (r *runner) workspaceArchive(ctx context.Context, buildID int64, volume string) (io.ReadCloser, func(), error) {
	if err := r.ensureImage(ctx, archiveImage, log.Printf); err != nil {
		return nil, nil, err
	}
	created, err := r.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:  archiveImage,
			Cmd:    []string{"true"},
			Labels: buildLabels(buildID),
		},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: volume, Target: workspaceDir}},
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("archive workspace: %w", err)
	}
	id := created.ID
	res, err := r.cli.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: workspaceDir + "/."})
	if err != nil {
		_, _ = r.cli.ContainerRemove(detached, id, client.ContainerRemoveOptions{Force: true})
		return nil, nil, fmt.Errorf("archive workspace: %w", err)
	}
	closeArchive := func() {
		res.Content.Close()
		if _, err := r.cli.ContainerRemove(detached, id, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			log.Printf("remove archive container %s: %v", id[:12], err)
		}
	}
	return res.Content, closeArchive, nil
}

// buildImage builds a single image tag from the workspace. The Dockerfile RUN
// steps execute on the Docker daemon, so deploy is opt-in and only for
// repositories the operator trusts.
func (r *runner) buildImage(ctx context.Context, buildID int64, volume, dockerfile, tag string) error {
	body, closeArchive, err := r.workspaceArchive(ctx, buildID, volume)
	if err != nil {
		return err
	}
	defer closeArchive()
	resp, err := r.cli.ImageBuild(ctx, body, client.ImageBuildOptions{
		Tags:       []string{tag},
		Dockerfile: dockerfile,
		Remove:     true,
		PullParent: true,
		Memory:     stepMemoryBytes,
		// Do not label the image with opsora.build. A container started from it
		// would inherit that label and the build cleanup would remove it.
	})
	if err != nil {
		return fmt.Errorf("docker build: %w", err)
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("docker build: %w", err)
		}
		if msg.Error != "" {
			return fmt.Errorf("docker build: %s", msg.Error)
		}
	}
}

func (r *runner) tagImage(ctx context.Context, source, target string) error {
	_, err := r.cli.ImageTag(ctx, client.ImageTagOptions{Source: source, Target: target})
	return err
}

func deployContainerName(repository string) string {
	return "opsora-deploy-" + strings.NewReplacer("/", "-", ":", "-").Replace(strings.ToLower(repository))
}

// replaceDeployContainer stops whatever is deployed for the repository and
// starts imageRef. The container gets no mounts and no secrets.
func (r *runner) replaceDeployContainer(ctx context.Context, repository, imageRef string) error {
	name := deployContainerName(repository)
	if _, err := r.cli.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove deploy container: %w", err)
	}
	init := true
	pids := int64(512)
	created, err := r.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:  imageRef,
			Labels: map[string]string{labelDeploy: repository, "opsora.image": imageRef},
		},
		HostConfig: &container.HostConfig{
			Resources: container.Resources{
				Memory:    1 << 30,
				NanoCPUs:  1_000_000_000,
				PidsLimit: &pids,
			},
			SecurityOpt: []string{"no-new-privileges"},
			Init:        &init,
		},
	})
	if err != nil {
		return fmt.Errorf("create deploy container: %w", err)
	}
	if _, err := r.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start deploy container: %w", err)
	}
	return nil
}

func (r *runner) removeDeployContainer(ctx context.Context, repository string) error {
	name := deployContainerName(repository)
	_, err := r.cli.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

type deployState struct {
	missing bool
	running bool
	exit    int
	image   string
}

func (r *runner) inspectDeploy(ctx context.Context, repository string) (deployState, error) {
	res, err := r.cli.ContainerInspect(ctx, deployContainerName(repository), client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return deployState{missing: true}, nil
	}
	if err != nil {
		return deployState{}, err
	}
	st := deployState{}
	if res.Container.State != nil {
		st.running = res.Container.State.Running
		st.exit = res.Container.State.ExitCode
	}
	if res.Container.Config != nil {
		st.image = res.Container.Config.Image
	}
	return st, nil
}

// waitDeployed returns when the container has exited or settle has passed
// while it is still running.
func (r *runner) waitDeployed(ctx context.Context, repository string, settle time.Duration) (deployState, error) {
	deadline := time.Now().Add(settle)
	for {
		st, err := r.inspectDeploy(ctx, repository)
		if err != nil || st.missing || !st.running || !time.Now().Before(deadline) {
			return st, err
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return st, ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *runner) kill(id string) {
	if _, err := r.cli.ContainerKill(detached, id, client.ContainerKillOptions{}); err != nil && !cerrdefs.IsNotFound(err) && !cerrdefs.IsConflict(err) {
		log.Printf("kill container %s: %v", id[:12], err)
	}
}
