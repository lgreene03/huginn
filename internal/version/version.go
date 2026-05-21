// Package version exposes the build-time identity of the Huginn binary.
// Variables are injected at link time via -ldflags:
//
//	go build -ldflags "-X github.com/lgreene03/huginn/internal/version.Version=v0.1.0 \
//	  -X github.com/lgreene03/huginn/internal/version.GitSHA=$(git rev-parse --short HEAD) \
//	  -X github.com/lgreene03/huginn/internal/version.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
package version

// Version is the semantic version tag (e.g. "v0.1.0").
// Defaults to "dev" when built without -ldflags.
var Version = "dev"

// GitSHA is the abbreviated git commit hash at build time.
var GitSHA = "unknown"

// BuildTime is the RFC-3339 UTC timestamp of the build.
var BuildTime = "unknown"

// Info bundles all three fields for structured logging and JSON serialisation.
type Info struct {
	Version   string `json:"version"`
	GitSHA    string `json:"git_sha"`
	BuildTime string `json:"build_time"`
}

// Get returns the current build identity.
func Get() Info {
	return Info{
		Version:   Version,
		GitSHA:    GitSHA,
		BuildTime: BuildTime,
	}
}
