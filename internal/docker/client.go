package docker

import (
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/go-errors/errors"
)

var cli *client.Client

func Init() (*client.Client, error) {
	if cli != nil {
		return cli, nil
	}

	// Use default Docker client which auto-discovers socket on macOS and Linux
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	cli = c
	return cli, nil
}

func Client() *client.Client {
	return cli
}

func CheckDocker(ctx context.Context) error {
	if cli == nil {
		var err error
		cli, err = Init()
		if err != nil {
			return err
		}
	}
	_, err := cli.Ping(ctx)
	if err != nil {
		return fmt.Errorf("Docker daemon is not accessible: %w", err)
	}
	return nil
}

func PullImageIfNotCached(ctx context.Context, imageName string) error {
	// Try to pull image - Docker will use cache if it exists
	out, err := cli.ImagePull(ctx, imageName, types.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull docker image: %w", err)
	}
	defer out.Close()

	// Drain output
	_, _ = io.Copy(io.Discard, out)

	return nil
}

func RunOnceWithConfig(ctx context.Context, cfg container.Config, hostConfig container.HostConfig, stdout, stderr *ContainerOutput) error {
	// Pull image if needed
	if err := PullImageIfNotCached(ctx, cfg.Image); err != nil {
		return err
	}

	// Create container
	resp, err := cli.ContainerCreate(ctx, &cfg, &hostConfig, &network.NetworkingConfig{}, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}
	containerID := resp.ID

	// Ensure container is removed
	defer func() {
		_ = cli.ContainerRemove(ctx, containerID, container.RemoveOptions{
			Force: true,
		})
	}()

	// Start container
	if err := cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}

	// Wait for container to finish first, then read logs
	waitCh, errCh := cli.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
	var exitCode int
	select {
	case result := <-waitCh:
		exitCode = int(result.StatusCode)
	case err := <-errCh:
		return fmt.Errorf("error waiting for container: %w", err)
	}

	// Stream logs (now that container has finished, this will read all logs)
	logs, err := cli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     false, // Container is already finished, no need to follow
	})
	if err != nil {
		return fmt.Errorf("failed to read container logs: %w", err)
	}
	defer logs.Close()

	if _, err := stdcopy.StdCopy(stdout, stderr, logs); err != nil {
		return fmt.Errorf("failed to copy logs: %w", err)
	}

	// Check exit code and include stderr in error message
	if exitCode != 0 {
		stderrStr := stderr.String()
		stdoutStr := stdout.String()
		if stderrStr != "" {
			return fmt.Errorf("container exited with code %d: %s", exitCode, stderrStr)
		}
		if stdoutStr != "" {
			// Sometimes errors go to stdout
			return fmt.Errorf("container exited with code %d: %s", exitCode, stdoutStr)
		}
		return errors.Errorf("container exited with code %d", exitCode)
	}
	return nil
}

type ContainerOutput struct {
	data []byte
}

func NewContainerOutput() *ContainerOutput {
	return &ContainerOutput{}
}

func (o *ContainerOutput) Write(p []byte) (int, error) {
	o.data = append(o.data, p...)
	return len(p), nil
}

func (o *ContainerOutput) Bytes() []byte {
	return o.data
}

func (o *ContainerOutput) String() string {
	return string(o.data)
}
