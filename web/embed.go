// web/embed.go
// Exposes the embedded static UI files to cmd/server.
// The //go:embed directive must be in the same package directory as the files.
package web

import "embed"

//go:embed static
var Static embed.FS
