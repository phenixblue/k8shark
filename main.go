package main

import (
	_ "embed"

	"github.com/phenixblue/k8shark/cmd"
)

// kwokStages is the bundled KWOK "fast" Stages config, embedded so
// `replay --writable --with-kwok` can run a detected kwok without a checkout.
// It is the same file documented for the manual walkthrough. Embedded here (the
// module root) because go:embed cannot reach a sibling directory from cmd/.
//
//go:embed examples/kwok-stages.yaml
var kwokStages []byte

func main() {
	cmd.KwokStages = kwokStages
	cmd.Execute()
}
