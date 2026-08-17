package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrimeModelSelectionUnsupported(t *testing.T) {
	t.Parallel()
	if ModelSelectionSupported("prime") {
		t.Fatal("ModelSelectionSupported(prime) should return false — the model is fixed process-globally and never read over ACP")
	}
	// Other providers should remain supported.
	if !ModelSelectionSupported("claude") {
		t.Fatal("ModelSelectionSupported(claude) should remain true")
	}
}

func TestPrimeThinkingControlUnsupported(t *testing.T) {
	t.Parallel()
	if ThinkingControlSupported("prime") {
		t.Fatal("ThinkingControlSupported(prime) should return false — Prime never reads a thinking-level field over ACP")
	}
}

func TestNewReturnsPrimeBackend(t *testing.T) {
	t.Parallel()
	b, err := New("prime", Config{ExecutablePath: "/nonexistent/prime-agent"})
	if err != nil {
		t.Fatalf("New(prime) error: %v", err)
	}
	if _, ok := b.(*primeBackend); !ok {
		t.Fatalf("expected *primeBackend, got %T", b)
	}
}

func TestPrimeIsSupportedType(t *testing.T) {
	t.Parallel()
	if !IsSupportedType("prime") {
		t.Fatal("IsSupportedType(prime) should be true")
	}
}

func TestPrimeLaunchHeader(t *testing.T) {
	t.Parallel()
	if got := LaunchHeader("prime"); got == "" {
		t.Fatal("LaunchHeader(prime) should not be empty")
	}
}

// fakePrimeACPScript impersonates `prime-agent --mode acp` for unit tests.
// It implements the ACP surface this investigation confirmed Prime Agent
// v0.7.1 actually exposes: initialize, session/new, session/prompt,
// session/close — and deliberately NOT session/resume or session/load,
// mirroring the real binary (see okf/prime-agent/session-model.md).
func fakePrimeACPScript() string {
	return "#!/bin/sh\n" + fakePrimeACPScriptBody()
}

// fakePrimeACPScriptBody is the shebang-less remainder of fakePrimeACPScript,
// factored out so tests that need to prepend a setup line (like capturing the
// process environment) can build "#!/bin/sh\n<setup>\n" + this without
// duplicating the whole ACP dialogue.
func fakePrimeACPScriptBody() string {
	return `while IFS= read -r line; do
  if [ -n "$PRIME_REQUESTS_FILE" ]; then
    printf '%s\n' "$line" >> "$PRIME_REQUESTS_FILE"
  fi
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":false,"promptCapabilities":{"image":true,"embeddedContext":true},"sessionCapabilities":{"close":{}}},"agentInfo":{"name":"prime-agent","title":"Prime Agent","version":"0.7.1"}}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"ses_prime_new"}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn","usage":{"inputTokens":10,"outputTokens":20,"cacheReadTokens":3,"cacheWriteTokens":2,"costUsdTicks":900}}}\n' "$id"
      ;;
    *'"method":"session/close"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}\n' "$id"
      ;;
  esac
done
`
}

