// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package rawexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/hashicorp/consul-template/signals"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/lib/cgroupslib"
	"github.com/hashicorp/nomad/client/lib/cpustats"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/drivers/shared/executor"
	"github.com/hashicorp/nomad/drivers/shared/validators"
	"github.com/hashicorp/nomad/helper/pluginutils/hclutils"
	"github.com/hashicorp/nomad/helper/pluginutils/loader"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/drivers/fsisolation"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	pstructs "github.com/hashicorp/nomad/plugins/shared/structs"
	"github.com/ryanuber/go-glob"
)

const (
	// pluginName is the name of the plugin
	pluginName = "raw_exec"

	// fingerprintPeriod is the interval at which the driver will send fingerprint responses
	fingerprintPeriod = 30 * time.Second

	// taskHandleVersion is the version of task handle which this driver sets
	// and understands how to decode driver state
	taskHandleVersion = 1
)

var (
	// PluginID is the rawexec plugin metadata registered in the plugin
	// catalog.
	PluginID = loader.PluginID{
		Name:       pluginName,
		PluginType: base.PluginTypeDriver,
	}

	// PluginConfig is the rawexec factory function registered in the
	// plugin catalog.
	PluginConfig = &loader.InternalPluginConfig{
		Config:  map[string]interface{}{},
		Factory: func(ctx context.Context, l hclog.Logger) interface{} { return NewRawExecDriver(ctx, l) },
	}

	errDisabledDriver = fmt.Errorf("raw_exec is disabled")
)

// PluginLoader maps pre-0.9 client driver options to post-0.9 plugin options.
func PluginLoader(opts map[string]string) (map[string]interface{}, error) {
	conf := map[string]interface{}{}
	if v, err := strconv.ParseBool(opts["driver.raw_exec.enable"]); err == nil {
		conf["enabled"] = v
	}
	return conf, nil
}

var (
	// pluginInfo is the response returned for the PluginInfo RPC
	pluginInfo = &base.PluginInfoResponse{
		Type:              base.PluginTypeDriver,
		PluginApiVersions: []string{drivers.ApiVersion010},
		PluginVersion:     "0.1.0",
		Name:              pluginName,
	}

	// configSpec is the hcl specification returned by the ConfigSchema RPC
	configSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"enabled": hclspec.NewDefault(
			hclspec.NewAttr("enabled", "bool", false),
			hclspec.NewLiteral("false"),
		),
		"denied_host_uids": hclspec.NewAttr("denied_host_uids", "string", false),
		"denied_host_gids": hclspec.NewAttr("denied_host_gids", "string", false),
		"denied_envvars":   hclspec.NewAttr("denied_envvars", "list(string)", false),
	})

	// taskConfigSpec is the hcl specification for the driver config section of
	// a task within a job. It is returned in the TaskConfigSchema RPC
	taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"command":            hclspec.NewAttr("command", "string", true),
		"args":               hclspec.NewAttr("args", "list(string)", false),
		"cgroup_v2_override": hclspec.NewAttr("cgroup_v2_override", "string", false),
		"cgroup_v1_override": hclspec.NewAttr("cgroup_v1_override", "list(map(string))", false),
		"oom_score_adj":      hclspec.NewAttr("oom_score_adj", "number", false),
		"work_dir":           hclspec.NewAttr("work_dir", "string", false),
		"denied_envvars":     hclspec.NewAttr("denied_envvars", "list(string)", false),
	})

	// capabilities is returned by the Capabilities RPC and indicates what
	// optional features this driver supports
	capabilities = &drivers.Capabilities{
		SendSignals: true,
		Exec:        true,
		FSIsolation: fsisolation.None,
		NetIsolationModes: []drivers.NetIsolationMode{
			drivers.NetIsolationModeHost,
			drivers.NetIsolationModeGroup,
		},
		MountConfigs: drivers.MountConfigSupportNone,
	}
)

type UserIDValidator interface {
	HasValidIDs(userName string) error
}

