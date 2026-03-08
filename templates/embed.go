// Package templates embeds default configuration and instruction files.
package templates

import "embed"

//go:embed config.yaml dashboard.md maestro.md instructions persona
var FS embed.FS
