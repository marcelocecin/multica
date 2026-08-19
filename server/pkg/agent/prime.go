package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// primeGlobalSettings is the slice of Prime Agent's global settings.json this
// backend has to understand. Everything else in that file is Prime's business.
type primeGlobalSettings struct {
	// any, not a numeric type: Prime's gate starts at `typeof value ===
	// "number"`, so a quoted "2" has to be rejected rather than coerced.
	// encoding/json decodes every JSON number into float64, which is also
	// what the JavaScript side is comparing.
	RlmMaxDepth any `json:"rlmMaxDepth"`
}

// jsMaxSafeInteger is Number.MAX_SAFE_INTEGER. Prime gates its global
// rlmMaxDepth on Number.isSafeInteger, so a larger value is discarded there and
// must be discarded here too.
const jsMaxSafeInteger = 9007199254740991

// primeGOOS is runtime.GOOS, overridable so tests can cover the Windows
// resolution from any runner. Never assigned outside tests. Mirrors
// reasonixGOOS in internal/daemon/execenv.
var primeGOOS = runtime.GOOS

// primeHomeEnvKey is the variable that decides the child's home directory:
// USERPROFILE on Windows, HOME everywhere else. This is the pair both
// os.UserHomeDir and Node's os.homedir (libuv's uv_os_homedir) key off, and the
// same pair openclawDiscoveryEnvVars tracks for the same reason. HOMEDRIVE and
// HOMEPATH are deliberately absent: neither runtime consults them, so setting
// them moves neither side and cannot desynchronise the two.
func primeHomeEnvKey() string {
	if primeGOOS == "windows" {
		return "USERPROFILE"
	}
	return "HOME"
}

// primeLookupEnv returns the value the child would see for name, or "" when it
// would see none.
//
// Later entries win, matching how exec resolves duplicates, and on Windows the
// comparison is case-insensitive because environment names are: an agent whose
// custom_env declared "userprofile" still overrides the inherited
// "USERPROFILE".
func primeLookupEnv(env []string, name string) string {
	value := ""
	for _, entry := range env {
		key, v, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if key == name || (primeGOOS == "windows" && strings.EqualFold(key, name)) {
			value = v
		}
	}
	return value
}

