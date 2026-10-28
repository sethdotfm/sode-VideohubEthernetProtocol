// Package version is the release number shown on the page and in the logs.
package version

// Bump both with every release, together with the "Version:" line in
// README.md; TestVersionNoted fails when they disagree, and (with
// VEP_RELEASE_TAG=v1.0) when the tag about to be pushed differs.
const (
	Stage   = "Beta"
	Version = "1.0"
)

// Label is how the version reads in the UI and README.
func Label() string { return Stage + " " + Version }
