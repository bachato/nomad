// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/hashicorp/go-set/v3"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/testutil"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/plugins/drivers"
)

func fakeContainerList(t *testing.T) (nomadContainer, nonNomadContainer types.Container) {
	path := "./test-resources/docker/reconciler_containers_list.json"

	f, err := os.Open(path)
	must.NoError(t, err, must.Sprintf("failed to open %s", path))

	var sampleContainerList []types.Container
	err = json.NewDecoder(f).Decode(&sampleContainerList)
	must.NoError(t, err, must.Sprint("failed to decode container list"))

	return sampleContainerList[0], sampleContainerList[1]
}

func Test_HasMount(t *testing.T) {
	ci.Parallel(t)

	nomadContainer, nonNomadContainer := fakeContainerList(t)

	must.True(t, hasMount(nomadContainer, "/alloc"))
	must.True(t, hasMount(nomadContainer, "/data"))
	must.True(t, hasMount(nomadContainer, "/secrets"))
	must.False(t, hasMount(nomadContainer, "/random"))

	must.False(t, hasMount(nonNomadContainer, "/alloc"))
	must.False(t, hasMount(nonNomadContainer, "/data"))
	must.False(t, hasMount(nonNomadContainer, "/secrets"))
	must.False(t, hasMount(nonNomadContainer, "/random"))
}

func Test_HasNomadName(t *testing.T) {
	ci.Parallel(t)

	nomadContainer, nonNomadContainer := fakeContainerList(t)

	must.True(t, hasNomadName(nomadContainer))
	must.False(t, hasNomadName(nonNomadContainer))
}

// TestDanglingContainerRemoval_normal asserts containers without corresponding tasks
// are removed after the creation grace period.
func TestDanglingContainerRemoval_normal(t *testing.T) {
	ci.Parallel(t)
	testutil.DockerCompatible(t)

	ctx := context.Background()

	// start two containers: one tracked nomad container, and one unrelated container
	task, cfg, _ := dockerTask(t)
	must.NoError(t, task.EncodeConcreteDriverConfig(cfg))

	dockerClient, d, handle, cleanup := dockerSetup(t, task, nil)
	t.Cleanup(cleanup)

	// wait for task to start
	must.NoError(t, d.WaitUntilStarted(task.ID, 5*time.Second))

	nonNomadContainer, err := dockerClient.ContainerCreate(ctx, &container.Config{
		Image: cfg.Image,
		Cmd:   append([]string{cfg.Command}, cfg.Args...),
	}, nil, nil, nil, "mytest-image-"+uuid.Generate())
	must.NoError(t, err)
	t.Cleanup(func() {
		_ = dockerClient.ContainerRemove(ctx, nonNomadContainer.ID, container.RemoveOptions{
			Force: true,
		})
	})

	err = dockerClient.ContainerStart(ctx, nonNomadContainer.ID, container.StartOptions{})
	must.NoError(t, err)

	untrackedNomadContainer, err := dockerClient.ContainerCreate(ctx, &container.Config{
		Image: cfg.Image,
		Cmd:   append([]string{cfg.Command}, cfg.Args...),
		Labels: map[string]string{
			dockerLabelAllocID: uuid.Generate(),
		},
	}, nil, nil, nil, "mytest-image-"+uuid.Generate())
	must.NoError(t, err)
	t.Cleanup(func() {
		_ = dockerClient.ContainerRemove(ctx, untrackedNomadContainer.ID, container.RemoveOptions{
			Force: true,
		})
	})

	err = dockerClient.ContainerStart(ctx, untrackedNomadContainer.ID, container.StartOptions{})
	must.NoError(t, err)

	dd := d.Impl().(*Driver)

	reconciler := newReconciler(dd)
	trackedContainers := set.From([]string{handle.containerID})

	tracked := reconciler.trackedContainers()
	must.Contains[string](t, handle.containerID, tracked)
	must.NotContains[string](t, untrackedNomadContainer.ID, tracked)
	must.NotContains[string](t, nonNomadContainer.ID, tracked)

	// assert tracked containers should never be untracked
	untracked, err := reconciler.untrackedContainers(trackedContainers, time.Now())
	must.NoError(t, err)
	must.NotContains[string](t, handle.containerID, untracked)
	must.NotContains[string](t, nonNomadContainer.ID, untracked)
	must.Contains[string](t, untrackedNomadContainer.ID, untracked)

	// assert we recognize nomad containers with appropriate cutoff
	untracked, err = reconciler.untrackedContainers(set.New[string](0), time.Now())
	must.NoError(t, err)
	must.Contains[string](t, handle.containerID, untracked)
	must.Contains[string](t, untrackedNomadContainer.ID, untracked)
	must.NotContains[string](t, nonNomadContainer.ID, untracked)

	// but ignore if creation happened before cutoff
	untracked, err = reconciler.untrackedContainers(set.New[string](0), time.Now().Add(-1*time.Minute))
	must.NoError(t, err)
	must.NotContains[string](t, handle.containerID, untracked)
	must.NotContains[string](t, untrackedNomadContainer.ID, untracked)
	must.NotContains[string](t, nonNomadContainer.ID, untracked)

	// a full integration tests to assert that containers are removed
	prestineDriver := dockerDriverHarness(t, nil).Impl().(*Driver)
	prestineDriver.config.GC.DanglingContainers = ContainerGCConfig{
		Enabled:       true,
		period:        1 * time.Second,
		CreationGrace: 0 * time.Second,
	}
	nReconciler := newReconciler(prestineDriver)

	err = nReconciler.removeDanglingContainersIteration()
	must.NoError(t, err)

	_, err = dockerClient.ContainerInspect(ctx, nonNomadContainer.ID)
	must.NoError(t, err)

	_, err = dockerClient.ContainerInspect(ctx, handle.containerID)
	must.ErrorContains(t, err, NoSuchContainerError)

	_, err = dockerClient.ContainerInspect(ctx, untrackedNomadContainer.ID)
	must.ErrorContains(t, err, NoSuchContainerError)
}

