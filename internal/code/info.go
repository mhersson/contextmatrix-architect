package code

import (
	"os"
	"path/filepath"
)

// Info is what repo_info reports about a checkout beyond its branch and sha.
type Info struct {
	// InstructionFile is "AGENTS.md", "CLAUDE.md" or "" in that precedence,
	// the file the agent injects into every phase.
	InstructionFile string
}

// Inspect reads the facts off a checkout.
func Inspect(dir string) Info {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return Info{InstructionFile: name}
		}
	}

	return Info{}
}