// Driver is a privileged version of the exec driver. It provides no
// resource isolation and just fork/execs. The Exec driver should be preferred
// and this should only be used when explicitly needed.
type Driver struct {
	// eventer is used to handle multiplexing of TaskEvents calls such that an
	// event can be broadcast to all callers
	eventer *eventer.Eventer

	// config is the driver configuration set by the SetConfig RPC
	config *Config

	// nomadConfig is the client config from nomad
	nomadConfig *base.ClientDriverConfig

	// tasks is the in memory datastore mapping taskIDs to driverHandles
	tasks *taskStore

	// ctx is the context for the driver. It is passed to other subsystems to
	// coordinate shutdown
	ctx context.Context

	// logger will log to the Nomad agent
	logger hclog.Logger

	// compute contains cpu compute information
	compute cpustats.Compute

	userIDValidator UserIDValidator
}

// Config is the driver configuration set by the SetConfig RPC call
type Config struct {
	// Enabled is set to true to enable the raw_exec driver
	Enabled bool `codec:"enabled"`

	DeniedHostUids string   `codec:"denied_host_uids"`
	DeniedHostGids string   `codec:"denied_host_gids"`
	DeniedEnvvars  []string `codec:"denied_envvars"`
}

// TaskConfig is the driver configuration of a task within a job
type TaskConfig struct {
	Command string   `codec:"command"`
	Args    []string `codec:"args"`

	// OverrideCgroupV2 allows overriding the unified cgroup the task will be
	// become a member of.
	//
	// * All resource isolation guarantees are lost FOR ALL TASKS if set *
	OverrideCgroupV2 string `codec:"cgroup_v2_override"`

	// OverrideCgroupV1 allows overriding per-controller cgroups the task will
	// become a member of.
	//
	// * All resource isolation guarantees are lost FOR ALL TASKS if set *
	OverrideCgroupV1 hclutils.MapStrStr `codec:"cgroup_v1_override"`

	// OOMScoreAdj sets the oom_score_adj on Linux systems
	OOMScoreAdj int `codec:"oom_score_adj"`

	// WorkDir sets the working directory of the task
	WorkDir string `codec:"work_dir"`

	//DeniedEnvvars enables the removal of specified environment variables from a given job environment
	DeniedEnvvars []string `codec:"denied_envvars"`
}

func (t *TaskConfig) validate() error {
	// ensure only one of cgroups_v1_override and cgroups_v2_override have been
	// configured; must check here because task config validation cannot happen
	// on the server.
	if len(t.OverrideCgroupV1) > 0 && t.OverrideCgroupV2 != "" {
		return errors.New("only one of cgroups_v1_override and cgroups_v2_override may be set")
	}
	if t.OOMScoreAdj < 0 {
		return errors.New("oom_score_adj must not be negative")
	}
	if t.WorkDir != "" && !filepath.IsAbs(t.WorkDir) {
		return errors.New("work_dir must be an absolute path")
	}
	return nil
}

// TaskState is the state which is encoded in the handle returned in
// StartTask. This information is needed to rebuild the task state and handler
// during recovery.
type TaskState struct {
	ReattachConfig *pstructs.ReattachConfig
	TaskConfig     *drivers.TaskConfig
	Pid            int
	StartedAt      time.Time
}

// NewRawExecDriver returns a new DriverPlugin implementation
func NewRawExecDriver(ctx context.Context, logger hclog.Logger) drivers.DriverPlugin {
	logger = logger.Named(pluginName)
	return &Driver{
		eventer: eventer.NewEventer(ctx, logger),
		config:  &Config{},
		tasks:   newTaskStore(),
		ctx:     ctx,
		logger:  logger,
	}
}

func (d *Driver) PluginInfo() (*base.PluginInfoResponse, error) {
	return pluginInfo, nil
}

func (d *Driver) ConfigSchema() (*hclspec.Spec, error) {
	return configSpec, nil
}

