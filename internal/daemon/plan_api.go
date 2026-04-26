package daemon

// SetPlanExecutor wires the plan executor for UDS plan handlers.
func (d *Daemon) SetPlanExecutor(pe PlanExecutor) {
	d.planExecutor = pe
	if d.api != nil && d.api.plan != nil {
		d.api.plan.SetExecutor(pe)
	}
}
