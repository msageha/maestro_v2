// Package templates embeds default configuration and instruction files.
package templates

import "embed"

// FS is the embedded filesystem containing default config, templates, and skills.
//
//go:embed config.yaml dashboard.md maestro.md instructions persona skills
var FS embed.FS
