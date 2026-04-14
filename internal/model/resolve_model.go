package model

// ResolveWorkerModel determines the model for a given worker ID from config.
// When boost is enabled, all workers are promoted to opus. Otherwise the
// resolution order is: per-worker override → default model → "sonnet".
func ResolveWorkerModel(workerID string, cfg Config) string {
	if cfg.Agents.Workers.Boost {
		return "opus"
	}
	if m, ok := cfg.Agents.Workers.Models[workerID]; ok {
		return m
	}
	if cfg.Agents.Workers.DefaultModel != "" {
		return cfg.Agents.Workers.DefaultModel
	}
	return "sonnet"
}