// primeHomeDir resolves the home directory the spawned prime-agent would use.
//
// It must come from the child's environment, not the daemon's: custom_env
// accepts any key, so an agent can set HOME or USERPROFILE, and the child's
// os.homedir() then points somewhere the daemon's os.UserHomeDir() never would.
// Resolving from the daemon would make the fail-closed gate open a different
// ~/.prime/agent/settings.json from the one Prime reads, and miss a real
// override — the same divergence class as the explicitly-empty and relative
// PRIME_AGENT_CODING_AGENT_DIR cases.
//
// An empty value counts as unset on both sides: Go's os.UserHomeDir ignores an
// empty variable, and libuv falls back to the password database (POSIX) or
// GetUserProfileDirectoryW (Windows) rather than treating it as a path.
func primeHomeDir(env []string) string {
	if home := primeLookupEnv(env, primeHomeEnvKey()); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// primeAgentDirFor resolves the directory the SPAWNED prime-agent would read
// its global settings from, mirroring getAgentDir
// (packages/coding-agent/src/config.ts).
//
// It takes the child's final environment and working directory rather than the
// daemon's, because the two can disagree in ways that would make this gate
// inspect a different file from the one the child actually reads:
//
//   - env is the merged slice handed to the process, so a configured value
//     shadows the inherited one exactly as exec does — including an explicitly
//     empty value, which is not the same as an absent one.
//   - Prime tests the raw string (`if (envDir)`), so it is never trimmed: ""
//     is falsy and falls back to ~/.prime/agent, while " " is truthy and names
//     a relative directory.
//   - a relative value resolves against the child's working directory, which
//     is opts.Cwd, not wherever the daemon happens to be running.
//
// Returns "" when no home directory can be determined and none was configured,
// which the caller reads as "no global settings to inspect".
func primeAgentDirFor(env []string, cwd string) string {
	dir := primeLookupEnv(env, "PRIME_AGENT_CODING_AGENT_DIR")
	home := primeHomeDir(env)
	if dir == "" {
		if home == "" {
			return ""
		}
		return filepath.Join(home, ".prime", "agent")
	}

	switch {
	case dir == "~":
		return home
	case strings.HasPrefix(dir, "~/"):
		if home == "" {
			return ""
		}
		dir = filepath.Join(home, dir[2:])
	}
	if !filepath.IsAbs(dir) && cwd != "" {
		return filepath.Join(cwd, dir)
	}
	return dir
}

// primeGlobalRlmMaxDepth reports the rlmMaxDepth the spawned prime-agent would
// read from its global settings.json, and whether that value would actually
// take effect.
//
// The second return value mirrors Prime's own gate — `typeof value ===
// "number" && Number.isSafeInteger(value) && value >= 0` (isNonNegativeInteger,
// agent-session.ts). A missing file, unreadable file, malformed JSON, absent
// key, non-numeric value, fractional value, negative value or one beyond
// Number.MAX_SAFE_INTEGER all make Prime fall through to RLM_MAX_DEPTH, so they
// report false here too: this must not refuse a run Prime itself would have run
// with subagents disabled.
func primeGlobalRlmMaxDepth(env []string, cwd string) (int64, bool) {
	dir := primeAgentDirFor(env, cwd)
	if dir == "" {
		return 0, false
	}
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return 0, false
	}
	var settings primeGlobalSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		return 0, false
	}
	value, ok := settings.RlmMaxDepth.(float64)
	if !ok {
		return 0, false
	}
	depth := int64(value)
	if float64(depth) != value || depth < 0 || depth > jsMaxSafeInteger {
		return 0, false
	}
	return depth, true
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

// primeTeardownGraceNanos optionally overrides, in nanoseconds, how long the
// success path waits for the stdout/stderr goroutines to drain before forcing
// the teardown. Set via atomic store in tests; zero keeps the default, which
// matches codexGracefulShutdownTimeout.
var primeTeardownGraceNanos atomic.Int64