func writeFakePrimeScript(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "prime-agent")
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatalf("write fake prime-agent: %v", err)
	}
	return bin
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestPrimeSessionNew(t *testing.T) {
	t.Parallel()
	bin := writeFakePrimeScript(t, fakePrimeACPScript())
	reqFile := filepath.Join(t.TempDir(), "requests.txt")

	b, err := New("prime", Config{
		ExecutablePath: bin,
		Logger:         testLogger(),
		Env:            map[string]string{"PRIME_REQUESTS_FILE": reqFile},
	})
	if err != nil {
		t.Fatalf("New(prime) error: %v", err)
	}

	ctx := context.Background()
	session, err := b.Execute(ctx, "test prompt", ExecOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	for msg := range session.Messages {
		if msg.Type == MessageText {
			t.Logf("received message: %s", msg.Content)
		}
	}

	result := <-session.Result
	if result.Status != "completed" {
		t.Fatalf("expected completed, got status=%q error=%q", result.Status, result.Error)
	}
	// Result.SessionID is deliberately never reported (see TestPrimeSessionIDNeverReported)
	// — but the RPC exchange with the real process must still have used the
	// session id from session/new for session/prompt/session/close.
	if result.SessionID != "" {
		t.Fatalf("expected Result.SessionID to be empty (never reported), got %q", result.SessionID)
	}
	raw, err := os.ReadFile(reqFile)
	if err != nil {
		t.Fatalf("read requests file: %v", err)
	}
	if !strings.Contains(string(raw), `"sessionId":"ses_prime_new"`) {
		t.Fatalf("expected the RPC exchange to use the session/new session id internally, got:\n%s", string(raw))
	}
	if result.Usage == nil {
		t.Fatal("expected usage to be non-nil")
	}
}

// TestPrimeNeverAttemptsResume is the single highest-risk-mitigation test for
// this backend: Prime Agent has no session/resume or session/load method
// (agentCapabilities.loadSession is false, confirmed empirically — see
// prime-acp-test.md). ExecOptions.ResumeSessionID is set unconditionally by
// the daemon for every provider (daemon.go:5853,6218), so primeBackend must
// ignore it and always take the session/new branch rather than translating it
// into a resume-style RPC the real binary would reject with "method not
// found".
func TestPrimeNeverAttemptsResume(t *testing.T) {
	t.Parallel()
	bin := writeFakePrimeScript(t, fakePrimeACPScript())
	reqFile := filepath.Join(t.TempDir(), "requests.txt")

	b, err := New("prime", Config{
		ExecutablePath: bin,
		Logger:         testLogger(),
		Env:            map[string]string{"PRIME_REQUESTS_FILE": reqFile},
	})
	if err != nil {
		t.Fatalf("New(prime) error: %v", err)
	}

	ctx := context.Background()
	session, err := b.Execute(ctx, "test prompt", ExecOptions{
		Cwd: t.TempDir(),
		// A prior task turn's session id, exactly as the daemon would set it
		// unconditionally regardless of provider.
		ResumeSessionID: "ses_from_a_prior_turn",
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	for range session.Messages {
	}
	result := <-session.Result
	if result.Status != "completed" {
		t.Fatalf("expected completed (fresh session/new despite ResumeSessionID), got status=%q error=%q", result.Status, result.Error)
	}
	// Result.SessionID is deliberately never reported (see
	// TestPrimeSessionIDNeverReported) — this specifically prevents a future
	// task's ResumeSessionID from being seeded from this turn.
	if result.SessionID != "" {
		t.Fatalf("expected Result.SessionID to be empty (never reported), got %q", result.SessionID)
	}

	raw, err := os.ReadFile(reqFile)
	if err != nil {
		t.Fatalf("read requests file: %v", err)
	}
	requests := string(raw)

	if !strings.Contains(requests, `"method":"session/new"`) {
		t.Fatalf("expected session/new to be called, got requests:\n%s", requests)
	}
	if strings.Contains(requests, `"method":"session/resume"`) {
		t.Fatalf("prime backend must never call session/resume (Prime Agent has no such method), got:\n%s", requests)
	}
	if strings.Contains(requests, `"method":"session/load"`) {
		t.Fatalf("prime backend must never call session/load (Prime Agent has no such method), got:\n%s", requests)
	}
}

// TestPrimeSessionIDNeverReported pins a fix found in a post-implementation
// audit: primeBackend.Execute uses the real ACP session id internally (for
// session/prompt and session/close), but must never surface it as
// Result.SessionID. The daemon persists a completed task's Result.SessionID
// as the next related task's ExecOptions.ResumeSessionID AND separately keys
// TaskContextForEnv.PriorSessionResumed / ExecOptions.ResumeExpected purely
// off task.PriorSessionID != "" (daemon.go:5687,6236) — independent of
// whether the backend would ever act on it. Since Prime never resumes
// anything (TestPrimeNeverAttemptsResume), a non-empty Result.SessionID here
// would make the daemon believe a continuation was expected on every
// follow-up turn and log/behave accordingly, even though every Prime turn is
// a cold start. Returning "" keeps that fact visible instead of implying
// continuity that does not exist.
func TestPrimeSessionIDNeverReported(t *testing.T) {
	t.Parallel()
	bin := writeFakePrimeScript(t, fakePrimeACPScript())

	b, err := New("prime", Config{ExecutablePath: bin, Logger: testLogger()})
	if err != nil {
		t.Fatalf("New(prime) error: %v", err)
	}

	ctx := context.Background()
	session, err := b.Execute(ctx, "test prompt", ExecOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	for range session.Messages {
	}

	result := <-session.Result
	if result.Status != "completed" {
		t.Fatalf("expected completed, got status=%q error=%q", result.Status, result.Error)
	}
	if result.SessionID != "" {
		t.Fatalf("Result.SessionID must always be empty for prime (got %q) — a non-empty value "+
			"would make the daemon treat the next related task as a resume, which Prime never honors",
			result.SessionID)
	}
}

// TestPrimeCallsSessionClose verifies primeBackend calls session/close on a
// successful turn. Unlike most ACP backends in this package (which rely on
// transport teardown alone because their agent doesn't implement it), Prime
// Agent's ACP mode does implement session/close (sessionCapabilities.close is
// advertised in initialize), so calling it is the correct, idiomatic
// teardown rather than relying only on stdin EOF.
func TestPrimeCallsSessionClose(t *testing.T) {
	t.Parallel()
	bin := writeFakePrimeScript(t, fakePrimeACPScript())
	reqFile := filepath.Join(t.TempDir(), "requests.txt")

	b, err := New("prime", Config{
		ExecutablePath: bin,
		Logger:         testLogger(),
		Env:            map[string]string{"PRIME_REQUESTS_FILE": reqFile},
	})
	if err != nil {
		t.Fatalf("New(prime) error: %v", err)
	}

	ctx := context.Background()
	session, err := b.Execute(ctx, "test prompt", ExecOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	for range session.Messages {
	}
	<-session.Result

	raw, err := os.ReadFile(reqFile)
	if err != nil {
		t.Fatalf("read requests file: %v", err)
	}
	requests := string(raw)
	if !strings.Contains(requests, `"method":"session/close"`) {
		t.Fatalf("expected session/close to be called, got requests:\n%s", requests)
	}
	if !strings.Contains(requests, `"sessionId":"ses_prime_new"`) {
		t.Fatalf("expected session/close to reference the created sessionId, got requests:\n%s", requests)
	}
}

// TestPrimeSessionNewSendsEmptyMcpServers pins a regression found by a real
// prime-agent v0.7.1 smoke test: the ACP SDK's session/new request schema
// requires an `mcpServers` field to be present (rejecting the request with
// "-32602 Invalid params: mcpServers Required value is missing" otherwise),
// even though Prime's own handler never reads its contents. Phase 1 does not
// implement MCP injection for Prime, so this must always be an empty array,
// never populated from ExecOptions.McpConfig.
func TestPrimeSessionNewSendsEmptyMcpServers(t *testing.T) {
	t.Parallel()
	bin := writeFakePrimeScript(t, fakePrimeACPScript())
	reqFile := filepath.Join(t.TempDir(), "requests.txt")

	b, err := New("prime", Config{
		ExecutablePath: bin,
		Logger:         testLogger(),
		Env:            map[string]string{"PRIME_REQUESTS_FILE": reqFile},
	})
	if err != nil {
		t.Fatalf("New(prime) error: %v", err)
	}

	ctx := context.Background()
	session, err := b.Execute(ctx, "test prompt", ExecOptions{
		Cwd: t.TempDir(),
		// McpConfig set on purpose: it must NOT leak into the mcpServers
		// array sent to Prime — Phase 1 does not implement MCP for Prime.
		McpConfig: []byte(`{"some-server":{"command":"whatever"}}`),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	for range session.Messages {
	}
	<-session.Result

	raw, err := os.ReadFile(reqFile)
	if err != nil {
		t.Fatalf("read requests file: %v", err)
	}
	requests := string(raw)

	if !strings.Contains(requests, `"method":"session/new"`) {
		t.Fatalf("expected a session/new request, got:\n%s", requests)
	}
	if !strings.Contains(requests, `"mcpServers":[]`) {
		t.Fatalf("expected session/new to send an empty mcpServers array, got:\n%s", requests)
	}
	if strings.Contains(requests, "some-server") {
		t.Fatalf("ExecOptions.McpConfig must never reach Prime's session/new (Phase 1 does not implement MCP for this provider), got:\n%s", requests)
	}
}

func TestPrimeBlockedArgs(t *testing.T) {
	t.Parallel()
	if mode, ok := primeBlockedArgs["--mode"]; !ok || mode != blockedWithValue {
		t.Fatalf("expected --mode to be blockedWithValue in primeBlockedArgs, got %v (present=%v)", mode, ok)
	}
	if mode, ok := primeBlockedArgs["--cwd"]; !ok || mode != blockedWithValue {
		t.Fatalf("expected --cwd to be blockedWithValue in primeBlockedArgs, got %v (present=%v)", mode, ok)
	}
}

// TestPrimeBlockedModeAndCwdArgs verifies user-defined --mode/--cwd in
// custom_args cannot override the daemon-controlled ACP transport mode or
// working directory (cmd.Dir is the sole source of truth for cwd — see the
// primeBackend doc comment).
func TestPrimeBlockedModeAndCwdArgs(t *testing.T) {
	t.Parallel()

	argsFile := filepath.Join(t.TempDir(), "args.txt")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" > "%s"
while IFS= read -r line; do
  id=$(printf '%%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":false}}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"sessionId":"ses_prime_blocked"}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"stopReason":"end_turn","usage":{"inputTokens":5,"outputTokens":10}}}\n' "$id"
      ;;
    *'"method":"session/close"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%%s,"error":{"code":-32601,"message":"method not found"}}\n' "$id"
      ;;
  esac
done
`, argsFile)

	bin := writeFakePrimeScript(t, script)

	b, err := New("prime", Config{ExecutablePath: bin, Logger: testLogger()})
	if err != nil {
		t.Fatalf("New(prime) error: %v", err)
	}

	ctx := context.Background()
	session, err := b.Execute(ctx, "test prompt", ExecOptions{
		Cwd:        t.TempDir(),
		CustomArgs: []string{"--mode", "text", "--cwd", "/evil/path"},
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	for range session.Messages {
	}
	<-session.Result

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	args := string(raw)

	if strings.Contains(args, "/evil/path") {
		t.Fatal("user-defined --cwd value should be blocked")
	}
	if strings.Contains(args, "text") {
		t.Fatal("user-defined --mode value should be blocked")
	}
	if !strings.Contains(args, "--mode acp") {
		t.Fatalf("expected daemon-controlled --mode acp in command args, got:\n%s", args)
	}
}

// TestPrimeTimeout tests that a context timeout during session/new is
// reported as status=timeout. The fake script responds to initialize
// immediately, then sleeps 30s on session/new so the 5s context deadline
// expires during the session/new RPC.
func TestPrimeTimeout(t *testing.T) {
	t.Parallel()

	script := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":false}}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      sleep 30
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"ses_late"}}\n' "$id"
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}\n' "$id"
      ;;
  esac
done`

	bin := writeFakePrimeScript(t, script)

	b, err := New("prime", Config{ExecutablePath: bin, Logger: testLogger()})
	if err != nil {
		t.Fatalf("New(prime) error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := b.Execute(ctx, "test prompt", ExecOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	for range session.Messages {
	}

	result := <-session.Result
	if result.Status != "timeout" {
		t.Fatalf("expected timeout, got status=%q error=%q", result.Status, result.Error)
	}
}

func TestPrimeBackendUsage(t *testing.T) {
	t.Parallel()
	bin := writeFakePrimeScript(t, fakePrimeACPScript())

	b, err := New("prime", Config{ExecutablePath: bin, Logger: testLogger()})
	if err != nil {
		t.Fatalf("New(prime) error: %v", err)
	}

	ctx := context.Background()
	session, err := b.Execute(ctx, "test prompt", ExecOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	for range session.Messages {
	}

	result := <-session.Result
	if result.Usage == nil {
		t.Fatal("expected usage in result")
	}
	usage, ok := result.Usage["unknown"]
	if !ok {
		t.Fatalf("expected usage entry for model 'unknown', got %+v", result.Usage)
	}
	want := TokenUsage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 3, CacheWriteTokens: 2, CostUSDTicks: 900}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}
}

// TestPrimeUsageModelIgnored verifies that opts.Model is never used as the
// usage attribution key — Prime's model selection is unsupported over ACP
// (ModelSelectionSupported("prime") is false), so usage must always be
// attributed to "unknown".
func TestPrimeUsageModelIgnored(t *testing.T) {
	t.Parallel()
	bin := writeFakePrimeScript(t, fakePrimeACPScript())

	b, err := New("prime", Config{ExecutablePath: bin, Logger: testLogger()})
	if err != nil {
		t.Fatalf("New(prime) error: %v", err)
	}

	ctx := context.Background()
	session, err := b.Execute(ctx, "test prompt", ExecOptions{
		Cwd:   t.TempDir(),
		Model: "must-not-be-reported",
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	for range session.Messages {
	}

	result := <-session.Result
	if _, ok := result.Usage["must-not-be-reported"]; ok {
		t.Fatal("opts.Model must not be used as usage attribution key for prime")
	}
	if _, ok := result.Usage["unknown"]; !ok {
		t.Fatalf("expected usage entry for model 'unknown', got %+v", result.Usage)
	}
}

// TestPrimeExtraArgsReachTheCommandLine pins MULTICA_PRIME_ARGS end to end:
// config.go reads it, daemon.go forwards it as ExecOptions.ExtraArgs, and
// ExtraArgs must land before CustomArgs, matching the precedence documented
// for every other backend that accepts both.
func TestPrimeExtraArgsReachTheCommandLine(t *testing.T) {
	t.Parallel()

	argsFile := filepath.Join(t.TempDir(), "args.txt")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" > "%s"
while IFS= read -r line; do
  id=$(printf '%%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":false}}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"sessionId":"ses_prime_extra"}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"stopReason":"end_turn","usage":{"inputTokens":5,"outputTokens":10}}}\n' "$id"
      ;;
    *'"method":"session/close"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%%s,"error":{"code":-32601,"message":"method not found"}}\n' "$id"
      ;;
  esac
done
`, argsFile)

	bin := writeFakePrimeScript(t, script)

	b, err := New("prime", Config{ExecutablePath: bin, Logger: testLogger()})
	if err != nil {
		t.Fatalf("New(prime) error: %v", err)
	}

	ctx := context.Background()
	session, err := b.Execute(ctx, "test prompt", ExecOptions{
		Cwd:        t.TempDir(),
		ExtraArgs:  []string{"--daemon-wide"},
		CustomArgs: []string{"--per-agent"},
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	for range session.Messages {
	}
	<-session.Result

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	args := string(raw)

	if !strings.Contains(args, "--daemon-wide") {
		t.Fatalf("expected ExtraArgs in command args, got:\n%s", args)
	}
	extra := strings.Index(args, "--daemon-wide")
	custom := strings.Index(args, "--per-agent")
	if custom < 0 {
		t.Fatalf("expected CustomArgs in command args, got:\n%s", args)
	}
	if extra > custom {
		t.Fatalf("ExtraArgs must precede CustomArgs, got:\n%s", args)
	}
}

// TestPrimeSetsRlmMaxDepthZero pins the blocker-1 fix: Prime's IPython-hosted
// rlm.run tool can spawn a fire-and-forget subagent that keeps streaming
// after session/prompt returns, which ACP has no RPC to wait for. Phase 1
// disables the capability entirely rather than tracking it, by forcing
// RLM_MAX_DEPTH=0 into the spawned process's environment — verified against
// prime-agent v0.7.1 source to be the sole gate _startRlmChildRun checks
// before creating a child (see the primeBackend doc comment). This test
// proves the env var actually reaches the child process.
func TestPrimeSetsRlmMaxDepthZero(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	captureFile := filepath.Join(tempDir, "env-capture.txt")
	script := "#!/bin/sh\nenv > \"$PRIME_ENV_CAPTURE_FILE\"\n" + fakePrimeACPScriptBody()
	bin := writeFakePrimeScript(t, script)

	b, err := New("prime", Config{
		ExecutablePath: bin,
		Logger:         testLogger(),
		Env:            map[string]string{"PRIME_ENV_CAPTURE_FILE": captureFile},
	})
	if err != nil {
		t.Fatalf("New(prime) error: %v", err)
	}

	ctx := context.Background()
	session, err := b.Execute(ctx, "test prompt", ExecOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	for range session.Messages {
	}
	result := <-session.Result
	if result.Status != "completed" {
		t.Fatalf("expected completed, got status=%q error=%q", result.Status, result.Error)
	}

	captured := readCapturedEnv(t, captureFile)
	if got, ok := captured["RLM_MAX_DEPTH"]; !ok || got != "0" {
		t.Fatalf("expected RLM_MAX_DEPTH=0 in the spawned process env, got %q (present=%v)", got, ok)
	}
}

// TestPrimeListModels pins a fix found in a post-implementation audit:
// ListModels("prime") used to fall through to the switch's default case and
// return an "unknown agent type" error, even though
// ModelSelectionSupported("prime") is false — exactly the same situation
// QwenPaw is already in, and the model-picker UI/API relies on
// ListModels not erroring for such providers. Mirrors
// TestQwenpawListModels: points at a real, executable fake that records its
// own invocation, since a non-existent path cannot prove ListModels never
// spawns a discovery subprocess (a missing binary would also silently
// produce an empty catalog for the wrong reason).
func TestPrimeListModels(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "invoked")
	bin := writeFakePrimeScript(t, "#!/bin/sh\ntouch '"+marker+"'\nexit 0\n")

	cat, err := ListModels(context.Background(), "prime", Command{Path: bin})
	if err != nil {
		t.Fatalf("prime ListModels should not error, got: %v", err)
	}
	if len(cat.Models) != 0 {
		t.Fatalf("prime ListModels should return empty catalog, got %d models", len(cat.Models))
	}
	if cat.Fallback {
		t.Error("prime's empty catalog is deliberate, not a discovery fallback")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("prime ListModels executed the CLI; it must return an empty catalog without spawning a discovery subprocess")
	}
}

// TestPrimeExecutableNotFound mirrors the "provider not installed" path
// every other backend's Execute exercises via exec.LookPath.
func TestPrimeExecutableNotFound(t *testing.T) {
	t.Parallel()
	b, err := New("prime", Config{ExecutablePath: "/nonexistent/prime-agent", Logger: testLogger()})
	if err != nil {
		t.Fatalf("New(prime) error: %v", err)
	}
	if _, err := b.Execute(context.Background(), "test prompt", ExecOptions{Cwd: t.TempDir()}); err == nil {
		t.Fatal("expected an error when prime-agent is not installed")
	}
}

// TestPrimeSessionNewMalformedResponse verifies a session/new response with
// no sessionId is treated as a failure rather than silently proceeding with
// an empty session id.
func TestPrimeSessionNewMalformedResponse(t *testing.T) {
	t.Parallel()

	script := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":false}}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}\n' "$id"
      ;;
  esac
done
`
	bin := writeFakePrimeScript(t, script)

	b, err := New("prime", Config{ExecutablePath: bin, Logger: testLogger()})
	if err != nil {
		t.Fatalf("New(prime) error: %v", err)
	}

	ctx := context.Background()
	session, err := b.Execute(ctx, "test prompt", ExecOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	for range session.Messages {
	}

	result := <-session.Result
	if result.Status != "failed" {
		t.Fatalf("expected failed, got status=%q", result.Status)
	}
	if !strings.Contains(result.Error, "no session ID") {
		t.Fatalf("expected 'no session ID' error, got %q", result.Error)
	}
}
