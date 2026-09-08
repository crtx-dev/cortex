package app

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	coreagent "github.com/gantry-dev/gantry-core/agent"
)

const (
	maxProviderLine   = 1 << 20
	maxProviderBytes  = 32 << 20
	maxProviderEvents = 4096
	maxImageCount     = 6
	maxImageBytes     = 10 << 20
	maxRunBytes       = 96 << 20
)

// sessionEvidence collects per-session activity, terminal, and error evidence
// during the stdout stream so the authoritative main session can be resolved
// after the loop rather than assuming the first session seen is main.
type sessionEvidence struct {
	firstSeen   uint64
	hasActivity bool
	lastReason  string
	stopObs     uint64
	// stopSeq captures the valid-completion observation in the same runState
	// sequence used by firstErrSeq, so the error-vs-completion ordering is
	// comparable after the main session is resolved.
	stopSeq       uint64
	firstErrSeq   uint64
	errMsg        string
	errCode       string
	errStatus     int
	errBillingURL string
}

type agentRunRequest struct {
	Workspace     string        `json:"workspace"`
	Prompt        string        `json:"prompt"`
	Session       string        `json:"session,omitempty"`
	ClientSession string        `json:"clientSession,omitempty"`
	Images        []imageUpload `json:"images,omitempty"`
}

type imageUpload struct {
	Name string `json:"name"`
	Data string `json:"data"`
}

func (a *App) agentStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method", 405)
		return
	}
	a.mu.RLock()
	providerID := a.settings.ActiveProvider
	a.mu.RUnlock()
	p, ok := providerByID(providerID)
	if !ok {
		http.Error(w, "unknown provider", 500)
		return
	}
	configured := false
	if p.AuthMode == "key" {
		_, configured = a.credential(providerID)
	} else {
		configured = a.hostOpenCodeAuthConfigured(p.OpenCodeID)
	}
	model := a.configuredModel(providerID)
	_, err := exec.LookPath("opencode")
	note := ""
	if p.AuthMode == "opencode-auth" {
		note = "Uses OpenCode's existing OAuth credential on the Cortex host. Run `opencode auth login --provider " + p.OpenCodeID + "` once if it is not configured."
	}
	jsonOut(w, map[string]any{
		"available":           err == nil && configured && model != "",
		"opencodeInstalled":   err == nil,
		"credentialAvailable": configured,
		"provider":            providerID,
		"providerLabel":       p.Label,
		"model":               modelRefFor(p, model),
		"authMode":            p.AuthMode,
		"note":                note,
	})
}

