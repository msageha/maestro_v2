package daemon

import (
	"github.com/msageha/maestro_v2/internal/daemon/daemonapi"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

func newAPI(d *Daemon) *API {
	shared := &apiContext{
		maestroDir: d.maestroDir,
		config:     &d.config,
		clock:      d.clock,
		lockMap:    d.lockMap,
		logFn:      d.log,
		logger:     d.logger,
		logLevel:   d.logLevel,
		selfWrites: d.selfWrites,
		fileStore:  newFSResultFileStore(d.maestroDir),
		eventBus:   d.eventBusAccessor,
		spawnTask:  d.spawnTracked,
	}

	queueWrite := &QueueWriteAPI{apiContext: shared}
	resultWrite := &ResultWriteAPI{
		apiContext:     shared,
		fallbackMgr:    d.fallbackAccessor,
		circuitBreaker: d.circuitBreakerAccessor,
		reviewCoord:    d.reviewCoordAccessor,
		ctx:            d.contextAccessor,
		triggerScan:    d.triggerResultWriteScan,
		statusSink:     tmuxAgentStatusSink{},
		// §S1-1: verifyRunner is left nil here on purpose. Production
		// startup calls SetVerifyRunner with either NewRealVerifyRunner or
		// NewSkipVerifyRunner. resolveVerifyRunner emits a fail-closed
		// outcome if neither path runs, so a wiring miss surfaces as a
		// verify failure instead of a silent pass.
		verifyRunner: nil,
	}
	return &API{
		shared: shared,
		result: resultWrite,
		resultUDS: daemonapi.NewResultWrite(func(params daemonapi.ResultWriteParams, status model.Status) *uds.Response {
			return resultWrite.handleValidatedResultWrite(params, status)
		}),
		queue: queueWrite,
		queueUDS: daemonapi.NewQueueWrite(
			queueWrite.handleQueueWriteCommand,
			queueWrite.handleQueueWriteTask,
			queueWrite.handleQueueWriteNotification,
			queueWrite.handleQueueWriteCancelRequest,
		),
		plan: daemonapi.NewPlan(
			d.maestroDir,
			shared.acquireFileLock,
			shared.releaseFileLock,
			commandStatePath,
			shared.publishQueueWritten,
			func(format string, args ...any) { d.log(LogLevelInfo, format, args...) },
			func(format string, args ...any) { d.log(LogLevelWarn, format, args...) },
		),
		heartbeat: daemonapi.NewHeartbeat(taskHeartbeatFactory(d)),
		dashboard: daemonapi.NewDashboard(
			d.maestroDir,
			func() bool { return d.handler != nil },
			func(maestroDir string) error {
				return NewDashboardFormatter(maestroDir).UpdateDashboardFile()
			},
			func(format string, args ...any) {
				d.log(LogLevelError, format, args...)
			},
		),
		skill: daemonapi.NewSkill(
			d.maestroDir,
			d.lockMap,
			func(format string, args ...any) { d.log(LogLevelInfo, format, args...) },
			func(format string, args ...any) { d.log(LogLevelWarn, format, args...) },
		),
		verify: daemonapi.NewVerifyWrite(
			d.maestroDir,
			shared.acquireFileLock,
			shared.releaseFileLock,
			func(format string, args ...any) { d.log(LogLevelInfo, format, args...) },
			func(format string, args ...any) { d.log(LogLevelWarn, format, args...) },
			func() bool { return d.config.Verify.EffectiveEnabled() },
		),
	}
}

func taskHeartbeatFactory(d *Daemon) daemonapi.HandlerFactory {
	return func() daemonapi.RequestHandler {
		if d.handler == nil || d.handler.leaseManager == nil || d.handler.scanExecutor == nil {
			return nil
		}
		return NewTaskHeartbeatHandler(
			d.maestroDir,
			d.config,
			d.handler.leaseManager,
			d.logger,
			d.logLevel,
			&d.handler.scanExecutor.scanMu,
			d.lockMap,
			WithHeartbeatSelfWrites(d.selfWrites),
		)
	}
}
