package version

import (
	"os"
	"regexp"
	"testing"
)

// The version lives in version.go and is noted in README.md. A release that
// bumps only one of them fails here, and with VEP_RELEASE_TAG set (the tag
// about to be pushed, e.g. v1.0) so does a tag that names a version the code
// doesn't carry.
func TestVersionNoted(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+(\.\d+)?$`).MatchString(Version) {
		t.Fatalf("Version %q is not like 0.9 or 1.0.1", Version)
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Skipf("no README.md to compare: %v", err)
	}
	want := "**Version:** " + Label()
	if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(want) + `$`).Match(readme) {
		t.Errorf("README.md needs the line %q (bump it with version.go)", want)
	}
	if tag := os.Getenv("VEP_RELEASE_TAG"); tag != "" && tag != "v"+Version {
		t.Errorf("release tag %q doesn't match version.go (v%s): bump Version first", tag, Version)
	}
}