func (a *App) agentRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	var q agentRunRequest
	if !decodeJSON(w, r, &q, maxRunBytes) {
		return
	}
	q.Prompt = strings.TrimSpace(q.Prompt)
	if q.Prompt == "" {
		http.Error(w, "prompt required", 400)
		return
	}
	if len(q.Images) > maxImageCount {
		http.Error(w, "too many images", http.StatusRequestEntityTooLarge)
		return
	}
	select {
	case a.runSlots <- struct{}{}:
		defer func() { <-a.runSlots }()
	default:
		http.Error(w, "agent concurrency limit reached", http.StatusTooManyRequests)
		return
	}
	workspace, err := a.resolve(q.Workspace)
	if err != nil {
		http.Error(w, "invalid workspace: "+err.Error(), 400)
		return
	}
	st, err := os.Stat(workspace)
	if err != nil || !st.IsDir() {
		http.Error(w, "workspace must be a directory", 400)
		return
	}
	a.mu.RLock()
	providerID := a.settings.ActiveProvider
	a.mu.RUnlock()
	provider, ok := providerByID(providerID)
	if !ok {
		http.Error(w, "unknown provider", 400)
		return
	}
	modelID := a.configuredModel(providerID)
	if modelID == "" {
		http.Error(w, "model is not configured for "+provider.Label, 400)
		return
	}
	key := ""
	if provider.AuthMode == "key" {
		var configured bool
		key, configured = a.credential(providerID)
		if !configured {
			http.Error(w, provider.Label+" credential is not configured", 400)
			return
		}
	} else if !a.hostOpenCodeAuthConfigured(provider.OpenCodeID) {
		http.Error(w, provider.Label+" is not connected in OpenCode; run `opencode auth login --provider "+provider.OpenCodeID+"` on the Cortex host", 400)
		return
	}
	modelRef := modelRefFor(provider, modelID)
	binary, err := exec.LookPath("opencode")
	if err != nil {
		http.Error(w, "OpenCode is not installed or not in Cortex's PATH", 503)
		return
	}
	clientSession := strings.TrimSpace(q.ClientSession)
	if clientSession == "" {
		clientSession = "default"
	}
	if !validRecordID(clientSession) {
		http.Error(w, "invalid client session", 400)
		return
	}
	runDir, err := os.MkdirTemp("", "cortex-opencode-config-")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer os.RemoveAll(runDir)
	configDir := filepath.Join(runDir, "config")
	dataDir := filepath.Join(a.dataDir, "sessions", clientSession, "data")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if provider.AuthMode == "opencode-auth" {
		if err := copyOpenCodeAuth(dataDir); err != nil {
			http.Error(w, "copy OpenCode auth: "+err.Error(), 500)
			return
		}
	}
	cfg, err := cortexOpenCodeConfig(provider, modelID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	configPath := filepath.Join(configDir, "opencode.json")
	if err := os.WriteFile(configPath, cfg, 0600); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	runID := randomToken(18)
	run := newActiveRun(cancel)
	defer run.finished()

	// Durable run creation happens before the process starts so an untracked
	// OpenCode process is never launched if persistence fails. Once the
	// durable row exists, every later failure finalizes it to a truthful state.
	// A workspace-less run resolves to the configured Cortex root for
	// execution, but the conversation keeps the explicit empty "use default
	// root" state verbatim so the browser and SQLite agree that no specific
	// workspace was selected.
	storedWorkspace := workspace
	if strings.TrimSpace(q.Workspace) == "" {
		storedWorkspace = ""
	}
	if err := a.startAgentRun(runID, clientSession, q.Prompt, storedWorkspace, providerID, modelID); err != nil {
		if strings.Contains(err.Error(), "already running") {
			http.Error(w, "agent is already running for this conversation", http.StatusConflict)
		} else {
			http.Error(w, "persist agent run: "+err.Error(), 500)
		}
		return
	}
	// The user prompt is persisted immediately after the durable run, at
	// sequence zero, before the process starts. Provider events begin at one,
	// so a restored transcript always shows the prompt first.
	seq := int64(0)
	if err := a.persistAgentRunEvent(runID, "user", q.Prompt, "", seq, time.Now().UnixMilli()); err != nil {
		a.finishRunChecked(runID, clientSession, "failed", "", "persist user prompt: "+err.Error(), 0, 0, 0, "")
		http.Error(w, "persist user prompt: "+err.Error(), 500)
		return
	}
	seq++
	// Register the active run before cmd.Start so request cancellation or
	// service shutdown between start and registry insertion is never an
	// unclassified signal. The run ID is not exposed to the browser until the
	// process actually started.
	a.runMu.Lock()
	a.activeRuns[runID] = run
	a.runMu.Unlock()
	defer func() { a.runMu.Lock(); delete(a.activeRuns, runID); a.runMu.Unlock() }()

	imageFiles := []string{}
	imageDir := ""
	if len(q.Images) > 0 {
		var err error
		imageDir, imageFiles, err = writeImageAttachments(q.Images)
		if err != nil {
			run.state.recordCause(causeRequestCanceled)
			a.finishRunChecked(runID, clientSession, "failed", "", "invalid image: "+err.Error(), 0, 0, 0, "")
			http.Error(w, "invalid image: "+err.Error(), 400)
			return
		}
		defer os.RemoveAll(imageDir)
	}
	args := agentRunArgs(workspace, modelRef, q.Session, imageFiles, q.Prompt)
	cmd := exec.CommandContext(ctx, binary, args...)
	configureProcessGroup(cmd)
	cmd.Dir = workspace
	env := append([]string{}, os.Environ()...)
	if provider.AuthMode == "key" {
		env = setEnv(env, "CORTEX_PROVIDER_API_KEY", key)
	}
	env = ghConfigDirEnv(env)
	env = setEnv(env, "OPENCODE_CONFIG", configPath)
	env = setEnv(env, "OPENCODE_CONFIG_DIR", configDir)
	env = setEnv(env, "XDG_CONFIG_HOME", configDir)
	env = setEnv(env, "XDG_DATA_HOME", dataDir)
	env = setEnv(env, "OPENCODE_DISABLE_AUTOUPDATE", "1")
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		run.state.recordCause(causeRequestCanceled)
		a.finishRunChecked(runID, clientSession, "failed", "", "stdout pipe: "+err.Error(), 0, 0, 0, "")
		http.Error(w, err.Error(), 500)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		run.state.recordCause(causeRequestCanceled)
		a.finishRunChecked(runID, clientSession, "failed", "", "stderr pipe: "+err.Error(), 0, 0, 0, "")
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	flusher, ok := w.(http.Flusher)
	if !ok {
		run.state.recordCause(causeRequestCanceled)
		a.finishRunChecked(runID, clientSession, "failed", "", "streaming unavailable", 0, 0, 0, "")
		http.Error(w, "streaming unavailable", 500)
		return
	}
	if err := cmd.Start(); err != nil {
		run.state.seal()
		diag, summary := a.runDiagnostics(outcomeFailed, run.state.snapshot(), "", false, processExitStatus(cmd), nil, nil, "", false, 0, true, nil, map[string]string{"result": "not_attempted"}, causeNone, "", providerID, modelID)
		a.finishRunChecked(runID, clientSession, "failed", "", summary, 0, 0, 0, diag)
		_ = writeEvent(w, flusher, "error", map[string]any{"message": summary, "outcome": string(outcomeFailed)})
		return
	}

	// Start succeeded: expose the run ID before any assistant event.
	var deliveryErr error
	if err := writeEvent(w, flusher, "run", map[string]any{"runID": runID}); err != nil {
		deliveryErr = err
	}

	// Record request/browser disconnection as a cancellation cause. It is
	// only accepted while no stronger cause exists and the run is not sealed.
	// Guard against a nil Done() channel (e.g. background contexts in tests or
	// internal callers), where receiving would block forever.
	if requestDone := r.Context().Done(); requestDone != nil {
		go func() {
			<-requestDone
			run.state.recordCause(causeRequestCanceled)
			cancel()
		}()
	}

	tail := captureTail(stderr, 64<<10)
	var input, output uint64
	var cost float64
	sessionID := ""
	// Per-session evidence is collected during the stream and resolved after
	// the loop: the authoritative main session is the one with the latest
	// terminal stop evidence, else the most active, else the first error's
	// session (a prompt-start failure). Error and terminal evidence for the
	// chosen session are then promoted chronologically.
	sessions := map[string]*sessionEvidence{}
	// streamedAssistant accumulates the assistant text durably persisted for
	// the run, used by recovery reconciliation to avoid duplicating content.
	streamedAssistant := ""
	validStop := false
	persistFailed := false
	var persistErr string
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 64<<10), maxProviderLine)
	streamBytes, streamEvents := 0, 0
	var stdoutScanErr error
	truncated := false
	obs := uint64(0)
	for scan.Scan() {
		line := append([]byte(nil), scan.Bytes()...)
		streamBytes += len(line)
		streamEvents++
		if streamBytes > maxProviderBytes || streamEvents > maxProviderEvents {
			truncated = true
			run.state.recordCause(causeOutputLimit)
			cancel()
			break
		}
		var raw map[string]any
		if json.Unmarshal(line, &raw) == nil {
			obs++
			collectUsage(raw, &input, &output, &cost)
			id, _ := raw["sessionID"].(string)
			if id != "" {
				sessionID = id
			}
			typ, _ := raw["type"].(string)
			// Activity events (steps, text, tools) mark a session as producing
			// output; step_finish records terminal evidence per session.
			if typ == "step_start" || typ == "step_finish" || typ == "text" || typ == "tool_use" || typ == "reasoning" {
				if id != "" {
					ev := sessions[id]
					if ev == nil {
						ev = &sessionEvidence{firstSeen: obs}
						sessions[id] = ev
					}
					ev.hasActivity = true
					if typ == "step_finish" {
						if reason, _ := raw["part"].(map[string]any)["reason"].(string); reason != "" {
							ev.lastReason = reason
							// stopObs/stopSeq are genuine completion evidence only
							// for reason=="stop": non-terminal step_finish reasons
							// must never be treated as valid completion by the
							// error-ordering classifier.
							if reason == "stop" {
								ev.stopObs = obs
								// Valid-completion evidence is captured in the same
								// observation sequence as errors so the classifier
								// can order error-before-stop vs error-after-stop.
								ev.stopSeq = run.state.nextSeq()
							}
						}
					}
				}
			}
			// Candidate authoritative error per session: capture the first
			// error for each session with its chronological sequence, but do
			// not resolve until the main session is confirmed.
			if typ == "error" && id != "" {
				ev := sessions[id]
				if ev == nil {
					ev = &sessionEvidence{firstSeen: obs}
					sessions[id] = ev
				}
				if ev.firstErrSeq == 0 {
					ev.firstErrSeq = run.state.nextSeq()
					ev.errMsg = a.redactSecrets(errorEventText(raw))
					ev.errCode, ev.errStatus = providerErrorMeta(raw)
					ev.errBillingURL = sanitizeBillingURL(extractBillingURL(ev.errMsg))
				}
			}
			// Persist normalized server-owned events before delivery so a
			// disconnect cannot lose the transcript. Error events are excluded
			// here: their session is resolved only after the stream ends, and a
			// post-completion error must never survive as a raw Error block.
			kind, text, name := normalizedEvent(raw)
			if kind != "" && kind != "error" {
				// Redact configured credentials from every normalized event text
				// (assistant, tool incl. todowrite output/errors, file) before
				// persistence so the durable transcript never bypasses output
				// sanitization.
				text = a.redactSecrets(text)
				if err := a.persistAgentRunEvent(runID, kind, text, name, seq, time.Now().UnixMilli()); err != nil {
					persistFailed = true
					persistErr = err.Error()
					cancel()
					break
				}
				if kind == "assistant" {
					streamedAssistant = strings.TrimSpace(streamedAssistant + "\n" + text)
				}
				seq++
			}
			// A valid todowrite snapshot is persisted as its own authoritative
			// task event so the task panel restores after reload, and streamed
			// as a normalized server-owned task event so the live panel opens
			// without waiting for terminal completion.
			if tasks := a.taskSnapshot(raw); tasks != "" {
				if err := a.persistAgentRunEvent(runID, "task", tasks, "", seq, time.Now().UnixMilli()); err != nil {
					persistFailed = true
					persistErr = err.Error()
					cancel()
					break
				}
				seq++
				if err := writeEvent(w, flusher, "task", map[string]any{"snapshot": tasks, "sessionID": sessionID}); err != nil {
					deliveryErr = err
				}
			}
			rewriteImageURLs(raw, clientSession)
			// The live forwarded event obeys the same secret-redaction contract
			// as the durable transcript, so a completed todowrite carrying the
			// secret in state.input/state.output/errors never reaches the
			// browser raw.
			if err := writeEvent(w, flusher, "opencode", a.sanitizeAgentEvent(raw, 0)); err != nil {
				deliveryErr = err
			}
		} else {
			lineText := strings.TrimSpace(string(line))
			if lineText != "" {
				if err := a.persistAgentRunEvent(runID, "assistant", lineText, "", seq, time.Now().UnixMilli()); err != nil {
					persistFailed = true
					persistErr = err.Error()
					cancel()
					break
				}
				streamedAssistant = strings.TrimSpace(streamedAssistant + "\n" + lineText)
				seq++
			}
			if err := writeEvent(w, flusher, "output", map[string]any{"text": string(line)}); err != nil {
				deliveryErr = err
			}
		}
	}
	// Resolve the target main session from per-session evidence. OpenCode's
	// `run --format json` emits only the target main session's events on
	// stdout, so in practice all events belong to one session. Defensively, we
	// choose the session with the latest terminal stop evidence; failing that,
	// the most recently active session; failing that, the session of the first
	// error (a prompt-start failure).
	mainSessionID := ""
	var mainEv *sessionEvidence
	for id, ev := range sessions {
		if ev == nil {
			continue
		}
		// Prefer the session with the latest terminal stop.
		if ev.lastReason == "stop" {
			if mainEv == nil || ev.stopObs > mainEv.stopObs {
				mainSessionID, mainEv = id, ev
			}
			continue
		}
		if mainEv == nil {
			mainSessionID, mainEv = id, ev
		} else if mainEv.lastReason != "stop" && ev.hasActivity && ev.firstSeen > mainEv.firstSeen {
			mainSessionID, mainEv = id, ev
		}
	}
	// Promote the main session's error evidence (first error, chronological).
	var stdoutErrMsg string
	stdoutErr := false
	var providerCause runCause
	var billingURL string
	postCompletionErr := false
	if mainEv != nil {
		if mainEv.stopSeq != 0 {
			run.state.recordStopAt(mainEv.stopSeq)
		}
		if mainEv.firstErrSeq != 0 {
			stdoutErr = true
			run.state.recordErrorAt(mainEv.firstErrSeq)
			stdoutErrMsg = mainEv.errMsg
			if classifyProviderError(mainEv.errMsg, mainEv.errCode, mainEv.errStatus) {
				providerCause = causeProviderInsufficientBalance
				run.state.recordProviderFailureAt(mainEv.firstErrSeq)
				billingURL = mainEv.errBillingURL
			}
			// An error observed after the main session's step_finish is
			// post-completion evidence: it is not failure of the answer.
			postCompletionErr = mainEv.stopSeq != 0 && mainEv.firstErrSeq > mainEv.stopSeq
		}
	}
	if mainEv != nil {
		validStop = mainEv.lastReason == "stop"
	}
	// The durable session identifier for recovery, persistence and the
	// terminal payload is always the target main session.
	sessionID = mainSessionID
	if err := scan.Err(); err != nil {
		stdoutScanErr = err
		if !truncated && !persistFailed {
			cancel()
		}
	}
	waitErr := cmd.Wait()
	// Seal the cause machine before any late disconnect/Stop can rewrite it.
	run.state.seal()
	tail.wait()
	stderrText := tail.String()
	stderrTrunc := tail.truncated()
	if stdoutScanErr != nil && waitErr == nil {
		waitErr = stdoutScanErr
	}
	exit := processExitStatus(cmd)
	snap := run.state.snapshot()
	outcome := classifyRun(snap, stdoutErr, validStop, exit, providerCause)
	if persistFailed {
		// A persistence failure is a storage/run failure: the authoritative
		// transcript is incomplete, so the run cannot claim completion.
		outcome = outcomeFailed
		stdoutErrMsg = "agent transcript persistence failed: " + persistErr
	}
	// Persist a pre-completion main-session error as a transcript error event.
	// A post-completion error is retained only as a diagnostic warning and must
	// not survive as a separate raw Error block beside the final warning.
	if stdoutErr && !postCompletionErr && stdoutErrMsg != "" {
		if err := a.persistAgentRunEvent(runID, "error", stdoutErrMsg, "", seq, time.Now().UnixMilli()); err != nil {
			persistFailed = true
			persistErr = err.Error()
			if !truncated {
				cancel()
			}
			outcome = outcomeFailed
			stdoutErrMsg = "agent transcript persistence failed: " + persistErr
		} else {
			seq++
		}
	}

	// Recovery reconciles with already-persisted assistant content so the
	// complete answer is not appended twice. It only runs when the outcome is
	// a genuine failure, no streamed/persisted assistant text already covers
	// the final response, and no known local cause stopped the run.
	recoveryResult := ""
	if outcome == outcomeFailed || outcome == outcomeCompletedWError {
		if !persistFailed && !truncated && snap.cause != causeRequestCanceled && snap.cause != causeUserStop && snap.cause != causeOutputLimit {
			if sessionID != "" {
				recCtx, recCancel := context.WithTimeout(context.Background(), 30*time.Second)
				recovered, images, ri, ro, rc, recErr := recoverSession(recCtx, binary, sessionID, workspace, clientSession, env)
				recCancel()
				if recErr != nil {
					recoveryResult = "failed"
				} else {
					recovered = strings.TrimSpace(recovered)
					// Reconcile against the assistant text already durably
					// streamed for this run: only the missing portion is
					// persisted, and recovery is suppressed when the complete
					// response is already present. Replacement cases (the
					// streamed fragment is only a suffix of the recovered
					// response) persist the full response under a
					// replacement marker so the merged transcript shows the
					// correct final answer rather than a misordered append.
					text, suppressed, replace := reconcileRecovered(streamedAssistant, recovered)
					recoveryName := "recovered"
					if replace {
						recoveryName = "repl:" + runID
					}
					if suppressed {
						recoveryResult = "ok_suppressed"
					} else if text == "" {
						recoveryResult = "ok_empty"
					} else {
						if err := a.persistAgentRunEvent(runID, "assistant", text, recoveryName, seq, time.Now().UnixMilli()); err != nil {
							recoveryResult = "persist_failed"
						} else {
							seq++
							recoveryResult = "ok"
							if err := writeEvent(w, flusher, "recovered", map[string]any{"text": text, "sessionID": sessionID, "recovered": true}); err != nil {
								deliveryErr = err
							}
						}
					}
					for _, im := range images {
						url := sanitizeImageURL(im["url"])
						if url == "" {
							continue
						}
						if err := a.persistAgentRunEvent(runID, "image", url, im["name"], seq, time.Now().UnixMilli()); err != nil {
							recoveryResult = "persist_failed"
							break
						}
						seq++
					}
					if ri > input {
						input = ri
					}
					if ro > output {
						output = ro
					}
					if rc > cost {
						cost = rc
					}
					if len(images) > 0 && recoveryResult == "ok" {
						if err := writeEvent(w, flusher, "recovered-images", map[string]any{"images": images, "sessionID": sessionID}); err != nil {
							deliveryErr = err
						}
					}
				}
			} else {
				recoveryResult = "no_session"
			}
		}
	}

	// Determine and persist the execution outcome before terminal delivery.
	stderrErrors, stderrWarnings := parsedStderr(a.redactSecrets(stderrText))
	diag, summary := a.runDiagnostics(outcome, snap, stdoutErrMsg, validStop, exit, stderrErrors, stderrWarnings, a.redactSecrets(stderrText), stderrTrunc, seq, false, stdoutScanErr, map[string]string{"result": recoveryResult}, providerCause, billingURL, providerID, modelID)
	// The terminal event carries the run identity and outcome so technical
	// details survive reload; the name field encodes them as
	// "run:<runID>:<outcome>". Persistence is checked: a storage failure must
	// never be represented as a durable completion.
	if err := a.persistAgentRunEvent(runID, terminalKind(outcome), summary, "run:"+runID+":"+string(outcome), seq, time.Now().UnixMilli()); err != nil {
		a.storageFailure(runID, clientSession, sessionID, "terminal event", err, input, output, cost, w, flusher, seq)
		return
	}
	seq++
	if err := a.finishAgentRun(runID, clientSession, string(outcome), sessionID, summary, input, output, cost, diag); err != nil {
		a.storageFailure(runID, clientSession, sessionID, "run-state", err, input, output, cost, w, flusher, seq)
		return
	}

	// Terminal delivery is attempted only when the stream is still writable.
	// The delivery outcome is captured from the actual write result.
	terminalDelivered := false
	var terminalDeliveryErr error
	if deliveryErr == nil {
		var payload map[string]any
		switch outcome {
		case outcomeCompleted:
			payload = map[string]any{"inputTokens": input, "outputTokens": output, "estimatedCostUsd": cost, "sessionID": sessionID, "outcome": string(outcomeCompleted)}
		case outcomeCompletedWError:
			payload = map[string]any{"message": summary, "exitCode": exit.exitCode, "signal": exit.signal, "outcome": string(outcomeCompletedWError)}
		case outcomeFailed:
			payload = map[string]any{"message": summary, "exitCode": exit.exitCode, "signal": exit.signal, "outcome": string(outcomeFailed), "runId": runID}
			if providerCause == causeProviderInsufficientBalance {
				payload["cause"] = string(causeProviderInsufficientBalance)
				payload["billingUrl"] = billingURL
			}
		case outcomeCancelled:
			payload = map[string]any{"message": "Agent stopped.", "outcome": string(outcomeCancelled)}
		case outcomeTruncated:
			payload = map[string]any{"message": "Provider output limit reached; the run was stopped.", "outcome": string(outcomeTruncated)}
		case outcomeInterrupted:
			payload = map[string]any{"message": "The agent was interrupted.", "outcome": string(outcomeInterrupted)}
		}
		var termErr error
		switch outcome {
		case outcomeCompleted:
			termErr = writeEvent(w, flusher, "done", payload)
		case outcomeCompletedWError:
			termErr = writeEvent(w, flusher, "warning", payload)
		case outcomeFailed:
			termErr = writeEvent(w, flusher, "error", payload)
		case outcomeCancelled:
			termErr = writeEvent(w, flusher, "cancelled", payload)
		case outcomeTruncated:
			termErr = writeEvent(w, flusher, "truncated", payload)
		case outcomeInterrupted:
			termErr = writeEvent(w, flusher, "cancelled", payload)
		}
		if termErr == nil {
			terminalDelivered = true
		} else {
			terminalDeliveryErr = termErr
		}
	} else {
		terminalDeliveryErr = deliveryErr
	}

	// Delivery diagnostics are recorded after the attempt, never before. A
	// delivery failure does not change the underlying execution outcome.
	diag2, summary2 := a.runDiagnostics(outcome, snap, stdoutErrMsg, validStop, exit, stderrErrors, stderrWarnings, a.redactSecrets(stderrText), stderrTrunc, seq, terminalDelivered, stdoutScanErr, map[string]string{"result": recoveryResult}, providerCause, billingURL, providerID, modelID)
	if terminalDeliveryErr != nil {
		var d diagnostics
		_ = json.Unmarshal([]byte(diag2), &d)
		d.DeliveryError = a.redactSecrets(terminalDeliveryErr.Error())
		d.TerminalEventDeliver = false
		if b, err := json.Marshal(d); err == nil {
			diag2 = string(b)
		}
	}
	// Persist the updated diagnostics with the delivery outcome. This final
	// run-state update is checked; a failure is logged and the run is left in
	// the storage-failure outcome rather than a durable completion.
	if err := a.finishAgentRun(runID, clientSession, string(outcome), sessionID, summary2, input, output, cost, diag2); err != nil {
		a.storageFailure(runID, clientSession, sessionID, "post-delivery run-state", err, input, output, cost, w, flusher, seq)
	}
}

