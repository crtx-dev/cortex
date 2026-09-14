package app

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestConcurrentAgentStartOneWinner proves the one-running-run guard is
// deterministic under concurrent submission: exactly one start succeeds and
// every other caller receives the clean "already running" conflict, never a
// spurious database error.
func TestConcurrentAgentStartOneWinner(t *testing.T) {
	a := hardeningTestApp(t)
	const callers = 32
	results := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			err := a.startAgentRun(fmt.Sprintf("run-%d", n), "conv-1", "prompt", "/", "provider", "model")
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	winners, alreadyRunning, unexpected := 0, 0, 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case strings.Contains(err.Error(), "already running"):
			alreadyRunning++
		default:
			unexpected++
			t.Logf("unexpected error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners=%d want 1", winners)
	}
	if alreadyRunning != callers-1 {
		t.Fatalf("already-running=%d unexpected=%d, want %d/0", alreadyRunning, unexpected, callers-1)
	}
}