func (d *Driver) SetConfig(cfg *base.Config) error {
	var config Config

	if len(cfg.PluginConfig) != 0 {
		if err := base.MsgPackDecode(cfg.PluginConfig, &config); err != nil {
			return err
		}
	}

	if d.userIDValidator == nil {
		idValidator, err := validators.NewValidator(d.logger, config.DeniedHostUids, config.DeniedHostGids)
		if err != nil {
			return fmt.Errorf("unable to start validator: %w", err)
		}

		d.userIDValidator = idValidator
	}

	d.config = &config

	if cfg.AgentConfig != nil {
		d.nomadConfig = cfg.AgentConfig.Driver
		d.compute = cfg.AgentConfig.Compute()
	}

	return nil
}

func (d *Driver) TaskConfigSchema() (*hclspec.Spec, error) {
	return taskConfigSpec, nil
}

func (d *Driver) Capabilities() (*drivers.Capabilities, error) {
	return capabilities, nil
}

func (d *Driver) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint)
	go d.handleFingerprint(ctx, ch)
	return ch, nil
}

func (d *Driver) handleFingerprint(ctx context.Context, ch chan<- *drivers.Fingerprint) {
	defer close(ch)
	ticker := time.NewTimer(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			ticker.Reset(fingerprintPeriod)
			ch <- d.buildFingerprint()
		}
	}
}

func (d *Driver) buildFingerprint() *drivers.Fingerprint {
	var health drivers.HealthState
	var desc string
	attrs := map[string]*pstructs.Attribute{}
	if d.config.Enabled {
		health = drivers.HealthStateHealthy
		desc = drivers.DriverHealthy
		attrs["driver.raw_exec"] = pstructs.NewBoolAttribute(true)
	} else {
		health = drivers.HealthStateUndetected
		desc = "disabled"
	}

	return &drivers.Fingerprint{
		Attributes:        attrs,
		Health:            health,
		HealthDescription: desc,
	}
}

func (d *Driver) RecoverTask(handle *drivers.TaskHandle) error {
	if handle == nil {
		return fmt.Errorf("handle cannot be nil")
	}

	// If already attached to handle there's nothing to recover.
	if _, ok := d.tasks.Get(handle.Config.ID); ok {
		d.logger.Trace("nothing to recover; task already exists",
			"task_id", handle.Config.ID,
			"task_name", handle.Config.Name,
		)
		return nil
	}

	// Handle doesn't already exist, try to reattach
	var taskState TaskState
	if err := handle.GetDriverState(&taskState); err != nil {
		d.logger.Error("failed to decode task state from handle", "error", err, "task_id", handle.Config.ID)
		return fmt.Errorf("failed to decode task state from handle: %v", err)
	}

	plugRC, err := pstructs.ReattachConfigToGoPlugin(taskState.ReattachConfig)
	if err != nil {
		d.logger.Error("failed to build ReattachConfig from task state", "error", err, "task_id", handle.Config.ID)
		return fmt.Errorf("failed to build ReattachConfig from task state: %v", err)
	}

	// Create client for reattached executor
	exec, pluginClient, err := executor.ReattachToExecutor(
		plugRC,
		d.logger.With("task_name", handle.Config.Name, "alloc_id", handle.Config.AllocID),
		d.compute,
	)
	if err != nil {
		d.logger.Error("failed to reattach to executor", "error", err, "task_id", handle.Config.ID)
		return fmt.Errorf("failed to reattach to executor: %v", err)
	}

	h := &taskHandle{
		exec:         exec,
		pid:          taskState.Pid,
		pluginClient: pluginClient,
		taskConfig:   taskState.TaskConfig,
		procState:    drivers.TaskStateRunning,
		startedAt:    taskState.StartedAt,
		exitResult:   &drivers.ExitResult{},
		logger:       d.logger,
		doneCh:       make(chan struct{}),
	}

	d.tasks.Set(taskState.TaskConfig.ID, h)

	go h.run()
	return nil
}