// storageFailure handles a terminal persistence failure: it logs at ERROR with
// the run ID, best-effort updates the durable run to a storage-failure outcome,
// and emits a bounded client-facing failure if the stream is still writable.
// The original error is never suppressed.
func (a *App) storageFailure(runID, clientSession, sessionID, stage string, err error, input, output uint64, cost float64, w http.ResponseWriter, f http.Flusher, seq int64) {
	log.Printf("cortex agent run %s: %s persistence failed: %v", runID, stage, err)
	summary := "The agent result could not be stored durably."
	d := diagnostics{
		Outcome:       string(outcomeFailed),
		Category:      "storage_failure",
		Summary:       summary,
		DeliveryError: stage + ": persistence failed: " + err.Error(),
	}
	diag, _ := json.Marshal(d)
	// Terminal-event supersession: append a sequenced storage-failure terminal
	// marker (name "run:<id>:failed") so the merged transcript/UI treats it as
	// the authoritative final outcome, superseding any earlier durable marker.
	// The run row and diagnostics remain the fallback authoritative source if
	// this insert also fails.
	if err := a.persistAgentRunEvent(runID, "error", summary, "run:"+runID+":failed", seq+1, time.Now().UnixMilli()); err != nil {
		log.Printf("cortex agent run %s: storage-failure terminal marker insert failed: %v", runID, err)
	}
	if ferr := a.finishAgentRun(runID, clientSession, string(outcomeFailed), sessionID, summary, input, output, cost, string(diag)); ferr != nil {
		log.Printf("cortex agent run %s: storage-failure finalization also failed: %v", runID, ferr)
	}
	if w != nil && f != nil {
		_ = writeEvent(w, f, "error", map[string]any{"message": summary, "outcome": string(outcomeFailed), "runId": runID})
	}
}

