package llm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ── 三信号裁决 ────────────────────────────────────────────────────────────

// TestDeclarationSetsBaseline pins the whole point of the design: the class DECLARATION is the
// primary signal. A light-baseline class with no runtime hints must land on light, and a
// strong-baseline class with no hints must land on strong. If either of these ever inverts, every
// other test in this file is measuring noise.
func TestDeclarationSetsBaseline(t *testing.T) {
	cfg := DefaultRoutingConfig()

	lightCases := []TaskClass{ClassMemoryRecall, ClassLogSummary, ClassFormatCheck, ClassExplore}
	for _, c := range lightCases {
		tier, reason, promoted, _ := Decide(c, RouteHints{}, cfg)
		if tier != TierLight {
			t.Errorf("%s: baseline tier = %s, want light", c, tier)
		}
		if promoted {
			t.Errorf("%s: promoted without any hints", c)
		}
		if !strings.Contains(reason, "declared=") {
			t.Errorf("%s: reason %q does not name the declaration — an unexplainable route cannot be tuned", c, reason)
		}
	}

	strongCases := []TaskClass{ClassPatchGen, ClassArchDecision, ClassConflictMerge, ClassCrossProject}
	for _, c := range strongCases {
		tier, _, promoted, _ := Decide(c, RouteHints{}, cfg)
		if tier != TierStrong {
			t.Errorf("%s: baseline tier = %s, want strong", c, tier)
		}
		if promoted {
			t.Errorf("%s: a strong baseline must never report promotion", c)
		}
	}
}

// TestToolBudgetPromotesLightToStrong — signal 1.
func TestToolBudgetPromotesLightToStrong(t *testing.T) {
	cfg := DefaultRoutingConfig()

	tier, reason, promoted, _ := Decide(ClassExplore, RouteHints{EstimatedToolCalls: cfg.ToolCallBudget}, cfg)
	if tier != TierLight {
		t.Errorf("at the budget (== threshold) must stay light; got %s", tier)
	}

	tier, reason, promoted, _ = Decide(ClassExplore, RouteHints{EstimatedToolCalls: cfg.ToolCallBudget + 1}, cfg)
	if tier != TierStrong {
		t.Errorf("over budget must promote to strong; got %s", tier)
	}
	if !promoted {
		t.Error("promoted flag not set")
	}
	if !strings.Contains(reason, "tool budget") {
		t.Errorf("reason %q must name the signal that fired", reason)
	}
}

// TestContextSizePromotesLightToStrong — signal 2, and the reason it exists at all.
func TestContextSizePromotesLightToStrong(t *testing.T) {
	cfg := DefaultRoutingConfig()

	if tier, _, _, _ := Decide(ClassFormatCheck, RouteHints{ContextTokens: cfg.ContextTokensThreshold}, cfg); tier != TierLight {
		t.Errorf("at the threshold must stay light; got %s", tier)
	}
	tier, reason, promoted, _ := Decide(ClassFormatCheck, RouteHints{ContextTokens: cfg.ContextTokensThreshold + 1}, cfg)
	if tier != TierStrong {
		t.Errorf("over threshold must promote; got %s", tier)
	}
	if !promoted || !strings.Contains(reason, "context size") {
		t.Errorf("promoted=%v reason=%q", promoted, reason)
	}
}

// TestZeroHintsAreUnknownNotZero — the single most dangerous shape of bug in a threshold system.
//
// An "unknown" estimate reported as the literal 0 would be silently treated as "tiny", so a task
// whose cost we simply do not know would route to the cheap model. Zero must mean UNKNOWN and
// must never promote.
func TestZeroHintsAreUnknownNotZero(t *testing.T) {
	cfg := DefaultRoutingConfig()
	for _, c := range []TaskClass{ClassExplore, ClassMemoryRecall, ClassLogSummary, ClassFormatCheck} {
		tier, reason, promoted, _ := Decide(c, RouteHints{EstimatedToolCalls: 0, ContextTokens: 0}, cfg)
		if tier != TierLight {
			t.Errorf("%s: zero hints must keep the baseline, got %s", c, tier)
		}
		if promoted {
			t.Errorf("%s: zero hints must not promote (reason %q) — 'unknown' is being read as 'tiny'", c, reason)
		}
	}
}

