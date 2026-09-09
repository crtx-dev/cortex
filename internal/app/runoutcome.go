package app

import (
	"io"
	"strings"
	"sync"

	coreagent "github.com/gantry-tools/gantry-core/agent"
)

// runCause enumerates the local cancellation causes a run may record before
// the process is stopped. Only the first accepted cause is retained; the
// transition rules below guarantee user-initiated or output-limit causes are
// never silently downgraded to a generic request cancellation.
type runCause string

const (
	causeNone            runCause = ""
	causeUserStop        runCause = "user_stop"
	causeRequestCanceled runCause = "request_cancelled"
	causeOutputLimit     runCause = "output_limit"
	causeServiceShutdown runCause = "service_shutdown"
	// causeProviderInsufficientBalance is a derived provider-failure cause,
	// distinct from any local termination cause. It is recorded on the
	// diagnostic/run cause and used by the classifier to derive a failed
	// outcome with an actionable billing message; it never implies the
	// process was locally cancelled.
	causeProviderInsufficientBalance runCause = "provider_insufficient_balance"
)

// runOutcome is the terminal classification persisted for a run. It mirrors
// the conversation states and the frontend badges.
type runOutcome string

const (
	outcomeCompleted       runOutcome = "completed"
	outcomeCompletedWError runOutcome = "completed_with_process_error"
	outcomeFailed          runOutcome = "failed"
	outcomeCancelled       runOutcome = "cancelled"
	outcomeTruncated       runOutcome = "truncated"
	outcomeInterrupted     runOutcome = "interrupted"
)

// runState is the synchronized cancellation-cause machine for one active run.
// Several goroutines (Stop endpoint, request-context watcher, output limiter,
// shutdown, process completion) may attempt to record a cause concurrently, so
// all transitions happen under a mutex and stop once the run is sealed.
//
// The sequence counter orders observable stdout events against accepted
// causes deterministically: an authoritative stdout error observed before the
// terminating cause is accepted forces a failed outcome even when a later
// cancellation was requested.
type runState struct {
	core *coreagent.RunState
}

// runStateSnapshot is a consistent view of the machine used by the classifier.
type runStateSnapshot struct {
	cause          runCause
	errSeq         uint64
	causeSeq       uint64
	stopSeq        uint64
	providerErrSeq uint64
	sealed         bool
}

func newRunState() *runState { return &runState{core: coreagent.NewRunState()} }

// recordProviderFailure records the sequence at which an authoritative
// main-session provider failure was observed (only the first is kept). It is
// not a local termination cause.
func (s *runState) recordProviderFailure() {
	s.core.RecordProviderFailure()
}

// recordCause accepts a cause only when the transition is legal and the run
// has not been sealed. It returns false when the request must be ignored (for
// example a late Stop after the process already completed).
func (s *runState) recordCause(c runCause) bool {
	return s.core.RecordCause(coreagent.Cause(c))
}

// observeError records the sequence at which an authoritative stdout
// `type:"error"` event was first seen. Only the first observation is kept so
// the error-before-cause ordering stays stable.
func (s *runState) observeError() {
	s.core.RecordError()
}

// nextSeq allocates the next observation sequence without recording any
// semantic. Used to capture the chronological position of a candidate error
// whose session is only resolved after the stream ends.
func (s *runState) nextSeq() uint64 {
	return s.core.NextSequence()
}

// recordErrorAt promotes a candidate error captured earlier to the recorded
// error sequence, preserving its chronological position relative to causes.
func (s *runState) recordErrorAt(seq uint64) {
	s.core.RecordErrorAt(seq)
}

// recordStopAt promotes a candidate valid-completion (step_finish "stop")
// observation captured earlier to the recorded stop sequence, preserving its
// chronological position relative to errors.
func (s *runState) recordStopAt(seq uint64) {
	s.core.RecordStopAt(seq)
}

// recordProviderFailureAt promotes a candidate provider failure captured
// earlier to the recorded provider sequence, preserving chronological order.
func (s *runState) recordProviderFailureAt(seq uint64) {
	s.core.RecordProviderFailureAt(seq)
}

// seal closes the machine so late requests can no longer rewrite history.
func (s *runState) seal() {
	s.core.Seal()
}

func (s *runState) snapshot() runStateSnapshot {
	snapshot := s.core.Snapshot()
	return runStateSnapshot{cause: runCause(snapshot.Cause), errSeq: snapshot.ErrorSequence, causeSeq: snapshot.CauseSequence, stopSeq: snapshot.StopSequence, providerErrSeq: snapshot.ProviderErrorSequence, sealed: snapshot.Sealed}
}

// exitStatus describes the raw process termination facts. Exit code and signal
// are retained independently so classification never parses waitErr text.
type exitStatus struct {
	exited   bool
	exitCode int
	signaled bool
	signal   string
}

// classifyRun applies the authoritative outcome precedence. The first matching
// rule wins; a nonzero process exit always remains evidence of a problem.
//
// Error and completion evidence share one observation sequence (runState.seq):
// a main-session error observed before valid completion (or with no valid
// completion at all) is a genuine failure, while an error observed after the
// main-session step_finish is post-completion evidence and never fails the
// produced answer.
//
// Chronological precedence for provider failures: an authoritative main-session
// provider error observed before an accepted local termination cause produces
// failed with the provider cause. A local stop/request cancellation/output
// limit/shutdown accepted before the provider error keeps its local outcome.
func classifyRun(state runStateSnapshot, stdoutError bool, validStop bool, exit exitStatus, providerCause runCause) runOutcome {
	return runOutcome(coreagent.Classify(coreagent.Snapshot{Cause: coreagent.Cause(state.cause), ErrorSequence: state.errSeq, CauseSequence: state.causeSeq, StopSequence: state.stopSeq, ProviderErrorSequence: state.providerErrSeq, Sealed: state.sealed}, stdoutError, validStop, coreagent.ExitStatus{Exited: exit.exited, ExitCode: exit.exitCode, Signaled: exit.signaled, Signal: exit.signal}, coreagent.Cause(providerCause)))
}

