package simple

import (
	"testing"

	"github.com/bnulwh/goreporter/linters/simplecode/lint/testutil"
)

func TestAll(t *testing.T) {
	testutil.TestAll(t, Funcs, "../../")
}
