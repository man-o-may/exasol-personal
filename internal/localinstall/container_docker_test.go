// Copyright 2026 Exasol AG
// SPDX-License-Identifier: MIT

package localinstall

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// scriptedRunner answers each command from the first reply whose prefix it
// starts with, and records every command it receives.
type scriptedRunner struct {
	replies []scriptedReply
	calls   []string
}

type scriptedReply struct {
	prefix string
	output string
	stderr string
	err    error
}

func (runner *scriptedRunner) Run(
	_ context.Context, _ io.Reader, stdout, stderr io.Writer, command ...string,
) error {
	line := strings.Join(command, " ")
	runner.calls = append(runner.calls, line)
	for _, reply := range runner.replies {
		if strings.HasPrefix(line, reply.prefix) {
			if stdout != nil {
				_, _ = io.WriteString(stdout, reply.output)
			}
			if stderr != nil {
				_, _ = io.WriteString(stderr, reply.stderr)
			}

			return reply.err
		}
	}

	return nil
}

func (runner *scriptedRunner) called(prefix string) bool {
	return slices.ContainsFunc(runner.calls, func(call string) bool {
		return strings.HasPrefix(call, prefix)
	})
}

func nanoLikeSpec() ContainerSpec {
	return ContainerSpec{
		Name:            "exasol-db-test",
		Image:           "exasol/nano:test",
		Replace:         true,
		NestedSandbox:   true,
		SecurityOptions: []string{"label=disable"},
		Entrypoint:      []string{"/bin/server", "--verbose"},
		Args:            []string{"init"},
		Mounts: []Mount{
			{Kind: MountImage, Source: "slc:1", Target: "/exa/slc/python", Writable: true},
		},
	}
}

func TestDockerCreate_SpellsSpecForDocker(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{replies: []scriptedReply{
		{prefix: "docker ps", output: "abc\n"},
		{prefix: "docker container inspect", output: `[{"Id":"abc","Name":"/exasol-db-test"}]`},
	}}

	err := newDockerRuntime(runner).Create(context.Background(), nanoLikeSpec(), nil, nil)

	require.NoError(t, err)
	require.True(t, runner.called("docker rm --force -- exasol-db-test"))
	run := runner.calls[len(runner.calls)-1]
	require.NotContains(t, run, "--replace")
	require.Contains(
		t,
		run,
		"--security-opt seccomp=unconfined --security-opt apparmor=unconfined "+
			"--security-opt label=disable",
	)
	require.Contains(t, run, "--mount type=image,source=slc:1,destination=/exa/slc/python ")
	require.True(t, strings.HasSuffix(run,
		"--entrypoint /bin/server exasol/nano:test --verbose init"), run)
}

func TestPodmanCreate_KeepsPodmanSpelling(t *testing.T) {
	t.Parallel()

	args, err := createArgs(nanoLikeSpec(), podmanDialect, true)

	require.NoError(t, err)
	run := strings.Join(args, " ")
	require.Contains(t, run, "run --detach --replace --name exasol-db-test")
	require.Contains(t, run, "--security-opt unmask=ALL --security-opt label=disable")
	require.Contains(t, run, "type=image,source=slc:1,destination=/exa/slc/python,rw=true")
	require.True(t, strings.HasSuffix(run,
		`--entrypoint ["/bin/server","--verbose"] exasol/nano:test init`), run)
}

func TestDockerRunToCompletion_RemovesContainerAfterExit(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{}

	err := newDockerRuntime(runner).RunToCompletion(
		context.Background(), ContainerSpec{Image: "busybox", Args: []string{"true"}}, nil, nil,
	)

	require.NoError(t, err)
	require.Equal(t, []string{"docker run --rm busybox true"}, runner.calls)
}

func TestDockerRemove_RemovesContainerByID(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{}

	err := newDockerRuntime(runner).Remove(context.Background(), "565b4d86", true, nil, nil)

	require.NoError(t, err)
	require.Equal(t, []string{"docker rm --force -- 565b4d86"}, runner.calls)
}

func TestDockerRemove_AcceptsAbsentContainer(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{replies: []scriptedReply{{
		prefix: "docker rm",
		stderr: "Error response from daemon: No such container: gone\n",
		err:    errors.New("exit status 1"),
	}}}

	err := newDockerRuntime(runner).Remove(context.Background(), "gone", true, nil, nil)

	require.NoError(t, err)
}

func TestDockerRemove_ReportsOtherFailures(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{replies: []scriptedReply{{
		prefix: "docker rm",
		stderr: "permission denied while trying to connect to the docker API\n",
		err:    errors.New("exit status 1"),
	}}}

	err := newDockerRuntime(runner).Remove(context.Background(), "db", true, nil, nil)

	require.Error(t, err)
}

