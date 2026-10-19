package blueprint

import "github.com/mhersson/contextmatrix-architect/internal/store"

// withPlain adds the pack's plain-language instructions after a run's own
// when the blueprint is plain; otherwise the instructions are unchanged.
func withPlain(bp store.Blueprint, instructions, plain string) string {
	if !bp.PlainLanguage || plain == "" {
		return instructions
	}

	return instructions + "\n\n" + plain
}
