package model

// Worker capability tags (issue #34: capability-based worker routing).
//
// A capability describes what a worker's runtime is good at ("得意分野"),
// orthogonal to Bloom level (which measures task complexity). Tasks declare
// required / preferred capabilities and plan.AssignWorkers narrows the
// candidate worker set before the existing bloom-family + least-loaded
// selection runs.
//
// The vocabulary is intentionally a small fixed set. Matching is exact
// string comparison, so custom tags configured under
// agents.workers.capabilities also work as long as tasks spell them
// identically — the constants below are the documented, recommended set.
const (
	// CapabilityCodeQuality — high-quality code with strong human-eval
	// results on expert tasks (claude-code default).
	CapabilityCodeQuality = "code_quality"
	// CapabilityDesignReview — architecture / design review work
	// (claude-code default).
	CapabilityDesignReview = "design_review"
	// CapabilityInstructionFollowing — precise adherence to detailed
	// instructions and constraints (claude-code default).
	CapabilityInstructionFollowing = "instruction_following"
	// CapabilityRunOnMain — eligible for run_on_main tasks. Only claude-code
	// enforces the read-only main guard (PreToolUse policy hook), so this is
	// a claude-code default. Note: the run_on_main task flag keeps its own
	// hard RequireClaudeRuntime enforcement independent of this tag.
	CapabilityRunOnMain = "run_on_main"

	// CapabilityBulkImplementation — high-volume implementation: many PRs,
	// migrations, mass refactors (codex default).
	CapabilityBulkImplementation = "bulk_implementation"
	// CapabilityLongHorizonAutonomy — long-running autonomous terminal
	// sessions with very long tool-call chains (codex default).
	CapabilityLongHorizonAutonomy = "long_horizon_autonomy"
	// CapabilityRefactor — large-scale refactoring / migration work
	// (codex default).
	CapabilityRefactor = "refactor"

	// CapabilityMultimodal — native video / audio / image / PDF ingestion
	// (gemini default).
	CapabilityMultimodal = "multimodal"
	// CapabilityLongContextIngest — very long context windows for whole-repo
	// or large-document ingestion (gemini default).
	CapabilityLongContextIngest = "long_context_ingest"
	// CapabilityResearch — real-time research backed by web search
	// (gemini default).
	CapabilityResearch = "research"
	// CapabilityCostEfficient — low-cost execution for high-volume,
	// low-stakes work (gemini default).
	CapabilityCostEfficient = "cost_efficient"
)

// DefaultCapabilitiesForRuntime returns the default capability tags implied
// by a runtime name (RuntimeClaudeCode / RuntimeCodex / RuntimeGemini).
// Workers without an explicit agents.workers.capabilities entry advertise
// this set, derived from their configured model via ParseRuntimeFromModel.
// Unknown runtimes return nil (no implied capabilities).
//
// The result is a fresh slice on every call so callers may append or mutate
// without corrupting the defaults.
func DefaultCapabilitiesForRuntime(runtime string) []string {
	switch runtime {
	case RuntimeClaudeCode:
		return []string{
			CapabilityCodeQuality,
			CapabilityDesignReview,
			CapabilityInstructionFollowing,
			CapabilityRunOnMain,
		}
	case RuntimeCodex:
		return []string{
			CapabilityBulkImplementation,
			CapabilityLongHorizonAutonomy,
			CapabilityRefactor,
		}
	case RuntimeGemini:
		return []string{
			CapabilityMultimodal,
			CapabilityLongContextIngest,
			CapabilityResearch,
			CapabilityCostEfficient,
		}
	default:
		return nil
	}
}
