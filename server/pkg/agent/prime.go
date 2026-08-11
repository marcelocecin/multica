package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// primeBlockedArgs are flags hardcoded by the daemon that must not be
// overridden by user-configured custom_args. `--mode` selects the ACP
// transport (`--mode acp`); overriding it would break the daemon↔Prime
// Agent communication contract. `--cwd` is set implicitly via cmd.Dir
// (see Execute) rather than a flag, so a user-supplied `--cwd` could
// silently move the agent's real working directory away from opts.Cwd
// without the daemon knowing.
var primeBlockedArgs = map[string]blockedArgMode{
	"--mode": blockedWithValue,
	"--cwd":  blockedWithValue,
}

// primeGracefulExitGraceNanos optionally overrides, in nanoseconds, how long a
// cancelled prime-agent is given to exit on its own from the stdin EOF before
// its process group is signalled at all. Set via atomic store in tests; zero
// keeps the default.
var primeGracefulExitGraceNanos atomic.Int64

func primeGracefulExitGrace() time.Duration {
	if n := primeGracefulExitGraceNanos.Load(); n > 0 {
		return time.Duration(n)
	}
	return 5 * time.Second
}

// primeTerminateGraceNanos optionally overrides, in nanoseconds, how long a
// cancelled prime-agent process group is given to exit after SIGTERM before
// it is SIGKILLed. Set via atomic store in tests; zero keeps the default.
var primeTerminateGraceNanos atomic.Int64

func primeTerminateGrace() time.Duration {
	if n := primeTerminateGraceNanos.Load(); n > 0 {
		return time.Duration(n)
	}
	return 5 * time.Second
}

// primeBackend implements Backend by spawning `prime-agent --mode acp` and
// communicating via the ACP (Agent Client Protocol) JSON-RPC 2.0 over
// stdin/stdout.
//
// Prime Agent's ACP server speaks the same protocol family as
// Hermes/Kimi/QwenPaw, so this reuses the shared hermesClient ACP transport
// — only the binary, launch args, and session semantics differ.
//
// Notable contract with Prime Agent v0.7.1 (verified against
// https://github.com/PrimeIntellect-ai/prime-agent/tree/v0.7.1 — links below
// point at specific files/lines on that tag):
//   - `initialize` reports `agentCapabilities.loadSession: false` and there
//     is no `session/resume`/`session/load` method on the wire at all — Prime
//     hosts exactly one session per ACP connection. Execute therefore never
//     attempts a resume-style call regardless of opts.ResumeSessionID; every
//     turn is a fresh `session/new`. See
//     https://github.com/PrimeIntellect-ai/prime-agent/blob/v0.7.1/packages/coding-agent/src/modes/acp/acp-mode.ts
//   - Prime's real working directory is fixed at OS process-spawn time
//     (via cmd.Dir here), not by the `cwd` sent in `session/new` — that
//     field is only compared against the real one and, on mismatch,
//     reported back informationally in `_meta`. Setting cmd.Dir = opts.Cwd
//     (as every backend already does) keeps the two in sync, so no mismatch
//     is expected in normal operation. Same file as above.
//   - Prime never reads `session/new`'s `mcpServers` content or a model field
//     on `session/new`/`session/prompt` — MCP injection and per-session model
//     selection are Phase-1 non-goals for this provider (see
//     ModelSelectionSupported and packages/core/agents/mcp-support.ts on the
//     frontend). Execute does send an mcpServers key on session/new, but only
//     ever as an empty array: a live smoke test against the real binary
//     showed the ACP SDK's request schema requires the field to be present
//     even though Prime's handler ignores its contents, so this is a
//     required-field workaround, never a channel for opts.McpConfig.
//   - Prime has no tool-permission-gating RPC (no `session/request_permission`
//     observed anywhere in its source) — tools always auto-execute, so unlike
//     Hermes this needs no YOLO-mode-equivalent env var.
//   - Prime reads AGENTS.md (and CLAUDE.md) from its cwd natively, so the
//     Multica runtime brief reaches it through execenv's normal per-task
//     context file, not through ExecOptions.SystemPrompt.
//   - Prime's IPython-hosted `rlm.run` tool can spawn a fire-and-forget
//     "subagent" (RLM child session) that keeps running and streaming
//     `session_info_update` notifications after `session/prompt` returns —
//     ACP has no RPC to wait for these to reach a terminal state. Phase 1
//     does not track them: Execute sets RLM_MAX_DEPTH=0 in the child
//     process's environment, which `_startRlmChildRun` checks against the
//     current session's rlmDepth before spawning a child, disabling
//     subagents on the default path — see the RLM_MAX_DEPTH doc comment
//     below for the full precedence chain and its one known gap (a
//     pre-existing global Prime Agent setting can outrank this env var). A
//     future phase may track subagents to a terminal state instead of
//     disabling them; that is out of scope here.
type primeBackend struct {
	cfg Config
}