// reconcileRecovered compares recovered assistant text against the assistant
// text already durably streamed for the run, returning the text to persist, a
// suppress flag (the complete response is already present), and whether the
// persisted recovery event must be a full-response replacement rather than an
// appended tail.
//
// Policy:
//   - recovered identical/contained in streamed -> suppress
//   - recovered starts with streamed -> append the missing suffix
//   - partial overlap where recovered continues streamed (a suffix of streamed
//     is a prefix of recovered) -> append the genuinely new tail
//   - streamed is only a suffix of recovered -> the missing content is a
//     prefix that cannot be appended after the existing suffix; persist the
//     full recovered response as a replacement/supersession event
//   - no overlap -> persist the full recovered response (genuinely new)
//   - empty recovered -> suppress
func reconcileRecovered(streamed, recovered string) (text string, suppressed, replace bool) {
	return coreagent.ReconcileRecovered(streamed, recovered)
}

// sanitizeImageURL validates or rewrites an unsafe file URL before persistence
// so stored image events only reference safe, servable paths.
func sanitizeImageURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	if strings.HasPrefix(u, "data:") {
		if len(u) > 10<<20 {
			return ""
		}
		return u
	}
	if !strings.HasPrefix(u, "file://") {
		return ""
	}
	p := fileURLPath(u)
	if p == "" {
		return ""
	}
	return "file://" + p
}

// finishRunChecked finalizes an early-failed run and logs at ERROR if that
// finalization itself fails, so a storage failure is never silently dropped.
func (a *App) finishRunChecked(id, conversationID, state, sessionID, message string, input, output uint64, cost float64, diag string) {
	if err := a.finishAgentRun(id, conversationID, state, sessionID, message, input, output, cost, diag); err != nil {
		log.Printf("cortex agent run %s: finalize failed: %v", id, err)
	}
}

// finishAgentRunErr is the error-returning form of finishAgentRun used when
// the run fails before the streaming tail exists.
func (a *App) finishAgentRunErr(id, conversationID, state, sessionID, message string, input, output uint64, cost float64, diag string) error {
	return a.finishAgentRun(id, conversationID, state, sessionID, message, input, output, cost, diag)
}

