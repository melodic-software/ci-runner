// Package buildinfo contains release metadata injected by the immutable build.
package buildinfo

// Version is overridden by release builds with:
//
//	-X github.com/melodic-software/ci-runner/internal/buildinfo.Version=<version>
var Version = "dev"
