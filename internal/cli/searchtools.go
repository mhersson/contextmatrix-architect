package cli

import (
	"log/slog"
	"os"
)

// warnMissingSearchTools logs at startup when the harness grep tool will fail:
// it execs rg on every call, while glob also runs on fd or Debian's fdfind.
// The server starts regardless; otherwise the gap shows only as tool errors
// in stage transcripts.
func warnMissingSearchTools(logger *slog.Logger, lookPath func(string) (string, error)) {
	found := func(name string) bool {
		_, err := lookPath(name)

		return err == nil
	}

	if found("rg") {
		return
	}

	logger.Warn("ripgrep (rg) is not on PATH: grep fails in every code stage until it is installed",
		"path", os.Getenv("PATH"),
		"glob_available", found("fd") || found("fdfind"))
}
