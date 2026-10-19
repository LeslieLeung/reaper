// Package buildinfo contains metadata for the running binary.
package buildinfo

// Version is set at build time using -ldflags -X. Local builds default to dev.
var Version = "dev"