func (b *primeBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "prime-agent"
	}
	if _, err := exec.LookPath(execPath); err != nil {
		return nil, fmt.Errorf("prime-agent executable not found at %q: %w", execPath, err)
	}

	timeout := opts.Timeout
	runCtx, cancel := runContext(ctx, timeout)

	// ExtraArgs (MULTICA_PRIME_ARGS, daemon-wide) precede CustomArgs
	// (per-agent), matching the documented precedence every other backend
	// that accepts both follows.
	primeArgs := []string{"--mode", "acp"}
	primeArgs = append(primeArgs, filterCustomArgs(opts.ExtraArgs, primeBlockedArgs, b.cfg.Logger)...)
	primeArgs = append(primeArgs, filterCustomArgs(opts.CustomArgs, primeBlockedArgs, b.cfg.Logger)...)

	cmd := exec.CommandContext(runCtx, execPath, primeArgs...)
	// Run prime-agent in its own process group so cancellation can reach the
	// whole tree — the IPython kernel and any tool subprocess it spawns, not
	// just the direct child. The default CommandContext behaviour SIGKILLs
	// only the leader, which would orphan those descendants. This mirrors the
	// fix already made for claude (#5918), codex (#4520), and opencode
	// (#4533); see proc_other.go / proc_windows.go.
	configureProcessGroup(cmd)
	// Take over context cancellation: the default would SIGKILL only the
	// leader the instant runCtx is done, which would not give
	// connection.dispose() (Prime's own ACP-mode shutdown hook) any chance to
	// clean up the IPython kernel before the process is torn down. We instead
	// drive a graceful group-wide SIGTERM→SIGKILL from the cancellation
	// goroutine below and close stdout only after the tree has been
	// signalled. Returning nil keeps os/exec from racing us with its own
	// kill; WaitDelay remains the hard backstop.
	cmd.Cancel = func() error { return nil }
	hideAgentWindow(cmd)
	b.cfg.Logger.Info("agent command", "exec", execPath, "args", primeArgs)
	cmd.WaitDelay = 10 * time.Second
	agentsMDPresent := false
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
		if _, err := os.Stat(filepath.Join(opts.Cwd, "AGENTS.md")); err == nil {
			agentsMDPresent = true
		}
	}
	b.cfg.Logger.Info("prime-agent acp starting", "cwd", opts.Cwd, "agents_md_present", agentsMDPresent)
	// RLM_MAX_DEPTH=0 disables Prime's rlm.run subagent tool on the default
	// path. Verified directly against prime-agent v0.7.1 source:
	// _startRlmChildRun (the sole entry point every rlm.run call goes
	// through) refuses to spawn a child whenever the current session's
	// rlmDepth >= rlmMaxDepth. With rlmMaxDepth resolved to 0, the top-level
	// session (rlmDepth 0) always fails that check before any child is
	// created.
	//
	// rlmMaxDepth's real resolution order (_resolveRlmMaxDepth,
	// agent-session.ts:1573) is, in priority: (1) state persisted on the
	// session's own branch — never present here, since Execute always takes
	// the fresh session/new path and never resumes a branch; (2) an explicit
	// per-session override threaded through session construction — ACP mode
	// never sets this (verified: no "rlmMaxDepth" reference anywhere under
	// modes/acp/); (3) a GLOBAL setting persisted at
	// ~/.prime/agent/settings.json (settingsManager.getRlmMaxDepth), which
	// the SAME LOCAL USER can set outside Multica entirely via Prime's own
	// interactive/daemon mode with `/rlm-max-depth <n> --global`; (4) this
	// RLM_MAX_DEPTH env var; (5) a default of 1.
	//
	// This env var is therefore NOT the top of that chain and is not an
	// absolute guarantee: a pre-existing global rlmMaxDepth the operating
	// user separately configured on this machine takes precedence over
	// RLM_MAX_DEPTH=0 and would silently re-enable subagents for
	// Multica-driven runs too, since Multica does not isolate
	// PRIME_AGENT_CODING_AGENT_DIR per task and so shares the same
	// settings.json a direct/manual `prime-agent` invocation would use. This
	// requires deliberate, out-of-band configuration by that user and is not
	// reachable through the ACP wire protocol itself — the
	// PrimeAgentSessionMeta.rlmMaxDepth/rlmDepth fields declared in
	// acp-meta.ts are outbound telemetry only (never read as client input
	// anywhere under modes/acp/) — so it is tracked as a P2 follow-up, not a
	// Phase 1 regression. On a host with no such pre-existing global
	// override — the default, and the case this provider's tests exercise —
	// RLM_MAX_DEPTH=0 is effective.
	//
	// This also removes the subagent-guidance section from Prime's own system
	// prompt (allowRecursion is threaded into buildRlmPrompt), so the model
	// is never told a capability exists that is actually blocked, and it does
	// not touch refinement/goal/other tools, which run through a separate,
	// synchronous code path (completeSimple) that never calls
	// _startRlmChildRun. See
	// https://github.com/PrimeIntellect-ai/prime-agent/blob/v0.7.1/packages/coding-agent/src/core/agent-session.ts#L9599
	// (the depth check), the same file's _resolveRlmMaxDepth at L1573 (the
	// precedence chain above), and
	// https://github.com/PrimeIntellect-ai/prime-agent/blob/v0.7.1/packages/coding-agent/src/core/system-prompt.ts
	// (the prompt gating).
	cmd.Env = append(buildEnv(b.cfg.Env), "RLM_MAX_DEPTH=0")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("prime-agent stdout pipe: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("prime-agent stdin pipe: %w", err)
	}
	var closeStdinOnce sync.Once
	closeStdin := func() { closeStdinOnce.Do(func() { _ = stdin.Close() }) }

	providerErr := newACPProviderErrorSniffer("prime")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("prime-agent stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start prime-agent: %w", err)
	}

	stderrSink := io.MultiWriter(newLogWriter(b.cfg.Logger, "[prime:stderr] "), providerErr)
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(stderrSink, stderr)
	}()

	b.cfg.Logger.Info("prime-agent acp started", "pid", cmd.Process.Pid, "cwd", opts.Cwd)

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)

	var outputMu sync.Mutex
	var output strings.Builder

	promptDone := make(chan hermesPromptResult, 1)

	c := &hermesClient{
		cfg:          b.cfg,
		stdin:        stdin,
		pending:      make(map[int]*pendingRPC),
		pendingTools: make(map[string]*pendingToolCall),
		onMessage: func(msg Message) {
			if msg.Type == MessageText {
				outputMu.Lock()
				output.WriteString(msg.Content)
				outputMu.Unlock()
			}
			trySend(msgCh, msg)
		},
		onPromptDone: func(result hermesPromptResult) {
			select {
			case promptDone <- result:
			default:
			}
		},
	}

	// procDone closes once cmd.Wait() returns (see the final deferred cleanup
	// in the goroutine below), letting the cancellation handler skip a
	// process that already exited on its own instead of signalling a
	// dead/reused pid.
	procDone := make(chan struct{})

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		scanner := newAgentStreamScanner(stdout)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			c.handleLine(line)
		}
		c.closeAllPending(fmt.Errorf("prime-agent process exited"))
	}()

	// On cancellation / timeout, terminate prime-agent (and its IPython
	// kernel / any tool subprocess it spawned) BEFORE unblocking the scanner.
	// EOF stdin, give prime-agent a bounded window to exit on its own, then
	// SIGTERM the whole process group, give it a grace period so
	// connection.dispose() can clean up, and SIGKILL the group if any member
	// is still alive. SIGKILL is uncatchable, so once delivered no group
	// member can write again — only then is it safe to close the stdout read
	// end as a last-resort unblock for a scanner a wedged descendant still
	// keeps open. WaitDelay is the final backstop. This mirrors
	// claude.go/codex.go/opencode.go/deveco.go's established pattern rather
	// than inventing a new one.
	go func() {
		select {
		case <-procDone:
			return // finished on its own; nothing to terminate
		case <-runCtx.Done():
		}
		closeStdin()
		// Let the stdin EOF above do its work before reaching for a signal.
		// prime-agent drives Prime's whole ACP shutdown hook off that EOF
		// (handle.closed -> connection.dispose() -> complete_owned_session),
		// and that hook is the only thing that stops the DETACHED daemon
		// worker Prime runs the session in — a worker that lives in its own
		// process group and therefore survives everything below. Signalling
		// straight away races the hook; losing that race strands the worker on
		// the supervisor's 30s owner-disconnect fallback, during which its
		// cron scheduler keeps starting fresh turns for a task Multica has
		// already reported as finished.
		select {
		case <-procDone:
		case <-time.After(primeGracefulExitGrace()):
		}
		// procDone only proves the LEADER was reaped, never that the group is
		// empty, so the whole-group check stays authoritative before deciding
		// not to signal — a descendant that outlived the leader must still be
		// terminated below.
		if cmd.Process != nil && !waitProcessGroupGone(cmd.Process, 0) {
			signalProcessGroup(cmd.Process, syscall.SIGTERM)
			// Escalate to a group SIGKILL unless the WHOLE process group has
			// exited within the grace window — keyed off the process group,
			// not procDone, so a SIGTERM-ignoring descendant that does not
			// hold prime-agent's stdout cannot let the leader exit, close
			// procDone, and skip the SIGKILL.
			if !waitProcessGroupGone(cmd.Process, primeTerminateGrace()) {
				signalProcessGroup(cmd.Process, syscall.SIGKILL)
			}
		}
		_ = stdout.Close()
	}()

	go func() {
		defer cancel()
		defer close(msgCh)
		defer close(resCh)
		defer func() {
			closeStdin()
			_ = cmd.Wait()
			close(procDone)
		}()

		startTime := time.Now()
		finalStatus := "completed"
		var finalError string
		var sessionID string

		// 1. Initialize handshake.
		_, err := c.request(runCtx, "initialize", map[string]any{
			"protocolVersion": 1,
			"clientInfo": map[string]any{
				"name":    "multica-agent-sdk",
				"version": "0.2.0",
			},
			"clientCapabilities": map[string]any{},
		})
		if err != nil {
			finalStatus = "failed"
			finalError = fmt.Sprintf("prime-agent initialize failed: %v", err)
			resCh <- Result{Status: finalStatus, Error: finalError, DurationMs: time.Since(startTime).Milliseconds()}
			return
		}

		// 2. Create a session. Prime Agent hosts exactly one session per ACP
		// connection and has no session/resume or session/load method, so this
		// is always a fresh session/new — opts.ResumeSessionID is intentionally
		// never read here (see the type doc comment above).
		cwd := opts.Cwd
		if cwd == "" {
			cwd = "."
		}
		// mcpServers is required by the ACP SDK's session/new request schema
		// (a live smoke test against prime-agent v0.7.1 confirmed the request
		// is rejected with "-32602 Invalid params: mcpServers Required value
		// is missing" when the field is absent) even though Prime's own
		// session/new handler never reads its contents — Phase 1 deliberately
		// does not implement MCP injection for Prime (see
		// packages/core/agents/mcp-support.ts, which excludes "prime"), so
		// this is always an empty array, never opts.McpConfig.
		result, err := c.request(runCtx, "session/new", map[string]any{
			"cwd":        cwd,
			"mcpServers": []any{},
		})
		if err != nil {
			if runCtx.Err() == context.DeadlineExceeded {
				finalStatus = "timeout"
				finalError = fmt.Sprintf("prime-agent timed out during session/new: %v", timeout)
			} else if runCtx.Err() == context.Canceled {
				finalStatus = "aborted"
				finalError = fmt.Sprintf("prime-agent aborted: %v", err)
			} else {
				finalStatus = "failed"
				finalError = fmt.Sprintf("prime-agent session/new failed: %v", err)
			}
			resCh <- Result{Status: finalStatus, Error: finalError, DurationMs: time.Since(startTime).Milliseconds()}
			return
		}
		sessionID = extractACPSessionID(result)
		if sessionID == "" {
			finalStatus = "failed"
			finalError = "prime-agent session/new returned no session ID"
			resCh <- Result{Status: finalStatus, Error: finalError, DurationMs: time.Since(startTime).Milliseconds()}
			return
		}

		c.sessionID = sessionID
		b.cfg.Logger.Info("prime-agent session created", "session_id", sessionID)

		// 3. Build the prompt content. If a system prompt is set, prepend it —
		// in practice this stays empty for Prime, which reads the Multica brief
		// from the AGENTS.md execenv writes into opts.Cwd (see
		// runtimeConfigPath), but ExecOptions.SystemPrompt must not be silently
		// dropped if it is ever populated.
		userText := prompt
		if opts.SystemPrompt != "" {
			userText = opts.SystemPrompt + "\n\n---\n\n" + prompt
		}

		// 4. Send the prompt and wait for the result.
		_, err = c.request(runCtx, "session/prompt", map[string]any{
			"sessionId": sessionID,
			"prompt": []map[string]any{
				{"type": "text", "text": userText},
			},
		})
		if err != nil {
			if runCtx.Err() == context.DeadlineExceeded {
				finalStatus = "timeout"
				finalError = fmt.Sprintf("prime-agent timed out after %s", timeout)
			} else if runCtx.Err() == context.Canceled {
				finalStatus = "aborted"
				finalError = "execution cancelled"
			} else {
				finalStatus = "failed"
				finalError = fmt.Sprintf("prime-agent session/prompt failed: %v", err)
			}
		} else {
			select {
			case pr := <-promptDone:
				if pr.stopReason == "cancelled" {
					duration := time.Since(startTime)
					b.cfg.Logger.Info("prime-agent prompt cancelled", "stopReason", pr.stopReason, "duration", duration.Round(time.Millisecond).String())
				}
				c.mergeUsage(pr.usage)
			default:
			}
		}

		// 5. Close the session — Prime Agent implements session/close (unlike
		// most ACP backends here, which rely on transport teardown alone), so
		// call it when we still have a live connection. Best-effort: a failure
		// here must not overwrite an already-decided finalStatus/finalError,
		// and the closeStdin + cmd.Wait() in the deferred cleanup above still
		// run regardless.
		if finalStatus != "aborted" {
			if _, closeErr := c.request(runCtx, "session/close", map[string]any{
				"sessionId": sessionID,
			}); closeErr != nil {
				b.cfg.Logger.Debug("prime-agent session/close failed", "error", closeErr)
			}
		}

		duration := time.Since(startTime)
		b.cfg.Logger.Info("prime-agent finished", "pid", cmd.Process.Pid, "status", finalStatus, "duration", duration.Round(time.Millisecond).String())

		// Nudge a clean exit before waiting for the reader/stderr goroutines —
		// cmd.Wait() itself happens in the deferred cleanup above, after this
		// goroutine returns, once the process has actually exited or the
		// cancellation goroutine has killed the group.
		closeStdin()

		<-readerDone
		<-stderrDone

		outputMu.Lock()
		finalOutput := output.String()
		outputMu.Unlock()

		finalStatus, finalError = promoteACPResultOnProviderError(finalStatus, finalError, finalOutput, providerErr)

		u := c.accumulatedUsage()

		var usageMap map[string]TokenUsage
		if acpUsagePresent(u) {
			// Prime's model is fixed internally and never reported back to
			// Multica (ModelSelectionSupported("prime") is false), so usage is
			// always attributed to "unknown" rather than a model Multica never
			// selected.
			usageMap = map[string]TokenUsage{"unknown": u}
		}

		resCh <- Result{
			Status:     finalStatus,
			Output:     finalOutput,
			Error:      finalError,
			DurationMs: duration.Milliseconds(),
			// SessionID is deliberately NOT reported here (sessionID is used
			// above only for the in-process session/prompt and session/close
			// RPCs). Reporting it would persist as task.PriorSessionID for a
			// future related task, which the daemon reads as "a resume was
			// expected" independent of any provider-specific gating —
			// task.PriorSessionID != "" alone sets TaskContextForEnv's
			// PriorSessionResumed and ExecOptions.ResumeExpected, and drives a
			// "resuming session" log line — even though Prime never resumes
			// anything (see the type doc comment above). Every future turn is
			// a fresh session/new regardless, so leaving SessionID empty here
			// keeps that fact visible instead of implying continuity that
			// does not exist.
			Usage: usageMap,
		}
	}()

	return &Session{Messages: msgCh, Result: resCh}, nil
}