func (d *Driver) buildEnvList(tc *TaskConfig, cfg *drivers.TaskConfig) []string {
	// combine tc and cfg denyLists
	denyList := slices.Concat(d.config.DeniedEnvvars, tc.DeniedEnvvars)
	envList := make([]string, 0, len(cfg.Env))

	for k, v := range cfg.Env {
		found := false
		for _, denied := range denyList {
			if found = glob.Glob(denied, k); found {
				break
			}
		}

		if !found {
			envList = append(envList, k+"="+v)
		}
	}
	sort.Strings(envList)
	return envList
}

func (d *Driver) StartTask(cfg *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
	if !d.config.Enabled {
		return nil, nil, errDisabledDriver
	}

	if _, ok := d.tasks.Get(cfg.ID); ok {
		return nil, nil, fmt.Errorf("task with ID %q already started", cfg.ID)
	}

	var driverConfig TaskConfig
	if err := cfg.DecodeDriverConfig(&driverConfig); err != nil {
		return nil, nil, fmt.Errorf("failed to decode driver config: %v", err)
	}

	driverConfig.OverrideCgroupV2 = cgroupslib.CustomPathCG2(driverConfig.OverrideCgroupV2)

	if err := driverConfig.validate(); err != nil {
		return nil, nil, fmt.Errorf("failed driver config validation: %v", err)
	}

	if err := d.Validate(*cfg); err != nil {
		return nil, nil, fmt.Errorf("failed driver config validation: %v", err)
	}

	d.logger.Info("starting task", "driver_cfg", hclog.Fmt("%+v", driverConfig))
	handle := drivers.NewTaskHandle(taskHandleVersion)
	handle.Config = cfg

	pluginLogFile := filepath.Join(cfg.TaskDir().Dir, "executor.out")
	executorConfig := &executor.ExecutorConfig{
		LogFile:  pluginLogFile,
		LogLevel: "debug",
		Compute:  d.compute,
	}

	logger := d.logger.With("task_name", handle.Config.Name, "alloc_id", handle.Config.AllocID)
	exec, pluginClient, err := executor.CreateExecutor(logger, d.nomadConfig, executorConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create executor: %v", err)
	}

	execCmd := &executor.ExecCommand{
		Cmd:              driverConfig.Command,
		Args:             driverConfig.Args,
		Env:              d.buildEnvList(&driverConfig, cfg),
		User:             cfg.User,
		TaskDir:          cfg.TaskDir().Dir,
		WorkDir:          driverConfig.WorkDir,
		StdoutPath:       cfg.StdoutPath,
		StderrPath:       cfg.StderrPath,
		NetworkIsolation: cfg.NetworkIsolation,
		Resources:        cfg.Resources.Copy(),
		OverrideCgroupV2: driverConfig.OverrideCgroupV2,
		OverrideCgroupV1: driverConfig.OverrideCgroupV1,
		OOMScoreAdj:      int32(driverConfig.OOMScoreAdj),
	}

	ps, err := exec.Launch(execCmd)
	if err != nil {
		pluginClient.Kill()
		return nil, nil, fmt.Errorf("failed to launch command with executor: %v", err)
	}

	h := &taskHandle{
		exec:         exec,
		pid:          ps.Pid,
		pluginClient: pluginClient,
		taskConfig:   cfg,
		procState:    drivers.TaskStateRunning,
		startedAt:    time.Now().Round(time.Millisecond),
		logger:       d.logger,
		doneCh:       make(chan struct{}),
	}

	driverState := TaskState{
		ReattachConfig: pstructs.ReattachConfigFromGoPlugin(pluginClient.ReattachConfig()),
		Pid:            ps.Pid,
		TaskConfig:     cfg,
		StartedAt:      h.startedAt,
	}

	if err := handle.SetDriverState(&driverState); err != nil {
		d.logger.Error("failed to start task, error setting driver state", "error", err)
		_ = exec.Shutdown("", 0)
		pluginClient.Kill()
		return nil, nil, fmt.Errorf("failed to set driver state: %v", err)
	}

	d.tasks.Set(cfg.ID, h)
	go h.run()
	return handle, nil, nil
}

