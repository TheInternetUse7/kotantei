package buildinfo

import (
	"runtime"
	"testing"
)

func TestUnstampedBuildHasDevelopmentIdentity(t *testing.T) {
	info := Current()
	if info.Version != "dev" || info.Revision != "" || info.SourceURL != "" || info.License != "AGPL-3.0-only" || info.GoVersion != runtime.Version() {
		t.Fatal("unexpected unstamped identity", info)
	}
}

func TestCurrentUsesExplicitReleaseIdentity(t *testing.T) {
	version, revision, source := Version, Revision, SourceURL
	t.Cleanup(func() { Version, Revision, SourceURL = version, revision, source })
	Version, Revision, SourceURL = "v1.2.3-rc.1", "0123456789012345678901234567890123456789", "https://example.com/source.tar.gz"
	info := Current()
	if info.Version != Version || info.Revision != Revision || info.SourceURL != SourceURL || info.GoVersion != runtime.Version() || info.License != License {
		t.Fatal("explicit release identity was lost", info)
	}
}
