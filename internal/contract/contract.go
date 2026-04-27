// Package contract holds neutral interface definitions shared by packages
// that would otherwise need to declare structurally identical interfaces and
// stitch them together with bridge adapters.
//
// F-039: ModelSelector previously existed independently in `internal/plan`
// and `internal/daemon/core`. Both versions had the exact same single
// method, but the cross-package handoff in `internal/bridge` had to keep
// adapter shims to convert between them. Centralising the contract here
// removes the structural duplication and lets `bridge` perform the handoff
// with a direct type assertion.
package contract

// ModelSelector is the optional adaptive model selection hook consulted
// during worker assignment. Implementations must be safe for concurrent use.
// A SelectModel call MAY return an empty string to defer to the static
// BloomLevel→model mapping.
type ModelSelector interface {
	SelectModel(bloomLevel int, taskName string) string
}