func (d *Driver) WaitTask(ctx context.Context, taskID string) (<-chan *drivers.ExitResult, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	ch := make(chan *drivers.ExitResult)
	go d.handleWait(ctx, handle, ch)

	return ch, nil
}

func (d *Driver) handleWait(ctx context.Context, handle *taskHandle, ch chan *drivers.ExitResult) {
	defer close(ch)
	var result *drivers.ExitResult
	ps, err := handle.exec.Wait(ctx)
	if err != nil {
		result = &drivers.ExitResult{
			Err: fmt.Errorf("executor: error waiting on process: %v", err),
		}
		// if process state is nil, we've probably been killed, so return a reasonable
		// exit state to the handlers
		if ps == nil {
			result.ExitCode = -1
			result.OOMKilled = false
		}
	} else {
		result = &drivers.ExitResult{
			ExitCode:  ps.ExitCode,
			Signal:    ps.Signal,
			OOMKilled: ps.OOMKilled,
		}
	}

	select {
	case <-ctx.Done():
		return
	case <-d.ctx.Done():
		return
	case ch <- result:
	}
}

func (d *Driver) StopTask(taskID string, timeout time.Duration, signal string) error {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	if err := handle.exec.Shutdown(signal, timeout); err != nil {
		if handle.pluginClient.Exited() {
			return nil
		}
		return fmt.Errorf("executor Shutdown failed: %v", err)
	}

	// Wait for handle to finish
	<-handle.doneCh

	// Kill executor
	handle.pluginClient.Kill()

	return nil
}

func (d *Driver) DestroyTask(taskID string, force bool) error {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	if handle.IsRunning() && !force {
		return fmt.Errorf("cannot destroy running task")
	}

	if !handle.pluginClient.Exited() {
		if err := handle.exec.Shutdown("", 0); err != nil {
			handle.logger.Error("destroying executor failed", "error", err)
		}

		handle.pluginClient.Kill()
	}

	d.tasks.Delete(taskID)
	return nil
}

func (d *Driver) InspectTask(taskID string) (*drivers.TaskStatus, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	return handle.TaskStatus(), nil
}

func (d *Driver) TaskStats(ctx context.Context, taskID string, interval time.Duration) (<-chan *drivers.TaskResourceUsage, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	return handle.exec.Stats(ctx, interval)
}

func (d *Driver) TaskEvents(ctx context.Context) (<-chan *drivers.TaskEvent, error) {
	return d.eventer.TaskEvents(ctx)
}

func (d *Driver) SignalTask(taskID string, signal string) error {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	sig := os.Interrupt
	if s, ok := signals.SignalLookup[signal]; ok {
		sig = s
	} else {
		d.logger.Warn("unknown signal to send to task, using SIGINT instead", "signal", signal, "task_id", handle.taskConfig.ID)
	}

	return handle.exec.Signal(sig)
}

func (d *Driver) ExecTask(taskID string, cmd []string, timeout time.Duration) (*drivers.ExecTaskResult, error) {
	if len(cmd) == 0 {
		return nil, fmt.Errorf("error cmd must have at least one value")
	}
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	out, exitCode, err := handle.exec.Exec(time.Now().Add(timeout), cmd[0], cmd[1:])
	if err != nil {
		return nil, err
	}

	return &drivers.ExecTaskResult{
		Stdout: out,
		ExitResult: &drivers.ExitResult{
			ExitCode: exitCode,
		},
	}, nil
}

var _ drivers.ExecTaskStreamingRawDriver = (*Driver)(nil)

func (d *Driver) ExecTaskStreamingRaw(ctx context.Context,
	taskID string,
	command []string,
	tty bool,
	stream drivers.ExecTaskStream) error {

	if len(command) == 0 {
		return fmt.Errorf("error cmd must have at least one value")
	}
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	return handle.exec.ExecStreaming(ctx, command, tty, stream)
}