// runDiagnostics builds the bounded, redacted diagnostic record and the
// user-facing summary for a run outcome.
func (a *App) runDiagnostics(outcome runOutcome, snap runStateSnapshot, stdoutErrMsg string, validStop bool, exit exitStatus, stderrErrors, stderrWarnings []string, stderrTail string, stderrTrunc bool, seq int64, delivered bool, scannerErr error, recovery map[string]string, providerCause runCause, billingURL, provider, model string) (string, string) {
	d := diagnostics{
		Outcome:              string(outcome),
		ExitCode:             exit.exitCode,
		Signal:               exit.signal,
		Cause:                string(snap.cause),
		StdoutError:          stdoutErrMsg,
		Errors:               boundedStrings(stderrErrors, 8),
		Warnings:             boundedStrings(stderrWarnings, 8),
		StderrTail:           boundedTail(stderrTail, 8<<10),
		StderrTruncated:      stderrTrunc,
		TerminalEventDeliver: delivered,
		OpenCodeVersion:      openCodeVersion(),
		Provider:             provider,
		Model:                model,
		ProviderCause:        string(providerCause),
		BillingURL:           billingURL,
	}
	if providerCause == causeProviderInsufficientBalance {
		d.Category = "provider_insufficient_balance"
		d.Summary, _ = insufficientBalanceMessage(billingURL)
	} else if snap.cause == causeUserStop {
		d.Category = "user_stop"
		d.Summary = "Agent stopped."
	} else if snap.cause == causeRequestCanceled {
		d.Category = "request_cancelled"
		d.Summary = "Agent connection closed; the run was stopped."
	} else if snap.cause == causeOutputLimit {
		d.Category = "output_limit"
		d.Summary = "Provider output limit reached; the run was stopped."
	} else if snap.cause == causeServiceShutdown {
		d.Category = "service_shutdown"
		d.Summary = "The agent was interrupted by a service shutdown."
	} else if outcome == outcomeFailed && stdoutErrMsg != "" {
		d.Category = "opencode_error"
		d.Summary = stdoutErrMsg
	} else if exit.signaled {
		d.Category = "signal"
		d.Summary = "OpenCode was terminated by signal " + exit.signal + "."
	} else if outcome == outcomeCompletedWError {
		d.Category = "opencode_exit"
		d.Summary = "OpenCode exited with status " + strconv.Itoa(exit.exitCode) + " after completing."
		// A post-completion stdout error is honest process evidence: it is
		// retained as a warning in technical diagnostics and never shown as the
		// failure text of the produced answer.
		if stdoutErrMsg != "" {
			d.Warnings = append([]string{stdoutErrMsg}, d.Warnings...)
		}
	} else if outcome == outcomeFailed {
		d.Category = "opencode_exit"
		d.Summary = "OpenCode exited with status " + strconv.Itoa(exit.exitCode) + "."
		if scannerErr != nil {
			d.Summary = "The provider output stream failed: " + scannerErr.Error()
		}
	} else {
		d.Category = "completed"
		d.Summary = ""
	}
	if d.Category != "completed" && d.Summary == "" {
		d.Summary = "Agent failed."
	}
	if len(d.Errors) > 0 && strings.TrimSpace(d.Summary) != "" && d.Category != "opencode_error" {
		d.Summary = d.Summary + " Details: " + strings.Join(d.Errors, "; ")
	}
	if rec, ok := recovery["result"]; ok {
		d.RecoveryAttempted = rec == "ok" || rec == "failed"
		d.RecoveryResult = rec
	}
	b, _ := json.Marshal(d)
	return string(b), d.Summary
}

// summaryFor computes the concise user-facing message from the outcome and the
// diagnostic precedence rules.
func (a *App) summaryFor(outcome runOutcome, snap runStateSnapshot, stdoutErrMsg string, exit exitStatus, stderrErrors []string) string {
	switch snap.cause {
	case causeUserStop:
		return "Agent stopped."
	case causeRequestCanceled:
		return "Agent connection closed; the run was stopped."
	case causeOutputLimit:
		return "Provider output limit reached; the run was stopped."
	case causeServiceShutdown:
		return "The agent was interrupted by a service shutdown."
	}
	if stdoutErrMsg != "" {
		return stdoutErrMsg
	}
	if exit.signaled {
		return "OpenCode was terminated by signal " + exit.signal + "."
	}
	switch outcome {
	case outcomeCompleted:
		return ""
	case outcomeCompletedWError:
		return "OpenCode exited with status " + strconv.Itoa(exit.exitCode) + " after completing."
	case outcomeCancelled:
		return "Agent stopped."
	case outcomeTruncated:
		return "Provider output limit reached; the run was stopped."
	case outcomeInterrupted:
		return "The agent was interrupted."
	default:
		return "Agent failed."
	}
}

func errorEventText(raw map[string]any) string {
	// The run command emits {"type":"error","error":{"name":..,"data":{"message":..}}}.
	if e, ok := raw["error"].(map[string]any); ok {
		if data, ok := e["data"].(map[string]any); ok {
			if msg, _ := data["message"].(string); msg != "" {
				return msg
			}
		}
		if name, _ := e["name"].(string); name != "" {
			return name
		}
	}
	if msg, _ := raw["message"].(string); msg != "" {
		return msg
	}
	return "OpenCode reported a provider error."
}

// providerErrorMeta extracts an optional provider error code and numeric HTTP
// status from a serialized OpenCode error event, tolerating their absence.
// Only fields that actually appear in the captured event shape are read.
func providerErrorMeta(raw map[string]any) (code string, statusCode int) {
	e, ok := raw["error"].(map[string]any)
	if !ok {
		return "", 0
	}
	if data, ok := e["data"].(map[string]any); ok {
		if c, _ := data["code"].(string); c != "" {
			code = c
		}
		if s, ok := data["statusCode"].(float64); ok {
			statusCode = int(s)
		} else if s, ok := data["status"].(float64); ok {
			statusCode = int(s)
		}
	}
	if c, _ := e["code"].(string); c != "" {
		code = c
	}
	if s, ok := e["statusCode"].(float64); ok {
		statusCode = int(s)
	}
	return code, statusCode
}

// extractBillingURL pulls a candidate billing URL out of a provider error
// message for later validation. It returns the first https URL found; the
// caller must still pass it through sanitizeBillingURL.
func extractBillingURL(msg string) string {
	i := strings.Index(msg, "https://")
	if i < 0 {
		return ""
	}
	rest := msg[i:]
	end := len(rest)
	for j, r := range rest {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != ':' && r != '/' && r != '.' && r != '-' && r != '_' && r != '?' && r != '=' && r != '&' {
			end = j
			break
		}
	}
	return rest[:end]
}

// insufficientBalanceMessage returns the concise, transcript-safe message and
// whether a validated billing action is available.
func insufficientBalanceMessage(billingURL string) (msg string, hasBilling bool) {
	msg = "The provider could not run this request because the account has insufficient credit. Add credit or choose another configured provider, then try again."
	return msg, billingURL != ""
}

func terminalKind(outcome runOutcome) string {
	switch outcome {
	case outcomeCompleted:
		return "done"
	case outcomeCompletedWError:
		return "warning"
	case outcomeFailed:
		return "error"
	case outcomeCancelled, outcomeInterrupted:
		return "cancelled"
	case outcomeTruncated:
		return "truncated"
	}
	return "error"
}

// parsedStderr splits mixed-format stderr output into bounded ERROR and WARN
// collections, tolerating unstructured lines. The raw bounded tail is retained
// separately as a fallback for unstructured output.
func parsedStderr(text string) (errors, warnings []string) {
	for _, line := range strings.Split(text, "\n") {
		msg := strings.TrimSpace(line)
		if msg == "" {
			continue
		}
		if len(msg) > 600 {
			msg = msg[:600]
		}
		switch stderrLevel(line) {
		case "ERROR":
			if len(errors) < 8 {
				errors = append(errors, msg)
			}
		case "WARN":
			if len(warnings) < 8 {
				warnings = append(warnings, msg)
			}
		}
	}
	return errors, warnings
}

func boundedStrings(v []string, n int) []string {
	if len(v) > n {
		v = v[:n]
	}
	return v
}

func boundedTail(s string, n int) string {
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}

func (a *App) agentCancel(w http.ResponseWriter, r *http.Request) {
	var q struct {
		RunID string `json:"runID"`
	}
	if !decode(w, r, &q) {
		return
	}
	if !validRecordID(q.RunID) {
		http.Error(w, "invalid run id", 400)
		return
	}
	a.runMu.Lock()
	run := a.activeRuns[q.RunID]
	a.runMu.Unlock()
	if run == nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	// Record the explicit user stop before invoking cancel so the outcome is
	// classified as a user cancellation rather than a disconnect or signal.
	if !run.state.recordCause(causeUserStop) {
		// The run may already be stopping (duplicate Stop is idempotent) or
		// sealed (already finished). A finished run cannot be stopped again.
		snap := run.state.snapshot()
		if snap.sealed {
			http.Error(w, "run already finished", http.StatusGone)
			return
		}
	}
	run.cancel()
	jsonOut(w, map[string]bool{"cancelled": true})
}

// agentRunDiagnostics serves the bounded, redacted technical-detail record for
// a finished run to the authenticated owner. Raw stderr is never returned in
// conversation list responses; it is only available here.
func (a *App) agentRunDiagnostics(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method", 405)
		return
	}
	runID := strings.TrimSpace(r.URL.Query().Get("runID"))
	if !validRecordID(runID) {
		http.Error(w, "invalid run id", 400)
		return
	}
	var diag string
	err := a.db.QueryRow("SELECT diagnostics FROM agent_runs WHERE id = ?", runID).Scan(&diag)
	if err != nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	if diag == "" {
		http.Error(w, "no diagnostics available", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(diag))
}

