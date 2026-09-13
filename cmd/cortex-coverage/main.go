package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/crtx-dev/cortex/internal/operations"
	"github.com/gantry-tools/gantry-core/contracttest"
)

func main() {
	root := "."
	if len(os.Args) == 2 {
		root = os.Args[1]
	} else if len(os.Args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: cortex-coverage [repository-root]")
		os.Exit(2)
	}
	if err := contracttest.WriteMatrixArtifacts(
		filepath.Join(root, "docs/generated/functional-coverage.json"),
		filepath.Join(root, "docs/generated/functional-coverage.md"),
		operations.Manifest(),
	); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
