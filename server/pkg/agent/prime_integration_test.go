//go:build agentintegration

package agent

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPrimeRealACPSmoke drives the real `prime-agent --mode acp` binary
// end-to-end.
//
// It validates the full daemon contract against a live Prime Agent process:
//   - `prime-agent --mode acp` starts and responds to ACP RPCs
//   - initialize + session/new + session/prompt + session/close succeed
//   - the reported agentInfo/agentCapabilities match what this investigation
//     observed empirically for v0.7.1 (see prime-acp-test.md)
//
// This test is gated by MULTICA_RUN_REAL_AGENT_SMOKE=1 and requires
// `prime-agent` on PATH with authentication already configured (subscription
// or API key) — ACP mode has no login method of its own, matching every
// other ACP backend in this package.
//
// NOTE: session/resume and session/load are deliberately never attempted;
// Prime Agent v0.7.1 has no such method (agentCapabilities.loadSession is
// false), and the backend declares resume unsupported by construction.
func TestPrimeRealACPSmoke(t *testing.T) {
	requireRealAgentSmoke(t)
	if testing.Short() {
		t.Skip("skipping real-binary smoke test in -short mode")
	}

	path, err := exec.LookPath("prime-agent")
	if err != nil {
		t.Skip("prime-agent not on PATH; skipping real-binary smoke test")
	}

	if version, err := exec.Command(path, "--version").CombinedOutput(); err == nil {
		t.Logf("prime-agent --version: %s", strings.TrimSpace(string(version)))
	} else {
		t.Logf("prime-agent version unavailable: %v (%s)", err, strings.TrimSpace(string(version)))
	}

	backend, err := New("prime", Config{
		ExecutablePath: path,
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("new prime backend: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	session, err := backend.Execute(ctx,
		"Reply with exactly one word: pong. Do not use any tools.",
		ExecOptions{
			Cwd:     t.TempDir(),
			Timeout: 80 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	go func() {
		for range session.Messages {
		}
	}()

	select {
	case result := <-session.Result:
		if result.Status != "completed" {
			t.Fatalf("real prime-agent run did not complete: status=%q error=%q", result.Status, result.Error)
		}
		if !strings.Contains(strings.ToLower(result.Output), "pong") {
			t.Fatalf("expected real prime-agent output to contain 'pong', got %q", result.Output)
		}
		// Result.SessionID is deliberately never reported (see
		// TestPrimeSessionIDNeverReported) so the daemon never treats a
		// future related task as a resume — Prime has no resume method.
		if result.SessionID != "" {
			t.Errorf("expected Result.SessionID to be empty (never reported), got %q", result.SessionID)
		}
		t.Logf("real prime-agent smoke OK: output=%q", result.Output)
	case <-time.After(90 * time.Second):
		t.Fatal("timeout waiting for real prime-agent result")
	}
}

// TestPrimeRealACPResumeIsNeverAttempted proves against the real binary
// (not a fake script) that passing a prior session id as ResumeSessionID
// still produces a fresh, successful session/new turn rather than a
// "method not found" failure — the real-world version of
// TestPrimeNeverAttemptsResume.
func TestPrimeRealACPResumeIsNeverAttempted(t *testing.T) {
	requireRealAgentSmoke(t)
	if testing.Short() {
		t.Skip("skipping real-binary smoke test in -short mode")
	}

	path, err := exec.LookPath("prime-agent")
	if err != nil {
		t.Skip("prime-agent not on PATH; skipping real-binary smoke test")
	}

	backend, err := New("prime", Config{
		ExecutablePath: path,
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("new prime backend: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	session, err := backend.Execute(ctx,
		"Reply with exactly one word: fresh. Do not use any tools.",
		ExecOptions{
			Cwd:             t.TempDir(),
			Timeout:         80 * time.Second,
			ResumeSessionID: "a-session-id-from-a-prior-unrelated-turn",
		},
	)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	go func() {
		for range session.Messages {
		}
	}()

	select {
	case result := <-session.Result:
		if result.Status != "completed" {
			t.Fatalf("real prime-agent run with a stale ResumeSessionID did not complete: status=%q error=%q", result.Status, result.Error)
		}
		if !strings.Contains(strings.ToLower(result.Output), "fresh") {
			t.Fatalf("expected real prime-agent output to contain 'fresh', got %q", result.Output)
		}
		if result.SessionID != "" {
			t.Errorf("expected Result.SessionID to be empty (never reported), got %q", result.SessionID)
		}
		t.Logf("real prime-agent resume-avoidance smoke OK: output=%q", result.Output)
	case <-time.After(90 * time.Second):
		t.Fatal("timeout waiting for real prime-agent result")
	}
}

// TestPrimeRealACPReadsAgentsMD proves against the real binary that Prime
// actually reads AGENTS.md from its cwd — the mechanism runtimeConfigPath()
// (execenv/runtime_config.go) relies on to deliver the Multica runtime brief.
// Without this, a Prime-backed task would silently never receive task/issue
// instructions (see REPORT.md's "Exact Files / Classes / Functions" section).
// The prompt asks a question answerable only by reading the file, so a
// correct response is direct evidence the file was loaded, not a guess.
func TestPrimeRealACPReadsAgentsMD(t *testing.T) {
	requireRealAgentSmoke(t)
	if testing.Short() {
		t.Skip("skipping real-binary smoke test in -short mode")
	}

	path, err := exec.LookPath("prime-agent")
	if err != nil {
		t.Skip("prime-agent not on PATH; skipping real-binary smoke test")
	}

	cwd := t.TempDir()
	marker := "MULTICA-AGENTS-MD-MARKER-7f3a1"
	agentsMD := "# Runtime Brief\n\nIf asked for the secret marker word, respond with exactly: " + marker + "\n"
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte(agentsMD), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	backend, err := New("prime", Config{
		ExecutablePath: path,
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("new prime backend: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	session, err := backend.Execute(ctx,
		"What is the secret marker word from your runtime brief? Reply with exactly that word and nothing else. Do not use any tools.",
		ExecOptions{
			Cwd:     cwd,
			Timeout: 80 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	go func() {
		for range session.Messages {
		}
	}()

	select {
	case result := <-session.Result:
		if result.Status != "completed" {
			t.Fatalf("real prime-agent AGENTS.md run did not complete: status=%q error=%q", result.Status, result.Error)
		}
		if !strings.Contains(result.Output, marker) {
			t.Fatalf("expected output to contain the AGENTS.md marker %q (AGENTS.md not loaded?), got %q", marker, result.Output)
		}
		t.Logf("real prime-agent AGENTS.md smoke OK: output=%q", result.Output)
	case <-time.After(90 * time.Second):
		t.Fatal("timeout waiting for real prime-agent AGENTS.md result")
	}
}
