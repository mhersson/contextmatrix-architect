package code

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
func Inspect(ctx context.Context, dir string) (Info, error) {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if err := ctx.Err(); err != nil {
			return Info{}, err
		}

		_, err := os.Stat(filepath.Join(dir, name))
		if err == nil {
			return Info{InstructionFile: name}, nil
		}

		if !errors.Is(err, fs.ErrNotExist) {
			return Info{}, fmt.Errorf("code: inspect %s: %w", name, err)
		}
	}

	return Info{}, nil
}