// agentImage serves a generated image from an isolated session's OpenCode data
// directory so the browser can display model-produced images. It resolves the
// path within the session data dir, rejects anything else (including symlinks
// escaping it), and only serves content that sniffs as an image.
func (a *App) agentImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method", 405)
		return
	}
	clientSession := strings.TrimSpace(r.URL.Query().Get("session"))
	if !validRecordID(clientSession) {
		http.Error(w, "invalid session", 400)
		return
	}
	base := filepath.Join(a.dataDir, "sessions", clientSession, "data")
	abs, err := filepath.Abs(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, "invalid path", 400)
		return
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		http.Error(w, "path escapes session data", 400)
		return
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "file is unavailable", 404)
			return
		}
		http.Error(w, "path is unavailable", 400)
		return
	}
	rel, err = filepath.Rel(base, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		http.Error(w, "path escapes session data", 400)
		return
	}
	f, err := os.Open(real)
	if err != nil {
		http.Error(w, "file is unavailable", 404)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.Error(w, "not a regular file", 400)
		return
	}
	if st.Size() > 20<<20 {
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		return
	}
	header := make([]byte, 512)
	n, _ := io.ReadFull(f, header)
	ct := http.DetectContentType(header[:n])
	if !strings.HasPrefix(ct, "image/") {
		http.Error(w, "not an image", 400)
		return
	}
	w.Header().Set("Content-Type", ct)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "file is unavailable", 500)
		return
	}
	_, _ = io.Copy(w, f)
}

// rewriteImageURLs converts file:// image URLs in an OpenCode streamed event
// into Cortex endpoint URLs the browser can fetch. Images produced by a model
// arrive as file parts or tool-result attachments carrying mediaType/mime.
func rewriteImageURLs(v any, clientSession string) {
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, y := range x {
				walk(y)
			}
		case map[string]any:
			u, _ := x["url"].(string)
			mime, _ := x["mediaType"].(string)
			if mime == "" {
				mime, _ = x["mime"].(string)
			}
			if strings.HasPrefix(u, "file://") && strings.HasPrefix(strings.ToLower(mime), "image/") {
				if p := fileURLPath(u); p != "" {
					x["url"] = "/api/agent/image?session=" + url.QueryEscape(clientSession) + "&path=" + url.QueryEscape(p)
				}
			}
			for _, y := range x {
				walk(y)
			}
		}
	}
	walk(v)
}

func fileURLPath(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return u.Path
}

// agentModels lists the models opencode knows for a provider by running
// `opencode models <id>` in the same isolated environment as a run. The list
// comes from the provider's catalog (models.dev for built-ins, the provider
// config for custom ones), so no credential is required beyond what a run needs.
func (a *App) agentModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method", 405)
		return
	}
	providerID := strings.TrimSpace(r.URL.Query().Get("provider"))
	p, ok := providerByID(providerID)
	if !ok {
		http.Error(w, "unknown provider", 400)
		return
	}
	if p.ID == "opencode" {
		jsonOut(w, map[string]any{"provider": providerID, "models": zenModels})
		return
	}
	modelID := a.configuredModel(providerID)
	if modelID == "" {
		modelID = p.DefaultModel
	}
	empty := func(note string) {
		jsonOut(w, map[string]any{"provider": providerID, "models": []string{}, "note": note})
	}
	key := ""
	if p.AuthMode == "key" {
		var configured bool
		key, configured = a.credential(providerID)
		if !configured {
			empty("Configure a " + p.Label + " API key to list models")
			return
		}
	} else if !a.hostOpenCodeAuthConfigured(p.OpenCodeID) {
		empty("Connect " + p.Label + " in OpenCode to list models")
		return
	}
	binary, err := exec.LookPath("opencode")
	if err != nil {
		empty("OpenCode is not installed")
		return
	}
	runDir, err := os.MkdirTemp("", "cortex-opencode-config-")
	if err != nil {
		empty("")
		return
	}
	defer os.RemoveAll(runDir)
	configDir := filepath.Join(runDir, "config")
	dataDir := filepath.Join(runDir, "data")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		empty("")
		return
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		empty("")
		return
	}
	if p.AuthMode == "opencode-auth" {
		if err := copyOpenCodeAuth(dataDir); err != nil {
			empty("")
			return
		}
	}
	cfg, err := cortexOpenCodeConfig(p, modelID)
	if err != nil {
		empty("")
		return
	}
	configPath := filepath.Join(configDir, "opencode.json")
	if err := os.WriteFile(configPath, cfg, 0600); err != nil {
		empty("")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "models", p.OpenCodeID)
	env := append([]string{}, os.Environ()...)
	if p.AuthMode == "key" {
		env = setEnv(env, "CORTEX_PROVIDER_API_KEY", key)
	}
	env = ghConfigDirEnv(env)
	env = setEnv(env, "OPENCODE_CONFIG", configPath)
	env = setEnv(env, "OPENCODE_CONFIG_DIR", configDir)
	env = setEnv(env, "XDG_CONFIG_HOME", configDir)
	env = setEnv(env, "XDG_DATA_HOME", dataDir)
	env = setEnv(env, "OPENCODE_DISABLE_AUTOUPDATE", "1")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		note := a.redactSecrets(strings.TrimSpace(string(out)))
		if note == "" {
			note = errText(err)
		}
		empty(note)
		return
	}
	jsonOut(w, map[string]any{"provider": providerID, "models": parseModelLines(out, p.OpenCodeID)})
}

func parseModelLines(out []byte, providerID string) []string {
	models := []string{}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, providerID+"/") {
			continue
		}
		id := strings.TrimPrefix(line, providerID+"/")
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, id)
		if len(models) >= 500 {
			break
		}
	}
	return models
}

// agentRunArgs builds the opencode run argv. The prompt must follow the "--"
// separator: "--file" is an array option in opencode run, so bare words placed
// after it would otherwise be consumed as further file paths.
//
// Production runs at WARN so routine INFO lifecycle logs do not flood stderr;
// INFO diagnostics remain available through an opt-in debug setting.
func agentRunArgs(workspace, modelRef, session string, files []string, prompt string) []string {
	return coreagent.RunArgs(workspace, modelRef, strings.TrimSpace(session), files, prompt)
}

// openCodeVersion returns the installed OpenCode version captured at first
// use, or the empty string when it cannot be determined.
var openCodeVersion = func() func() string {
	version := ""
	var once sync.Once
	return func() string {
		once.Do(func() {
			if b, err := exec.Command("opencode", "--version").Output(); err == nil {
				version = strings.TrimSpace(string(b))
				if len(version) > 64 {
					version = version[:64]
				}
			}
		})
		return version
	}
}()

func writeImageAttachments(imgs []imageUpload) (dir string, paths []string, err error) {
	dir, err = os.MkdirTemp("", "cortex-images-")
	if err != nil {
		return "", nil, err
	}
	for _, img := range imgs {
		path, perr := writeImageAttachment(dir, img)
		if perr != nil {
			os.RemoveAll(dir)
			return "", nil, perr
		}
		paths = append(paths, path)
	}
	return dir, paths, nil
}

func writeImageAttachment(dir string, img imageUpload) (string, error) {
	const prefix = "data:image/"
	if len(img.Name) > 500 {
		return "", errors.New("image name is too long")
	}
	if !strings.HasPrefix(img.Data, prefix) {
		return "", errors.New("unsupported image format")
	}
	comma := strings.IndexByte(img.Data, ',')
	if comma < 0 {
		return "", errors.New("invalid image data")
	}
	meta := img.Data[len(prefix):comma]
	var ext string
	switch {
	case strings.HasPrefix(meta, "png;"):
		ext = ".png"
	case strings.HasPrefix(meta, "jpeg;") || strings.HasPrefix(meta, "jpg;"):
		ext = ".jpg"
	case strings.HasPrefix(meta, "gif;"):
		ext = ".gif"
	case strings.HasPrefix(meta, "webp;"):
		ext = ".webp"
	default:
		return "", errors.New("unsupported image type")
	}
	raw, err := base64.StdEncoding.DecodeString(img.Data[comma+1:])
	if err != nil {
		return "", errors.New("invalid image encoding")
	}
	if len(raw) == 0 {
		return "", errors.New("empty image")
	}
	if len(raw) > maxImageBytes {
		return "", errors.New("image exceeds the 10 MiB limit")
	}
	path := filepath.Join(dir, randomToken(12)+ext)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		return "", err
	}
	return path, nil
}