var (
	dockerNetRe = regexp.MustCompile(`/var/run/docker/netns/[[:xdigit:]]`)
)

func TestDanglingContainerRemoval_network(t *testing.T) {
	ci.Parallel(t)
	testutil.DockerCompatible(t)
	testutil.RequireLinux(t) // bridge implies linux

	dd := dockerDriverHarness(t, nil).Impl().(*Driver)
	reconciler := newReconciler(dd)

	// create a pause container
	allocID := uuid.Generate()
	spec, created, err := dd.CreateNetwork(allocID, &drivers.NetworkCreateRequest{
		Hostname: "hello",
	})
	must.NoError(t, err)
	must.True(t, created)
	must.RegexMatch(t, dockerNetRe, spec.Path)
	id := spec.Labels[dockerNetSpecLabelKey]

	// execute reconciliation
	err = reconciler.removeDanglingContainersIteration()
	must.NoError(t, err)

	dockerClient := newTestDockerClient(t)
	c, iErr := dockerClient.ContainerInspect(context.Background(), id)
	must.NoError(t, iErr)
	must.Eq(t, "running", c.State.Status)

	// cleanup pause container
	err = dd.DestroyNetwork(allocID, spec)
	must.NoError(t, err)
}

// TestDanglingContainerRemoval_Stopped asserts stopped containers without
// corresponding tasks are not removed even if after creation grace period.
func TestDanglingContainerRemoval_Stopped(t *testing.T) {
	ci.Parallel(t)
	testutil.DockerCompatible(t)

	ctx := context.Background()

	_, cfg, _ := dockerTask(t)

	dockerClient := newTestDockerClient(t)
	cont, err := dockerClient.ContainerCreate(ctx, &container.Config{
		Image: cfg.Image,
		Cmd:   append([]string{cfg.Command}, cfg.Args...),
		Labels: map[string]string{
			dockerLabelAllocID: uuid.Generate(),
		},
	}, nil, nil, nil, "mytest-image-"+uuid.Generate())
	must.NoError(t, err)
	t.Cleanup(func() {
		_ = dockerClient.ContainerRemove(ctx, cont.ID, container.RemoveOptions{
			Force: true,
		})
	})

	err = dockerClient.ContainerStart(ctx, cont.ID, container.StartOptions{})
	must.NoError(t, err)

	err = dockerClient.ContainerStop(ctx, cont.ID, container.StopOptions{Timeout: pointer.Of(60)})
	must.NoError(t, err)

	dd := dockerDriverHarness(t, nil).Impl().(*Driver)
	reconciler := newReconciler(dd)

	// assert nomad container is tracked, and we ignore stopped one
	tracked := reconciler.trackedContainers()
	must.NotContains[string](t, cont.ID, tracked)

	checkUntracked := func() error {
		untracked, err := reconciler.untrackedContainers(set.New[string](0), time.Now())
		must.NoError(t, err)
		if untracked.Contains(cont.ID) {
			return fmt.Errorf("container ID %s in untracked set: %v", cont.ID, untracked.Slice())
		}
		return nil
	}

	// retry because it's slower on windows :\
	must.Wait(t, wait.InitialSuccess(
		wait.ErrorFunc(checkUntracked),
		wait.Timeout(time.Second),
		wait.Gap(100*time.Millisecond),
	))

	// if we start container again, it'll be marked as untracked
	must.NoError(t, dockerClient.ContainerStart(ctx, cont.ID, container.StartOptions{}))

	untracked, err := reconciler.untrackedContainers(set.New[string](0), time.Now())
	must.NoError(t, err)
	must.Contains[string](t, cont.ID, untracked)
}
