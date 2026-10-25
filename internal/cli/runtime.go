package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/config"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
)

// runtimeDeps is what serve loads beyond the config's own values. config
// validate loads it too, so a file validate passes is one serve starts with.
type runtimeDeps struct {
	pack *pack.Pack
}

// loadRuntime loads what a validated cfg names: the pack, embedded or
// cfg.Pack, whose personas every models key must name.
func loadRuntime(cfg *config.Config) (*runtimeDeps, error) {
	pk, err := loadPack(cfg.Pack)
	if err != nil {
		return nil, err
	}

	if err := checkModels(cfg.Models, pk); err != nil {
		return nil, err
	}

	return &runtimeDeps{pack: pk}, nil
}

func loadPack(dir string) (*pack.Pack, error) {
	if dir == "" {
		return pack.LoadEmbedded()
	}

	pk, err := pack.LoadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("load pack %s: %w", dir, err)
	}

	return pk, nil
}

// checkModels refuses a models key that names no persona of pk: its
// override would never apply.
func checkModels(models map[string]string, pk *pack.Pack) error {
	personas := pk.Personas()

	var unknown []string

	for name := range models {
		if !slices.Contains(personas, name) {
			unknown = append(unknown, name)
		}
	}

	if len(unknown) == 0 {
		return nil
	}

	slices.Sort(unknown)

	return fmt.Errorf("models: %s names no persona of pack %s; its personas are %s",
		strings.Join(unknown, ", "), pk.Name, strings.Join(personas, ", "))
}
