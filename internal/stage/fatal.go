package stage

import (
	"fmt"
	"strings"
)

// ReasonFatalCheck is a run's reason when a check refused its submit over a
// problem the next step could not build on, with no submit left to give.
const ReasonFatalCheck = "fatal_check"

// FatalCheckError is a submit validator's refusal that ends the run: the
// artifact would leave the next step nothing to build on, and the check has
// no submit left. Problems are the ones that make it so.
type FatalCheckError struct {
	Problems []string
}

func (e *FatalCheckError) Error() string {
	return fmt.Sprintf("the submit still had %d problem(s) that leave the next step nothing to build on, so it was not stored: %s",
		len(e.Problems), strings.Join(e.Problems, "; "))
}
