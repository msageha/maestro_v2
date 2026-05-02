// Package contract holds neutral interface definitions shared by packages
// that would otherwise declare structurally identical interfaces and stitch
// them together with bridge adapters. Centralising the contract lets
// `bridge` perform cross-package handoffs with a direct type assertion.
package contract

// ModelSelector is the optional adaptive model selection hook consulted
// during worker assignment. Implementations must be safe for concurrent use.
// A SelectModel call MAY return an empty string to defer to the static
// BloomLevel→model mapping.
type ModelSelector interface {
	SelectModel(bloomLevel int, taskName string) string
}
