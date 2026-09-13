package operations

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gantry-tools/gantry-core/contracttest"
)

func TestOperationContracts(t *testing.T) {
	if err := contracttest.Require(Manifest()); err != nil {
		t.Fatal(err)
	}
	if findings := contracttest.CertificationFindings(Manifest()); len(findings) != 0 {
		t.Fatalf("certification findings: %+v", findings)
	}
}

func TestGeneratedCoverageIsCurrent(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	if err := contracttest.CheckMatrixArtifacts(filepath.Join(root, "docs/generated/functional-coverage.json"), filepath.Join(root, "docs/generated/functional-coverage.md"), Manifest()); err != nil {
		t.Fatal(err)
	}
}