func primeTeardownGrace() time.Duration {
	if n := primeTeardownGraceNanos.Load(); n > 0 {
		return time.Duration(n)
	}
	return 10 * time.Second
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
	// Fail closed on a global rlmMaxDepth this backend cannot override.
	//
	// RLM_MAX_DEPTH=0 below disables Prime's fire-and-forget subagents on the
	// default path, but it is not the top of Prime's precedence chain: a
	// persisted global rlmMaxDepth outranks it (see the RLM_MAX_DEPTH comment
	// further down for the full chain). With subagents re-enabled, a child can
	// keep running after session/prompt returns, and Multica reports the task
	// complete and tears the owned session down while that child is still
	// working — losing or truncating it. Multica has no lever to prevent that:
	// isolating PRIME_AGENT_CODING_AGENT_DIR would take auth.json with it, and
	// ACP exposes no per-session override. Refusing the run is the only
	// honest outcome, so the operator gets a fixable error instead of a task
	// that silently reports the wrong thing.
	//
	// Checked before the executable lookup deliberately: this is a property of
	// the host's Prime configuration, and it stays true after the CLI is
	// installed, so reporting it first is more useful than a missing-binary
	// error that hides it.
	// childEnv is what the process will actually receive, so the gate below
	// inspects the same settings.json the child would read rather than an
	// approximation built from b.cfg.Env alone.
	childEnv := append(buildEnv(b.cfg.Env), "RLM_MAX_DEPTH=0")
	if depth, ok := primeGlobalRlmMaxDepth(childEnv, opts.Cwd); ok && depth > 0 {
		return nil, fmt.Errorf(
			"prime-agent has a global rlmMaxDepth of %d in %s, which re-enables RLM subagents and outranks the RLM_MAX_DEPTH=0 Multica sets; "+
				"subagents can outlive the task and Multica would report it complete while they are still running. "+
				"Set it to 0 (`/rlm-max-depth 0 --global` in prime-agent) or remove the key to run Prime tasks from Multica",
			depth, filepath.Join(primeAgentDirFor(childEnv, opts.Cwd), "settings.json"))
	}

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

	cmd := b.cfg.commandAt(execPath).exec(runCtx, primeArgs...)
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
	// settings.json a direct/manual `prime-agent` invocation would use.
	//
	// Isolating that directory is the obvious mitigation and is NOT viable:
	// getAgentDir (config.ts) does honour PRIME_AGENT_CODING_AGENT_DIR, but
	// the same directory holds auth.json, models.json and the sessions/cron
	// state, so pointing it at a per-task temp dir strips Prime of its
	// credentials and fails every run. Prime exposes no CLI flag for the
	// depth, and ACP mode never sets the per-session override that would
	// outrank the global (nothing under modes/acp/ references rlmMaxDepth
	// except outbound telemetry). Writing rlmMaxDepth into the user's own
	// global settings.json is the only remaining lever and is not something a
	// task runner should do to a developer's machine.
	//
	// The exposure is narrower than "untracked child processes" suggests: an
	// RLM subagent is a `new AgentSession` inside the same prime-agent
	// process, not a spawned OS process, so there is no descendant escaping
	// the process group — what survives is in-process work continuing after
	// Multica has reported the task complete.
	//
	// Reaching the global override requires deliberate, out-of-band
	// configuration by the operating user and is not reachable through the
	// ACP wire protocol itself — the PrimeAgentSessionMeta.rlmMaxDepth/
	// rlmDepth fields declared in acp-meta.ts are outbound telemetry only
	// (never read as client input anywhere under modes/acp/).
	//
	// Since Multica cannot win that precedence, Execute refuses to launch at
	// all when an effective non-zero global override is present, rather than
	// running a task whose completion it would then misreport (see the
	// fail-closed check at the top of Execute and
	// TestPrimeFailsClosedOnGlobalRlmMaxDepth). On a host with no such
	// override — the default — RLM_MAX_DEPTH=0 is effective and the run
	// proceeds normally.
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
	cmd.Env = childEnv

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

	// startOwnedProcessTree rather than cmd.Start so the tree is owned on
	// Windows too. configureProcessGroup above is a no-op there, so without
	// this nothing is registered in ownedProcessTrees, waitProcessGroupGone
	// reports false unconditionally (never "the group is gone"), and
	// signalProcessGroup falls back to killing the direct child alone — which
	// for a .cmd/.ps1 shim may not even be prime-agent itself. On non-Windows
	// this is exactly cmd.Start: the process group was already configured
	// before the process existed.
	//
	// Owning the tree is safe for this backend specifically because ACP mode
	// never spawns Prime's machine-wide daemon supervisor: the daemon is only
	// started via ensureInteractiveDaemonRunning, which the ACP path never
	// reaches (shouldEnsureInteractiveDaemonForStartup requires interactive
	// mode), and DaemonClient.connect only dials an existing socket. A
	// supervisor a member started separately is therefore not a descendant of
	// this process and never joins this job.
	if err := startOwnedProcessTree(cmd, b.cfg.Logger); err != nil {
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
		if cmd.Process != nil && !waitProcessGroupGone(cmd, 0) {
			signalProcessGroup(cmd, syscall.SIGTERM)
			// Escalate to a group SIGKILL unless the WHOLE process group has
			// exited within the grace window — keyed off the process group,
			// not procDone, so a SIGTERM-ignoring descendant that does not
			// hold prime-agent's stdout cannot let the leader exit, close
			// procDone, and skip the SIGKILL.
			if !waitProcessGroupGone(cmd, primeTerminateGrace()) {
				signalProcessGroup(cmd, syscall.SIGKILL)
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
			// Cancel before Wait, not after. Defers run in reverse order, so
			// the `defer cancel()` above is the LAST thing to run — leaving
			// cmd.Wait() to be entered with runCtx still live. That matters
			// because cmd.WaitDelay only bounds a child that fails to exit
			// *after its context is cancelled*; with a live context and a
			// prime-agent that closed its pipes but has not exited, Wait
			// blocks and the delay never starts. Cancelling here makes the
			// backstop reachable on every path, and it is idempotent and
			// harmless once the process has already exited.
			cancel()
			_ = cmd.Wait()
			close(procDone)
			// The tree has been reaped; drop ownership. Only safe here, after
			// Wait: on Windows releasing closes the job handle, which kills
			// whatever is still inside it — correct for anything that outlived
			// the reap, fatal if a live agent were still using it. No-op on
			// other platforms.
			releaseProcessGroup(cmd)
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
			// Classify off runCtx, not off err: the handshake can be cancelled
			// or time out like any other RPC, and reporting that as "failed"
			// would surface a user-cancelled task as a provider fault. Mirrors
			// the session/new and session/prompt paths below.
			if runCtx.Err() == context.DeadlineExceeded {
				finalStatus = "timeout"
				finalError = fmt.Sprintf("prime-agent timed out during initialize: %v", timeout)
			} else if runCtx.Err() == context.Canceled {
				finalStatus = "aborted"
				finalError = fmt.Sprintf("prime-agent aborted: %v", err)
			} else {
				finalStatus = "failed"
				finalError = fmt.Sprintf("prime-agent initialize failed: %v", err)
			}
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

		// Draining is bounded, including on this success path.
		//
		// These receives sit BEFORE the deferred cmd.Wait(), so cmd.WaitDelay
		// cannot backstop them — it only runs once Wait is entered. A pipe the
		// leader closed is not necessarily a pipe at EOF: any descendant that
		// inherited it holds it open. prime-agent keeps its own stdio
		// disciplined (the IPython kernel, the fork server and the detached
		// daemon worker all get fresh pipes or /dev/null), but its
		// package-manager subprocesses run with stdio ["ignore", 2, 2] under
		// ACP's stdout takeover, so they do inherit fd 2. An unbounded receive
		// here parks a run that already finished until the daemon's idle
		// watchdog force-stops it, turning a successful task into a failed one.
		//
		// cancel() is the forcing function rather than a direct pipe close: it
		// runs the same teardown cancellation uses (stdin EOF, then a group
		// SIGTERM/SIGKILL, then stdout.Close()), and killing the group is what
		// actually releases a pipe a descendant is holding. It is idempotent
		// and cannot disturb finalStatus, which is already decided here.
		//
		// Giving up after the second window is safe rather than merely
		// pragmatic: every value read below is mutex-guarded — output by
		// outputMu, the sniffer by its own mutex, usage by usageMu — so
		// continuing while a goroutine is still writing races nothing. The
		// only cost is trailing output that a wedged descendant was delaying
		// anyway, and cmd.WaitDelay now becomes reachable in the deferred
		// cleanup, which is what finally reaps the process.
		//
		// This reuses hermes.go's waitForHermesPipeDrain rather than adding a
		// second drain helper; the shape is the established one for this ACP
		// family (drain, else cancel and join). The one deliberate difference
		// is that the post-cancel join is bounded here too.
		if !waitForHermesPipeDrain(readerDone, stderrDone, primeTeardownGrace()) {
			b.cfg.Logger.Warn("prime-agent did not close output pipes after stdin EOF; forcing teardown",
				"pid", cmd.Process.Pid, "grace", primeTeardownGrace().String())
			cancel()
			// Bounded a second time rather than joining unconditionally: the
			// group kill releases a pipe held by a descendant inside the
			// group, but a descendant that left it would keep this parked,
			// which is the failure being fixed. Already-closed channels
			// return immediately, so a pipe that drained in the first window
			// costs nothing here.
			if !waitForHermesPipeDrain(readerDone, stderrDone, primeTeardownGrace()) {
				b.cfg.Logger.Error("prime-agent output pipes never reached EOF; continuing without a full drain",
					"pid", cmd.Process.Pid)
			}
		}

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
