// Copyright 2026 Exasol AG
// SPDX-License-Identifier: MIT

package localruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/exasol/exasol-personal/internal/localinstall"
)

const (
	dockerCommand = "docker"
	// dockerProbeTimeout bounds the daemon probe, so a hung daemon cannot
	// hold up `exasol status` past its own timeout.
	dockerProbeTimeout = 10 * time.Second
	dockerLinuxOSType  = "linux"
)

var errDockerUnavailable = errors.New("docker is not available")

// dockerHostEnvironmentPreparer readies a direct host whose containers run on
// an existing Docker installation. The launcher never installs, starts, or
// configures Docker, so every check reads state only and needs no approval.
type dockerHostEnvironmentPreparer struct {
	platform HostPlatform
}

func (preparer dockerHostEnvironmentPreparer) Platform() HostPlatform {
	return preparer.platform
}

func (dockerHostEnvironmentPreparer) EnsureReady(ctx context.Context, _ PrepareOptions) error {
	return requireDockerLinuxEngine(ctx)
}

// EnsureStartable repeats the readiness check, because the daemon can stop or
// lose its socket between preparation and start.
func (dockerHostEnvironmentPreparer) EnsureStartable(ctx context.Context, _, _ io.Writer) error {
	return requireDockerLinuxEngine(ctx)
}

// ContainerHostRunning reports whether the Docker daemon answers. A daemon
// that does not answer is unreachable rather than stopped, because Docker
// restarts the deployment's containers once the daemon is back.
func (dockerHostEnvironmentPreparer) ContainerHostRunning(ctx context.Context) (bool, error) {
	if _, err := exec.LookPath(dockerCommand); err != nil {
		return false, fmt.Errorf("%w: %w", ErrContainerHostUnreachable, dockerUnavailable(err, ""))
	}
	if probe, err := dockerOSType(ctx); err != nil {
		return false, fmt.Errorf(
			"%w: %w", ErrContainerHostUnreachable, dockerUnavailable(err, probe.output),
		)
	}

	return true, nil
}

func (preparer dockerHostEnvironmentPreparer) NewExecutionEnvironment(
	runtimeExec []string,
) localinstall.ExecutionEnvironment {
	direct := localinstall.NewDirectExecutionEnvironment(runtimeExec)
	if preparer.platform == HostPlatformWindows {
		return windowsDockerExecutionEnvironment{DirectExecutionEnvironment: direct}
	}

	return direct
}

// requireDockerLinuxEngine checks that the docker CLI resolves, its daemon
// answers, and the daemon runs Linux containers, which Nano needs.
func requireDockerLinuxEngine(ctx context.Context) error {
	if _, err := exec.LookPath(dockerCommand); err != nil {
		return dockerUnavailable(err, "")
	}
	probe, err := dockerOSType(ctx)
	if err != nil {
		return dockerUnavailable(err, probe.output)
	}
	if probe.osType != dockerLinuxOSType {
		return fmt.Errorf(
			"%w: the Docker daemon runs %q containers, but local deployments need "+
				"Linux containers; switch Docker to Linux containers, or select Podman "+
				"with `exasol config set --container-engine podman`",
			errDockerUnavailable, probe.osType,
		)
	}

	return nil
}

// dockerProbe is the daemon's OS type, or the CLI's error output when the
// daemon does not answer.
type dockerProbe struct {
	osType string
	output string
}

func dockerOSType(ctx context.Context) (dockerProbe, error) {
	probeCtx, cancel := context.WithTimeout(ctx, dockerProbeTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	//nolint:gosec // fixed command and arguments
	command := exec.CommandContext(probeCtx, dockerCommand, "info", "--format", "{{.OSType}}")
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return dockerProbe{output: strings.TrimSpace(stderr.String())}, err
	}

	return dockerProbe{osType: strings.TrimSpace(stdout.String())}, nil
}

func dockerUnavailable(err error, output string) error {
	detail := err.Error()
	if output != "" {
		detail = output
	}

	return fmt.Errorf(
		"%w: %s\nCheck that the Docker daemon is running (`docker info`), that your "+
			"user may use it (for example membership of the `docker` group, which "+
			"applies to new login sessions), and that the active Docker context "+
			"(`docker context ls`) is the one you expect",
		errDockerUnavailable, detail,
	)
}

// windowsDockerExecutionEnvironment skips Sync: Windows has no host-side
// `sync`, and Docker Desktop's VM offers no supported way to flush it.
type windowsDockerExecutionEnvironment struct {
	*localinstall.DirectExecutionEnvironment
}

func (windowsDockerExecutionEnvironment) Sync(context.Context, io.Writer, io.Writer) error {
	return nil
}
