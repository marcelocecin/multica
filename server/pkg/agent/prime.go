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

// primeBackend implements Backend by spawning `prime-agent --mode acp` and
// communicating via the ACP (Agent Client Protocol) JSON-RPC 2.0 over
// stdin/stdout.
//
// Prime Agent's ACP server speaks the same protocol family as
// Hermes/Kimi/QwenPaw, so this reuses the shared hermesClient ACP transport
// — only the binary, launch args, and session semantics differ.
//
// Notable contract with Prime Agent v0.7.1 (see okf/prime-agent/ and
// REPORT.md for the full source-verified investigation this is built from):
//   - `initialize` reports `agentCapabilities.loadSession: false` and there
//     is no `session/resume`/`session/load` method on the wire at all — Prime
//     hosts exactly one session per ACP connection. Execute therefore never
//     attempts a resume-style call regardless of opts.ResumeSessionID; every
//     turn is a fresh `session/new`.
//   - Prime's real working directory is fixed at OS process-spawn time
//     (via cmd.Dir here), not by the `cwd` sent in `session/new` — that
//     field is only compared against the real one and, on mismatch,
//     reported back informationally in `_meta`. Setting cmd.Dir = opts.Cwd
//     (as every backend already does) keeps the two in sync, so no mismatch
//     is expected in normal operation.
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
	hideAgentWindow(cmd)
	b.cfg.Logger.Info("agent command", "exec", execPath, "args", primeArgs)
	agentsMDPresent := false
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
		if _, err := os.Stat(filepath.Join(opts.Cwd, "AGENTS.md")); err == nil {
			agentsMDPresent = true
		}
	}
	b.cfg.Logger.Info("prime-agent acp starting", "cwd", opts.Cwd, "agents_md_present", agentsMDPresent)
	cmd.Env = buildEnv(b.cfg.Env)

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

	go func() {
		defer cancel()
		defer close(msgCh)
		defer close(resCh)
		defer func() {
			stdin.Close()
			_ = cmd.Wait()
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
		// and the stdin-close + cmd.Wait() below still runs regardless.
		if finalStatus != "aborted" {
			if _, closeErr := c.request(runCtx, "session/close", map[string]any{
				"sessionId": sessionID,
			}); closeErr != nil {
				b.cfg.Logger.Debug("prime-agent session/close failed", "error", closeErr)
			}
		}

		duration := time.Since(startTime)
		b.cfg.Logger.Info("prime-agent finished", "pid", cmd.Process.Pid, "status", finalStatus, "duration", duration.Round(time.Millisecond).String())

		stdin.Close()
		cancel()

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
