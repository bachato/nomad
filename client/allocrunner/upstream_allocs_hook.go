// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package allocrunner

import (
	"context"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/client/taskenv"
)

// upstreamAllocsHook waits for a PrevAllocWatcher to exit before allowing
// an allocation to be executed
type upstreamAllocsHook struct {
	allocWatcher config.PrevAllocWatcher
	logger       log.Logger
}

func newUpstreamAllocsHook(logger log.Logger, allocWatcher config.PrevAllocWatcher) *upstreamAllocsHook {
	h := &upstreamAllocsHook{
		allocWatcher: allocWatcher,
	}
	h.logger = logger.Named(h.Name())
	return h
}

// statically assert the hook implements the expected interfaces
var _ interfaces.RunnerPrerunHook = (*upstreamAllocsHook)(nil)

func (h *upstreamAllocsHook) Name() string {
	return "await_previous_allocations"
}

func (h *upstreamAllocsHook) Prerun(_ *taskenv.TaskEnv) error {
	// Wait for a previous alloc - if any - to terminate
	return h.allocWatcher.Wait(context.Background())
}
