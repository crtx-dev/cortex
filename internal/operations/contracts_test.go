package operations

import (
	"github.com/gantry-tools/gantry-core/contracttest"
	"testing"
)

func TestAdoptionContract(t *testing.T) {
	if err := contracttest.Require(AdoptionManifest()); err != nil {
		t.Fatal(err)
	}
}
