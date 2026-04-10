package daemon

import (
	"log"
	"sync"

	"github.com/msageha/maestro_v2/internal/model"
)

// baseHandler provides shared fields and methods for daemon handlers
// (Dispatcher, ResultHandler, CancelHandler).
type baseHandler struct {
	maestroDir   string
	config       model.Config
	dl           *DaemonLogger
	logger       *log.Logger
	logLevel     LogLevel
	clock        Clock
	execProvider ExecutorGetter
	mu           sync.RWMutex
}

func (b *baseHandler) log(level LogLevel, format string, args ...any) {
	b.dl.Logf(level, format, args...)
}

func (b *baseHandler) getExecutor() (AgentExecutor, error) {
	return b.execProvider.GetExecutor()
}