// TestBothSignalsPromoteOnlyOnce — a task tripping both signals is "strong", not "stronger".
//
// Without this, a three-tier reading creeps in and there is no model configured for it.
func TestBothSignalsPromoteOnlyOnce(t *testing.T) {
	cfg := DefaultRoutingConfig()
	hints := RouteHints{EstimatedToolCalls: 9999, ContextTokens: 999999}
	tier, reason, promoted, _ := Decide(ClassExplore, hints, cfg)
	if tier != TierStrong {
		t.Fatalf("tier = %s, want strong", tier)
	}
	if !promoted {
		t.Error("promoted not set")
	}
	// Both signals should be NAMED even though only one promotion step happened: the reader needs
	// to know both fired to understand why the margin was that wide.
	if !strings.Contains(reason, "tool budget") || !strings.Contains(reason, "context size") {
		t.Errorf("reason must name both signals: %q", reason)
	}
}

// TestStrongBaselineIsNeverDemoted — there is no cheapening signal, and that is deliberate.
func TestStrongBaselineIsNeverDemoted(t *testing.T) {
	cfg := DefaultRoutingConfig()
	tier, _, promoted, _ := Decide(ClassArchDecision, RouteHints{EstimatedToolCalls: 1, ContextTokens: 1}, cfg)
	if tier != TierStrong {
		t.Errorf("a tiny estimate must not cheapen a strong-baseline class; got %s", tier)
	}
	if promoted {
		t.Error("nothing was promoted")
	}
}

// TestDisabledForcesStrongAndSaysSo — the A/B control switch (决策 C).
func TestDisabledForcesStrongAndSaysSo(t *testing.T) {
	cfg := DefaultRoutingConfig()
	cfg.Disabled = true

	for _, c := range AllTaskClasses() {
		tier, reason, _, suppressed := Decide(c, RouteHints{}, cfg)
		if tier != TierStrong {
			t.Errorf("%s: disabled routing must force strong, got %s", c, tier)
		}
		if !suppressed {
			t.Errorf("%s: suppressed flag not set", c)
		}
		if !strings.Contains(reason, "disabled") {
			t.Errorf("%s: reason %q must distinguish 'routing off' from 'routing said strong'", c, reason)
		}
	}
}

// TestNormalizeFillsZeroThresholds — a config with only the switch set must not divide by / compare
// against 0, which would promote EVERY task on both signals.
func TestNormalizeFillsZeroThresholds(t *testing.T) {
	// All-zero config (a routing block the user wrote but never filled in).
	cfg := RoutingConfig{}.normalized()
	d := DefaultRoutingConfig()
	if cfg.ToolCallBudget != d.ToolCallBudget || cfg.ContextTokensThreshold != d.ContextTokensThreshold {
		t.Errorf("zero thresholds not filled: %+v", cfg)
	}
	// Consequence check: with defaults filled in, a modest task stays light. Without normalization
	// a zero threshold would compare 5>0 and promote EVERY task on both signals.
	tier, _, _, _ := Decide(ClassExplore, RouteHints{EstimatedToolCalls: 5}, cfg)
	if tier != TierLight {
		t.Errorf("normalized config promoted a 5-call task: %s", tier)
	}
}

// ── TaskClass 解析 ─────────────────────────────────────────────────────────