func cortexOpenCodeConfig(provider Provider, modelID string) ([]byte, error) {
	cfg := map[string]any{
		"$schema":    "https://opencode.ai/config.json",
		"model":      modelRefFor(provider, modelID),
		"permission": "allow",
	}
	if provider.AuthMode == "key" {
		if provider.ID == "opencode" {
			cfg["provider"] = zenProviderConfig(modelID)
		} else {
			cfg["provider"] = map[string]any{provider.OpenCodeID: map[string]any{
				"options": map[string]any{"apiKey": "{env:CORTEX_PROVIDER_API_KEY}"},
			}}
		}
	}
	return json.Marshal(cfg)
}

// zenModels is the OpenCode Zen catalogue, snapshot from
// https://opencode.ai/zen/v1/models on 2026-08-29. Zen serves each model family
// through a different protocol, so the catalogue is split by family and the
// generated config declares one provider entry per family (see zenProviderConfig).
// Unknown or newer IDs can still be typed manually; they route to the
// OpenAI-compatible chat family. Refresh this list with
// `curl https://opencode.ai/zen/v1/models` when needed.
var (
	zenOpenAIModels = []string{
		"gpt-5.6-sol",
		"gpt-5.6-terra",
		"gpt-5.6-luna",
		"gpt-5.5",
		"gpt-5.5-pro",
		"gpt-5.4",
		"gpt-5.4-pro",
		"gpt-5.4-mini",
		"gpt-5.4-nano",
		"gpt-5.3-codex-spark",
		"gpt-5.3-codex",
		"gpt-5.2",
		"gpt-5.2-codex",
		"gpt-5.1",
		"gpt-5.1-codex-max",
		"gpt-5.1-codex",
		"gpt-5.1-codex-mini",
		"gpt-5",
		"gpt-5-codex",
		"gpt-5-nano",
		"grok-build-0.1",
		"grok-4.6",
		"grok-4.5",
		"muse-spark-1.2",
	}
	zenAnthropicModels = []string{
		"claude-fable-5",
		"claude-opus-5",
		"claude-opus-4-8",
		"claude-opus-4-7",
		"claude-opus-4-6",
		"claude-opus-4-5",
		"claude-sonnet-5",
		"claude-sonnet-4-6",
		"claude-sonnet-4-5",
		"claude-sonnet-4",
		"claude-haiku-4-5",
	}
	zenGoogleModels = []string{
		"gemini-3.6-flash",
		"gemini-3.7-flash",
		"gemini-3.5-flash-lite",
		"gemini-3.5-flash",
		"gemini-3.1-pro",
		"gemini-3-flash",
	}
	zenChatModels = []string{
		"deepseek-v4-pro",
		"deepseek-v4-flash",
		"glm-5.2",
		"glm-5.1",
		"glm-5",
		"minimax-m3",
		"minimax-m2.7",
		"minimax-m2.5",
		"kimi-k3",
		"kimi-k2.7-code",
		"kimi-k2.6",
		"kimi-k2.5",
		"qwen3.6-plus",
		"qwen3.5-plus",
		"big-pickle",
		"deepseek-v4-flash-free",
		"muse-spark-1.2-contributor-free",
		"mimo-v2.5-free",
		"hy3-free",
		"ling-3.0-flash-fin-free",
		"nemotron-3-ultra-free",
		"nemotron-3.5-lightning-free",
		"laguna-s-2.1-free",
	}
	zenModels = append(append(append(append([]string{}, zenOpenAIModels...), zenAnthropicModels...), zenGoogleModels...), zenChatModels...)
)

// zenProviderKey maps a Zen model ID to the generated provider entry that
// speaks the protocol that model family needs on the Zen gateway.
func zenProviderKey(modelID string) string {
	switch {
	case strings.HasPrefix(modelID, "gpt-"), strings.HasPrefix(modelID, "grok-"), modelID == "muse-spark-1.2":
		return "opencode"
	case strings.HasPrefix(modelID, "claude-"):
		return "opencode-anthropic"
	case strings.HasPrefix(modelID, "gemini-"):
		return "opencode-google"
	default:
		return "opencode-chat"
	}
}

func zenProviderConfig(selected string) map[string]any {
	providers := map[string]any{}
	addFamily := func(key, npm string, models []string) {
		m := make(map[string]any, len(models)+1)
		for _, id := range models {
			m[id] = map[string]any{"name": id}
		}
		if zenProviderKey(selected) == key {
			m[selected] = map[string]any{"name": selected}
		}
		providers[key] = map[string]any{
			"npm":     npm,
			"name":    "OpenCode Zen",
			"options": map[string]any{"apiKey": "{env:CORTEX_PROVIDER_API_KEY}", "baseURL": "https://opencode.ai/zen/v1"},
			"models":  m,
		}
	}
	addFamily("opencode", "@ai-sdk/openai", zenOpenAIModels)
	addFamily("opencode-anthropic", "@ai-sdk/anthropic", zenAnthropicModels)
	addFamily("opencode-google", "@ai-sdk/google", zenGoogleModels)
	addFamily("opencode-chat", "@ai-sdk/openai-compatible", zenChatModels)
	return providers
}

func modelRefFor(provider Provider, modelID string) string {
	if provider.ID == "opencode" {
		return zenProviderKey(modelID) + "/" + modelID
	}
	return provider.OpenCodeID + "/" + modelID
}

func copyOpenCodeAuth(dataDir string) error {
	src := hostOpenCodeAuthPath()
	if src == "" {
		return errors.New("OpenCode auth path unavailable")
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	dstDir := filepath.Join(dataDir, "opencode")
	if err := os.MkdirAll(dstDir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dstDir, "auth.json"), b, 0600)
}

func eventText(raw map[string]any) string {
	if raw["type"] != "text" {
		return ""
	}
	part, _ := raw["part"].(map[string]any)
	s, _ := part["text"].(string)
	return strings.TrimSpace(s)
}

// normalizedEvent maps an OpenCode stdout event to the server-owned
// conversation event kind/text/name that mirrors what the frontend would
// render. Non-rendered event types (step_start/step_finish) return "".
// taskSnapshot extracts and validates a todowrite task list from a tool_use
// event. It returns a normalized JSON string (kind "task") or "" when the
// payload is not a trusted structured todowrite snapshot. Validation bounds
// item count, content length, statuses and priorities; unrecognized values are
// normalized so a malformed item never corrupts the panel.
// taskSnapshot extracts and normalizes a todowrite snapshot. Each accepted
// content field is redacted through the established credential redactor before
// the list is marshalled, so the exact JSON string can be used for durable
// persistence, live delivery and reload without a separate string-level pass
// that could miss JSON-escaped secrets. Length bounds are decided before
// redaction so an oversized original value cannot become acceptable merely
// because redaction shortened it.
func (a *App) taskSnapshot(raw map[string]any) string {
	part, _ := raw["part"].(map[string]any)
	tool, _ := part["tool"].(string)
	if tool != "todowrite" {
		return ""
	}
	state, _ := part["state"].(map[string]any)
	input, _ := state["input"].(map[string]any)
	items, ok := input["todos"].([]any)
	if !ok {
		return ""
	}
	if len(items) > 100 {
		items = items[:100]
	}
	// A valid todowrite with an empty todos list is an authoritative clear
	// operation: it must reach the frontend so a stale panel is hidden rather
	// than retained.
	if len(items) == 0 {
		return "[]"
	}
	out := make([]map[string]string, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		content, _ := m["content"].(string)
		content = strings.TrimSpace(content)
		// Bound the length before redaction.
		if content == "" || len(content) > 500 {
			continue
		}
		content = a.redactSecrets(content)
		status := normalizeTaskStatus(fmt.Sprint(m["status"]))
		priority := normalizeTaskPriority(fmt.Sprint(m["priority"]))
		out = append(out, map[string]string{"content": content, "status": status, "priority": priority})
	}
	if len(out) == 0 {
		return ""
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// sanitizeAgentEvent recursively redacts configured credentials from every
// string in an OpenCode event before it is forwarded to the browser, mirroring
// the durable-transcript sanitization so no path can bypass output redaction.
// Depth and cardinality limits keep untrusted payloads bounded.
func (a *App) sanitizeAgentEvent(v any, depth int) any {
	if depth > 32 {
		return "[depth limit]"
	}
	switch x := v.(type) {
	case string:
		return a.redactSecrets(x)
	case []any:
		if len(x) > 1024 {
			x = x[:1024]
		}
		out := make([]any, len(x))
		for i := range x {
			out[i] = a.sanitizeAgentEvent(x[i], depth+1)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = a.sanitizeAgentEvent(val, depth+1)
		}
		return out
	default:
		return v
	}
}

func normalizeTaskStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "completed", "done":
		return "completed"
	case "in_progress", "in-progress", "running":
		return "in_progress"
	case "pending", "":
		return "pending"
	default:
		return "pending"
	}
}

