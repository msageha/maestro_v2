package plan

import (
	"sort"
	"strconv"
	"strings"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

func lockQueueKeys(lockMap *lock.MutexMap, keys []string) func() {
	if lockMap == nil || len(keys) == 0 {
		return func() {}
	}
	keys = uniqueQueueKeys(keys)
	sort.Slice(keys, func(i, j int) bool {
		return queueKeyOrder(keys[i]) < queueKeyOrder(keys[j])
	})
	for _, key := range keys {
		lockMap.Lock(key)
	}
	return func() {
		for i := len(keys) - 1; i >= 0; i-- {
			lockMap.Unlock(keys[i])
		}
	}
}

func lockWorkerQueues(lockMap *lock.MutexMap, workerIDs []string) func() {
	keys := make([]string, 0, len(workerIDs))
	for _, workerID := range workerIDs {
		keys = append(keys, "queue:"+workerID)
	}
	return lockQueueKeys(lockMap, keys)
}

func workerIDsFromAssignments(assignments []WorkerAssignment) []string {
	workerIDs := make([]string, 0, len(assignments))
	for _, a := range assignments {
		workerIDs = append(workerIDs, a.WorkerID)
	}
	return workerIDs
}

func allConfiguredWorkerIDs(cfg model.WorkerConfig) []string {
	workerIDs := make([]string, 0, cfg.Count)
	for i := 1; i <= cfg.Count; i++ {
		workerIDs = append(workerIDs, "worker"+strconv.Itoa(i))
	}
	return workerIDs
}

func uniqueQueueKeys(keys []string) []string {
	seen := make(map[string]bool, len(keys))
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

func queueKeyOrder(key string) string {
	switch key {
	case "queue:planner":
		return "00:"
	case "queue:orchestrator":
		return "90:"
	case "queue:planner_signals":
		return "99:"
	default:
		if strings.HasPrefix(key, "queue:worker") {
			return "10:" + key
		}
		return "50:" + key
	}
}