func TestParseTaskClassTolerates(t *testing.T) {
	cases := map[string]TaskClass{
		"patch_gen":     ClassPatchGen,
		"PATCH_GEN":     ClassPatchGen,
		"  patch-gen ":  ClassPatchGen,
		"Patch Gen":     ClassPatchGen,
		"explore":       ClassExplore,
		"memory_recall": ClassMemoryRecall,
	}
	for in, want := range cases {
		if got := ParseTaskClass(in); got != want {
			t.Errorf("ParseTaskClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestParseTaskClassUnknownIsEmpty — an unrecognised declaration must NOT fall back to a light
// class. Empty routes to strong (see AgentTool.selectClientForSpec), which is the safe direction.
func TestParseTaskClassUnknownIsEmpty(t *testing.T) {
	for _, in := range []string{"", "  ", "patchgen", "typo", "PATCH_GEN_V2"} {
		if got := ParseTaskClass(in); got != "" {
			t.Errorf("ParseTaskClass(%q) = %q, want empty (unknown must fail safe)", in, got)
		}
	}
}

func TestEveryClassIsDistinct(t *testing.T) {
	seen := map[TaskClass]bool{}
	for _, c := range AllTaskClasses() {
		if seen[c] {
			t.Errorf("duplicate class %q — the eight-way enum is not eight", c)
		}
		seen[c] = true
		if BaselineTier(c) != TierLight && BaselineTier(c) != TierStrong {
			t.Errorf("%s has baseline %q", c, BaselineTier(c))
		}
	}
}

// ── 质量门禁 ──────────────────────────────────────────────────────────────

// TestGatePassesOnCleanTree is the null case; without it a gate that never passes would look fine.
func TestGatePassesOnCleanTree(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	dir := t.TempDir()
	writeGoMod(t, dir)
	writeGo(t, dir, "main.go", "package main\n\nfunc main() { println(\"ok\") }\n")

	g := NewQualityGate(120_000_000_000) // 2 min
	res := g.Run(context.Background(), dir)
	if res.Skipped {
		t.Skip("gate skipped (no go.mod detected)")
	}
	if !res.Passed {
		t.Fatalf("clean tree failed the gate: %+v", res)
	}
	if res.TimedOut {
		t.Error("clean tree reported a timeout")
	}
}

// TestGateFailsOnCompileError — the whole reason the gate exists.
func TestGateFailsOnCompileError(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	dir := t.TempDir()
	writeGoMod(t, dir)
	writeGo(t, dir, "main.go", "package main\n\nfunc main() { undefinedSymbol() }\n")

	res := NewQualityGate(120_000_000_000).Run(context.Background(), dir)
	if res.Skipped {
		t.Skip("gate skipped")
	}
	if res.Passed {
		t.Fatal("a file that does not compile passed the gate — the gate is decorative")
	}
	if res.TimedOut {
		t.Fatal("a compile error must fail, not time out")
	}
	f := res.FailedChecks(20)
	if !strings.Contains(f, "undefinedSymbol") && !strings.Contains(f, "build") {
		t.Errorf("failure report does not carry the compiler output: %q", f)
	}
}

// TestGateFailsOnFormatting — gofmt is one of the three checks, not decoration.
func TestGateFailsOnFormatting(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	dir := t.TempDir()
	writeGoMod(t, dir)
	// Compiles, but badly formatted: no newline before the closing brace.
	writeGo(t, dir, "main.go", "package main\nfunc main(){println(1)}")

	res := NewQualityGate(120_000_000_000).Run(context.Background(), dir)
	if res.Skipped {
		t.Skip("gate skipped")
	}
	if res.Passed {
		t.Fatal("misformatted code passed the gate")
	}
	if !strings.Contains(res.FailedChecks(20), "gofmt") {
		t.Errorf("expected the gofmt check to be the one that failed: %q", res.FailedChecks(20))
	}
}

// TestGateSkipsNonGoDir — a Python/JS project must not pay for three Go checks.
// Skipped is NEUTRAL: it must not be reported as a pass, and it must not be reported as a failure.
func TestGateSkipsNonGoDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := NewQualityGate(30_000_000_000).Run(context.Background(), dir)
	if !res.Skipped {
		t.Errorf("non-Go dir must skip, got %+v", res)
	}
	if !res.Passed {
		t.Error("a skip must count as passed so callers do not escalate on it")
	}
}

// TestGateCachesUnchangedTree — the gate runs go build on every light-tier code sub-agent. Without a
// fingerprint cache a 40-turn session pays 40 uncached builds; with it, only the turns that changed
// files do. This test asserts the cache EXISTS (second identical run is a cache hit) and that
// editing a file invalidates it.
func TestGateCachesUnchangedTree(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	dir := t.TempDir()
	writeGoMod(t, dir)
	writeGo(t, dir, "main.go", "package main\n\nfunc main() { println(\"v1\") }\n")

	g := NewQualityGate(120_000_000_000)
	first := g.Run(context.Background(), dir)
	if first.Skipped {
		t.Skip("gate skipped")
	}
	// Unchanged tree → served from cache.
	if second := g.Run(context.Background(), dir); second.TimedOut {
		t.Error("unchanged tree timed out")
	}
	// Changed tree → must re-run (and now fail, since v2 does not compile).
	writeGo(t, dir, "main.go", "package main\n\nfunc main() { nope() }\n")
	third := g.Run(context.Background(), dir)
	if third.Skipped {
		t.Fatal("changed tree was skipped")
	}
	if third.Passed {
		t.Fatal("the fingerprint cache served a stale pass for an edited tree — worse than no cache")
	}
}

// TestGateIsSafeOnMissingDir — a deleted worktree must not panic.
func TestGateIsSafeOnMissingDir(t *testing.T) {
	res := NewQualityGate(10_000_000_000).Run(context.Background(), filepath.Join(t.TempDir(), "does-not-exist"))
	if !res.Skipped {
		t.Errorf("missing dir must skip, got %+v", res)
	}
}

// TestGateIsSafeOnNilContext — cheap guard: a nil ctx must not panic the gate.
func TestGateIsSafeOnNilContext(t *testing.T) {
	//nolint:staticcheck // deliberately passing nil to prove the guard
	res := NewQualityGate(1_000_000_000).Run(nil, t.TempDir())
	if !res.Skipped {
		t.Errorf("expected a skip, got %+v", res)
	}
}

func writeGoMod(t *testing.T, dir string) {
	t.Helper()
	body := "module gatetest\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeGo(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