// classifyProviderError recognizes a provider insufficient-balance failure
// from structured provider evidence. The rules are intentionally narrow to
// avoid misclassifying rate limits, configured spending limits, auth errors,
// quota text, or unrelated provider failures.
//
// Evidence is accepted only when it is an authoritative provider error (a
// main-session stdout `type:"error"` event or a main-session structured stderr
// ERROR record), never assistant prose. Recognition matches:
//   - an explicit insufficient-balance/provider-credit code when the provider
//     serializes one (e.g. a structured "insufficient_balance" code),
//   - a structured provider error with numeric HTTP status 402 when that field
//     survives into the serialized event,
//   - the exact case-insensitive provider phrase "insufficient balance",
//   - explicitly proven equivalent phrases listed in the table.
//
// A bare "402", "payment required", "quota", "limit", "credit", or "billing"
// mention alone is not sufficient.
func classifyProviderError(msg, code string, statusCode int) (providerInsufficientBalance bool) {
	return coreagent.ClassifyProviderError(msg, code, statusCode)
}

// sanitizeBillingURL validates a provider-supplied billing URL strictly. It
// accepts only https, the exact normalized hostname "opencode.ai", no user
// information, no explicit port, and a normal absolute URL. The real
// /workspace/... path is preserved. Returns "" for anything else.
func sanitizeBillingURL(raw string) string {
	return coreagent.SanitizeBillingURL(raw)
}

// tailCapture drains an io.Reader to EOF in the background while retaining
// only the last limit bytes. This prevents a full stderr pipe from blocking
// the child and keeps the final, most relevant, error lines. Reading always
// continues to EOF so the pipe never fills.
type tailCapture struct {
	mu        sync.Mutex
	buf       []byte
	discarded int64
	limit     int
	done      chan struct{}
}

func captureTail(r io.Reader, limit int) *tailCapture {
	c := &tailCapture{limit: limit, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		tmp := make([]byte, 32<<10)
		for {
			n, err := r.Read(tmp)
			if n > 0 {
				c.write(tmp[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return c
}

func (c *tailCapture) write(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(b) == 0 {
		return
	}
	if len(b) >= c.limit {
		c.discarded += int64(len(c.buf)) + int64(len(b)-c.limit)
		c.buf = append([]byte(nil), b[len(b)-c.limit:]...)
		return
	}
	need := c.limit - len(c.buf)
	if len(b) > need {
		drop := len(b) - need
		c.discarded += int64(drop)
		c.buf = c.buf[drop:]
	}
	c.buf = append(c.buf, b...)
}

// wait blocks until the underlying reader has reached EOF.
func (c *tailCapture) wait() { <-c.done }

// String returns the retained tail.
func (c *tailCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}

// truncated reports whether bytes were discarded because the source exceeded
// the capture bound.
func (c *tailCapture) truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.discarded > 0
}

// stderrLevel extracts the level field from a structured stderr line. It
// returns the empty string for unstructured lines, which callers must tolerate
// rather than fail classification on.
func stderrLevel(line string) string {
	const marker = "level="
	i := strings.Index(line, marker)
	if i < 0 {
		return ""
	}
	rest := line[i+len(marker):]
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		rest = rest[:j]
	}
	if j := strings.IndexByte(rest, '\t'); j >= 0 {
		rest = rest[:j]
	}
	level := strings.ToUpper(rest)
	switch level {
	case "DEBUG", "INFO", "WARN", "ERROR":
		return level
	}
	return ""
}

// diagnostics is the structured outcome record persisted for a run and served
// through the owner-authorized technical-details endpoint. All text fields are
// redacted and bounded before storage.
type diagnostics struct {
	Outcome              string   `json:"outcome"`
	Category             string   `json:"category"`
	Summary              string   `json:"summary"`
	ExitCode             int      `json:"exitCode,omitempty"`
	Signal               string   `json:"signal,omitempty"`
	Cause                string   `json:"cause,omitempty"`
	StdoutError          string   `json:"stdoutError,omitempty"`
	Errors               []string `json:"errors,omitempty"`
	Warnings             []string `json:"warnings,omitempty"`
	StderrTail           string   `json:"stderrTail,omitempty"`
	StderrTruncated      bool     `json:"stderrTruncated,omitempty"`
	RecoveryAttempted    bool     `json:"recoveryAttempted,omitempty"`
	RecoveryResult       string   `json:"recoveryResult,omitempty"`
	TerminalEventDeliver bool     `json:"terminalEventDelivered,omitempty"`
	DeliveryError        string   `json:"deliveryError,omitempty"`
	OpenCodeVersion      string   `json:"opencodeVersion,omitempty"`
	Provider             string   `json:"provider,omitempty"`
	Model                string   `json:"model,omitempty"`
	ProviderCause        string   `json:"providerCause,omitempty"`
	BillingURL           string   `json:"billingUrl,omitempty"`
}