func TestDockerList_ReadsInspectedContainers(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{replies: []scriptedReply{
		{prefix: "docker ps", output: "abc\n"},
		{prefix: "docker container inspect abc", output: `[{
			"Id": "abc", "Name": "/svc",
			"State": {"Status": "exited", "ExitCode": 3},
			"Config": {"Labels": {"owner": "me"}},
			"NetworkSettings": {"Ports": {
				"4896/tcp": [{"HostIp": "127.0.0.1", "HostPort": "4896"}]
			}}
		}]`},
	}}

	containers, err := newDockerRuntime(runner).List(
		context.Background(), nil, ContainerFilter{Label: "owner=me"},
	)

	require.NoError(t, err)
	require.Equal(t, []ContainerListing{{
		ID: "abc", Names: []string{"svc"}, Labels: map[string]string{"owner": "me"},
		State: "exited", ExitCode: 3,
		Ports: []ContainerPort{{
			HostIP: "127.0.0.1", HostPort: 4896, ContainerPort: 4896, Protocol: "tcp",
		}},
	}}, containers)
	require.True(t, runner.called("docker ps --all --quiet --no-trunc --filter label=owner=me"))
}

func TestDockerNetworks_ReportUserBridgesWithDNS(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{replies: []scriptedReply{
		{prefix: "docker network ls", output: "n1\nn2\n"},
		{prefix: "docker network inspect", output: `[
			{"Name": "bridge", "Driver": "bridge"},
			{"Name": "owned-services", "Driver": "bridge", "Labels": {"owner": "me"}}
		]`},
	}}

	networks, err := newDockerRuntime(runner).Networks(context.Background(), nil)

	require.NoError(t, err)
	require.Equal(t, []ContainerNetwork{
		{Name: "bridge", Bridge: true, DNSEnabled: false},
		{
			Name:       "owned-services",
			Bridge:     true,
			DNSEnabled: true,
			Labels:     map[string]string{"owner": "me"},
		},
	}, networks)
}

func TestDockerCreateNetwork_AcceptsExistingNetwork(t *testing.T) {
	t.Parallel()
	existing := &scriptedRunner{replies: []scriptedReply{
		{prefix: "docker network ls", output: "n1\n"},
		{prefix: "docker network inspect", output: `[{"Name": "svc", "Driver": "bridge"}]`},
	}}
	missing := &scriptedRunner{}
	spec := NetworkSpec{Name: "svc", DNS: true, Labels: map[string]string{"owner": "me"}}

	require.NoError(
		t,
		newDockerRuntime(existing).CreateNetwork(context.Background(), spec, nil, nil),
	)
	require.NoError(
		t,
		newDockerRuntime(missing).CreateNetwork(context.Background(), spec, nil, nil),
	)

	require.False(t, existing.called("docker network create"))
	require.True(t, missing.called("docker network create --driver bridge --label owner=me svc"))
}

func TestDockerAttachment_TreatsUserNetworkModeAsBridged(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{replies: []scriptedReply{
		{prefix: "docker ps", output: "abc\n"},
		{prefix: "docker container inspect", output: `[{
			"Id": "abc", "Name": "/db",
			"HostConfig": {"NetworkMode": "db-services"},
			"NetworkSettings": {"Networks": {"db-services": {"Aliases": ["exasol-db"]}}}
		}]`},
	}}

	attachment, present, err := newDockerRuntime(runner).Attachment(context.Background(), "db", nil)

	require.NoError(t, err)
	require.True(t, present)
	require.True(t, attachment.Bridged)
	membership, joined := attachment.Member("db-services")
	require.True(t, joined)
	require.Equal(t, []string{"exasol-db"}, membership.Aliases)
}

func TestDockerImageExists_ReadsImageListing(t *testing.T) {
	t.Parallel()
	present := &scriptedRunner{replies: []scriptedReply{{prefix: "docker images", output: "sha\n"}}}

	found, err := newDockerRuntime(present).ImageExists(context.Background(), "slc:1", nil)
	require.NoError(t, err)
	require.True(t, found)

	found, err = newDockerRuntime(&scriptedRunner{}).ImageExists(context.Background(), "slc:1", nil)
	require.NoError(t, err)
	require.False(t, found)
}

func TestParseLoadedImage_AcceptsUntaggedImageID(t *testing.T) {
	t.Parallel()

	image, err := parseLoadedImage("Loaded image ID: sha256:abc\n")

	require.NoError(t, err)
	require.Equal(t, "sha256:abc", image)
}
