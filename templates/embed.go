package templates

import "embed"

//go:embed config.yaml dashboard.md maestro.md instructions
var FS embed.FS
