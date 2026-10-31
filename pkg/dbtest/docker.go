package dbtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

var (
	errDockerUnavailable = errors.New("Docker-compatible runtime unavailable")
	errDockerCommand     = errors.New("Docker-compatible operation failed")
	errDockerOutput      = errors.New("Docker-compatible output exceeded limit")
)

type dockerCommand func(context.Context, []string, []string) ([]byte, error)
type dockerCLI struct {
	endpoint string
	command  dockerCommand
}

func (d dockerCLI) run(ctx context.Context, environment []string, args ...string) ([]byte, error) {
	command := d.command
	if command == nil {
		command = runDockerCommand
	}
	return command(ctx, append([]string{"--host", d.endpoint}, args...), environment)
}

// The process has no shell, receives an explicit daemon, and never returns raw
// stderr. Templates select only metadata; full inspect output and logs may
// contain administrator secrets and must not be used by the factory.
func runDockerCommand(ctx context.Context, args, environment []string) ([]byte, error) {
	command := exec.CommandContext(ctx, "docker", args...)
	command.Env = dockerEnvironment(environment)
	command.Stderr = io.Discard
	command.WaitDelay = time.Second
	output := boundedDockerOutput{limit: 256 * 1024}
	command.Stdout = &output
	err := command.Run()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errDockerUnavailable
		}
		return nil, errors.Join(errDockerCommand, ctx.Err())
	}
	if output.overflow {
		return nil, errDockerOutput
	}
	return output.Bytes(), nil
}

func dockerEnvironment(extra []string) []string {
	var result []string
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if key == "DOCKER_HOST" || key == "DOCKER_CONTEXT" {
			continue
		}
		result = append(result, value)
	}
	return append(result, extra...)
}

type boundedDockerOutput struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedDockerOutput) Bytes() []byte { return b.buffer.Bytes() }
func (b *boundedDockerOutput) Len() int      { return b.buffer.Len() }

func (b *boundedDockerOutput) Write(value []byte) (int, error) {
	size := len(value)
	remaining := b.limit - b.Len()
	if size > remaining {
		b.overflow = true
		value = value[:remaining]
	}
	_, _ = b.buffer.Write(value)
	// Keep draining the child's output without retaining unbounded data.
	return size, nil
}

func (d dockerCLI) daemonID(ctx context.Context) (string, error) {
	raw, err := d.run(ctx, nil, "info", "--format", "{{json .ID}}")
	if err != nil {
		return "", errors.Join(ErrFactoryConnection, err)
	}
	var id string
	if json.Unmarshal(raw, &id) != nil || !leaseDaemonID.MatchString(id) {
		return "", ErrFactoryConnection
	}
	return id, nil
}

type dockerImage struct {
	ID      string         `json:"id"`
	Volumes map[string]any `json:"volumes"`
}

const dockerImageTemplate = `{"id":{{json .Id}},"volumes":{{json .Config.Volumes}}}`

func (d dockerCLI) image(ctx context.Context, reference string, major int) (dockerImage, error) {
	raw, err := d.run(ctx, nil, "image", "inspect", "--format", dockerImageTemplate, reference)
	if err != nil {
		if _, err := d.run(ctx, nil, "image", "pull", "--quiet", reference); err != nil {
			return dockerImage{}, err
		}
		raw, err = d.run(ctx, nil, "image", "inspect", "--format", dockerImageTemplate, reference)
	}
	if err != nil {
		return dockerImage{}, err
	}
	var image dockerImage
	if json.Unmarshal(raw, &image) != nil || !leaseImageID.MatchString(image.ID) {
		return dockerImage{}, ErrFactoryConfig
	}
	for path := range image.Volumes {
		if path != containerDataPath(major) {
			return dockerImage{}, ErrFactoryConfig
		}
	}
	return image, nil
}

func containerDataPath(major int) string {
	if major >= 18 {
		return "/var/lib/postgresql"
	}
	return "/var/lib/postgresql/data"
}
