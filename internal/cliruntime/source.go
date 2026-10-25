package cliruntime

import "embed"

// Sources contains the runtime copied into each independent generated module.
// Tests exercise these exact files before they are embedded.
//
//go:embed runtime.go input.go auth.go
var Sources embed.FS
