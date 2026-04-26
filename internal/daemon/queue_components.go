package daemon

import (
	"log"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/metrics"
	"github.com/msageha/maestro_v2/internal/model"
)

type queueComponents struct {
	clock               Clock
	execProvider        *ExecutorProvider
	queueStore          QueueStore
	leaseManager        QueueLeaseManager
	dispatcher          QueueDispatcher
	dependencyResolver  QueueDependencyResolver
	cancelHandler       *CancelHandler
	resultHandler       *ResultHandler
	reconciler          *Reconciler
	deadLetterProcessor *DeadLetterProcessor
	metricsHandler      *metrics.Handler
	dl                  *DaemonLogger
}

func newQueueComponents(maestroDir string, cfg model.Config, lockMap *lock.MutexMap, logger *log.Logger, logLevel LogLevel) queueComponents {
	clock := RealClock{}
	factory := ExecutorFactory(func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
		return agent.NewExecutor(dir, wcfg, level)
	})
	ep := NewExecutorProvider(maestroDir, cfg.Watcher, cfg.Logging.Level, factory, clock)
	lm := NewLeaseManager(cfg.Watcher, logger, logLevel)
	dl := NewDaemonLoggerFromLegacy("queue_handler", logger, logLevel)
	rh := NewResultHandler(maestroDir, cfg, lockMap, logger, logLevel, ep, clock)

	return queueComponents{
		clock:               clock,
		execProvider:        ep,
		queueStore:          NewQueueStore(maestroDir, cfg, clock, lockMap, dl),
		leaseManager:        lm,
		dispatcher:          NewDispatcher(maestroDir, cfg, lm, logger, logLevel, ep, clock),
		dependencyResolver:  NewDependencyResolver(nil, logger, logLevel),
		cancelHandler:       NewCancelHandler(maestroDir, cfg, lockMap, logger, logLevel, ep),
		resultHandler:       rh,
		reconciler:          NewReconciler(maestroDir, cfg, lockMap, logger, logLevel, rh, ep.Factory()),
		deadLetterProcessor: NewDeadLetterProcessor(maestroDir, cfg, lockMap, logger, logLevel),
		metricsHandler:      newMetricsHandler(maestroDir, logger, logLevel, clock),
		dl:                  dl,
	}
}