func normalizeTaskPriority(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "high":
		return "high"
	case "medium", "normal":
		return "medium"
	case "low":
		return "low"
	default:
		return "medium"
	}
}

func normalizedEvent(raw map[string]any) (kind, text, name string) {
	typ, _ := raw["type"].(string)
	switch typ {
	case "text":
		if t := eventText(raw); t != "" {
			return "assistant", t, ""
		}
	case "tool_use":
		part, _ := raw["part"].(map[string]any)
		tool, _ := part["tool"].(string)
		state, _ := part["state"].(map[string]any)
		status, _ := state["status"].(string)
		input, _ := state["input"].(map[string]any)
		lines := []string{"↳ " + tool}
		if status != "" {
			lines = []string{"↳ " + tool + " · " + status}
		}
		if command, _ := input["command"].(string); tool == "bash" && command != "" {
			lines = append(lines, "$ "+command)
		} else if fp, _ := input["filePath"].(string); fp != "" {
			lines = append(lines, fp)
		} else if fp, _ := input["path"].(string); fp != "" {
			lines = append(lines, fp)
		}
		if errTxt, _ := state["error"].(string); errTxt != "" {
			lines = append(lines, "ERROR: "+errTxt)
		} else if outTxt, _ := state["output"].(string); outTxt != "" {
			lines = append(lines, boundedTail(outTxt, 2600))
		}
		return "tool", strings.Join(lines, "\n"), ""
	case "error":
		return "error", errorEventText(raw), ""
	case "file":
		part, _ := raw["part"].(map[string]any)
		url, _ := part["url"].(string)
		mime, _ := part["mediaType"].(string)
		if mime == "" {
			mime, _ = part["mime"].(string)
		}
		if strings.HasPrefix(strings.ToLower(mime), "image/") && sanitizeImageURL(url) != "" {
			filename, _ := part["filename"].(string)
			return "image", sanitizeImageURL(url), filename
		}
	}
	return "", "", ""
}
func recoverSession(ctx context.Context, binary, id, workspace, clientSession string, env []string) (string, []map[string]string, uint64, uint64, float64, error) {
	cmd := exec.CommandContext(ctx, binary, "export", id)
	cmd.Dir = workspace
	cmd.Env = env
	stdout := &boundedCommandOutput{limit: 8 << 20}
	stderrOut := &boundedCommandOutput{limit: 256 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderrOut
	err := cmd.Run()
	if stdout.truncated || stderrOut.truncated {
		return "", nil, 0, 0, 0, errors.New("session export exceeded output limit")
	}
	if err != nil {
		msg := strings.TrimSpace(stderrOut.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", nil, 0, 0, 0, errors.New(msg)
	}
	start := strings.IndexByte(stdout.String(), '{')
	if start < 0 {
		return "", nil, 0, 0, 0, errors.New("session export did not contain JSON")
	}
	var v any
	if err := json.Unmarshal([]byte(stdout.String())[start:], &v); err != nil {
		return "", nil, 0, 0, 0, err
	}
	rewriteImageURLs(v, clientSession)
	texts := assistantTexts(v)
	images := assistantImages(v)
	var i, o uint64
	var c float64
	collectUsage(v, &i, &o, &c)
	return strings.Join(texts, "\n\n"), images, i, o, c, nil
}

// boundedCommandOutput retains only the first limit bytes written to it and
// records whether anything was discarded. Used to bound recovery subprocess
// output without unbounded buffering.
type boundedCommandOutput struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

func (w *boundedCommandOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	remaining := w.limit - len(w.buf)
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		w.buf = append(w.buf, p[:remaining]...)
	}
	if remaining < len(p) {
		w.truncated = true
	}
	return n, nil
}

func (w *boundedCommandOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}
func assistantImages(v any) []map[string]string {
	out := []map[string]string{}
	seen := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, y := range x {
				walk(y)
			}
		case map[string]any:
			role, _ := x["role"].(string)
			if info, ok := x["info"].(map[string]any); ok && role == "" {
				role, _ = info["role"].(string)
			}
			if role == "assistant" {
				if parts, ok := x["parts"].([]any); ok {
					for _, y := range parts {
						p, _ := y.(map[string]any)
						if p == nil {
							continue
						}
						if p["type"] == "file" || p["type"] == "image" {
							if m, _ := p["mediaType"].(string); strings.HasPrefix(strings.ToLower(m), "image/") {
								u, _ := p["url"].(string)
								n, _ := p["filename"].(string)
								if u != "" && !seen[u] {
									seen[u] = true
									out = append(out, map[string]string{"url": u, "name": n})
								}
							}
						}
						if p["type"] == "tool" {
							if state, ok := p["state"].(map[string]any); ok {
								if atts, ok := state["attachments"].([]any); ok {
									for _, a := range atts {
										am, _ := a.(map[string]any)
										if am == nil {
											continue
										}
										if m, _ := am["mime"].(string); strings.HasPrefix(strings.ToLower(m), "image/") {
											u, _ := am["url"].(string)
											if u != "" && !seen[u] {
												seen[u] = true
												out = append(out, map[string]string{"url": u})
											}
										}
									}
								}
							}
						}
					}
					return
				}
			}
			for _, y := range x {
				walk(y)
			}
		}
	}
	walk(v)
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}
func assistantTexts(v any) []string {
	out := []string{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, y := range x {
				walk(y)
			}
		case map[string]any:
			role, _ := x["role"].(string)
			if info, ok := x["info"].(map[string]any); ok && role == "" {
				role, _ = info["role"].(string)
			}
			if role == "assistant" {
				if parts, ok := x["parts"].([]any); ok {
					for _, y := range parts {
						p, _ := y.(map[string]any)
						if p["type"] == "text" {
							if t, _ := p["text"].(string); strings.TrimSpace(t) != "" {
								out = append(out, t)
							}
						}
					}
					return
				}
			}
			for _, y := range x {
				walk(y)
			}
		}
	}
	walk(v)
	return out
}
func collectUsage(v any, input, output *uint64, cost *float64) {
	if xs, ok := v.([]any); ok {
		for _, x := range xs {
			collectUsage(x, input, output, cost)
		}
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	// OpenCode session exports record step-finish usage as:
	// "tokens": {"input": ..., "output": ..., ...}, with cost beside it.
	if tokens, ok := m["tokens"].(map[string]any); ok {
		if n, ok := number(tokens["input"]); ok {
			*input += uint64(n)
		}
		if n, ok := number(tokens["output"]); ok {
			*output += uint64(n)
		}
	}
	for k, x := range m {
		lk := strings.ToLower(k)
		switch lk {
		case "inputtokens", "input_tokens":
			if n, ok := number(x); ok {
				*input += uint64(n)
			}
		case "outputtokens", "output_tokens":
			if n, ok := number(x); ok {
				*output += uint64(n)
			}
		case "cost", "costusd", "cost_usd":
			if n, ok := number(x); ok {
				*cost += n
			}
		}
		collectUsage(x, input, output, cost)
	}
}
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, e := n.Float64()
		return f, e == nil
	case string:
		f, e := strconv.ParseFloat(n, 64)
		return f, e == nil
	}
	return 0, false
}
func sanitize(s, secret string) string {
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	if len(s) > 12000 {
		s = s[len(s)-12000:]
	}
	return s
}
