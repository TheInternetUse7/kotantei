// Package buildinfo exposes identity supplied by the release build, not Git discovery.
package buildinfo

import "runtime"

// Release builds set these with -ldflags -X. Unstamped local builds stay "dev".
var (
	Version   = "dev"
	Revision  = ""
	SourceURL = ""
)

const License = "AGPL-3.0-only"

type Info struct {
	Version   string `json:"version"`
	Revision  string `json:"revision,omitempty"`
	SourceURL string `json:"source_url,omitempty"`
	License   string `json:"license"`
	GoVersion string `json:"go_version"`
}

func Current() Info {
	return Info{Version: Version, Revision: Revision, SourceURL: SourceURL, License: License, GoVersion: runtime.Version()}
}
