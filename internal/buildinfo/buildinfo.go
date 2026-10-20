// Package buildinfo reports the build the server runs: the version, commit
// and build time stamped by ldflags, and the merge history embedded at build.
package buildinfo

import (
	"embed"
	"io/fs"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/mhersson/contextmatrix-protocol/changelog"
)

// Set with -ldflags -X by the Makefile and the Dockerfile; empty in a bare
// go build.
var (
	version   string
	commit    string
	buildTime string
)

// all: is required because the directory holds only .gitkeep in a fresh
// clone, and a plain pattern skips dot files and would match nothing.
//
//go:embed all:history
var history embed.FS

// Info returns the build this binary was made from.
func Info() protocol.BuildInfo {
	return info(history)
}

func info(fsys fs.FS) protocol.BuildInfo {
	// In practice the only read error is a missing file, which means no
	// history was recorded; nil raw then parses to an empty list.
	raw, _ := fs.ReadFile(fsys, "history/merges.log")

	return protocol.BuildInfo{
		Version:   version,
		Commit:    commit,
		BuildTime: buildTime,
		Changes:   changelog.Parse(raw, changelog.Limit),
	}
}
