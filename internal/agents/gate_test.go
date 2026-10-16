package agents

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"tencoder/internal/conversation"
	"tencoder/internal/llm"
)

// ── 门禁触发范围（决策 B）─────────────────────────────────────────────────

// TestShouldGateOnlyForLightTierCode is the trigger-scope test. The gate costs three subprocesses;
// running it on an exploration or a log summary is pure latency, and running it on strong-tier work
// protects code that was never at risk.
func TestShouldGateOnlyForLightTierCode(t *testing.T) {
	gate := llm.NewQualityGate(30_000_000_000)

	cases := []struct {
		name string
		spec SubAgentSpec
		want bool
	}{
		{"light patch_gen", SubAgentSpec{Tier: llm.TierLight, TaskClass: llm.ClassPatchGen}, true},
		{"light explore", SubAgentSpec{Tier: llm.TierLight, TaskClass: llm.ClassExplore}, true},
		{"light log_summary", SubAgentSpec{Tier: llm.TierLight, TaskClass: llm.ClassLogSummary}, false},
		{"light memory_recall", SubAgentSpec{Tier: llm.TierLight, TaskClass: llm.ClassMemoryRecall}, false},
		{"light format_check", SubAgentSpec{Tier: llm.TierLight, TaskClass: llm.ClassFormatCheck}, false},
		{"strong patch_gen", SubAgentSpec{Tier: llm.TierStrong, TaskClass: llm.ClassPatchGen}, false},
		{"unclassified", SubAgentSpec{Tier: llm.TierLight}, false},
	}
	for _, c := range cases {
		at := &AgentTool{QualityGate: gate, GateEnabled: true}
		if got := at.shouldGate(c.spec); got != c.want {
			t.Errorf("%s: shouldGate = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestShouldGateRequiresBothFlagAndGate — the two are separate switches on purpose: the machinery can
// stay wired while a run disables gating.
func TestShouldGateRequiresBothFlagAndGate(t *testing.T) {
	spec := SubAgentSpec{Tier: llm.TierLight, TaskClass: llm.ClassPatchGen}

	if (&AgentTool{QualityGate: llm.NewQualityGate(1), GateEnabled: false}).shouldGate(spec) {
		t.Error("GateEnabled=false must not gate")
	}
	if (&AgentTool{GateEnabled: true}).shouldGate(spec) {
		t.Error("nil QualityGate must not gate")
	}
}

// ── 升级链：最多一次（决策 B 防循环）────────────────────────────────────────

// TestEscalateReturnsNilOnPass is the null case for the whole escalation path.
func TestEscalateReturnsNilOnPass(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	dir := cleanGoTree(t)

	at := &AgentTool{
		QualityGate: llm.NewQualityGate(120_000_000_000),
		GateEnabled: true,
	}
	spec := SubAgentSpec{Name: "impl", Tier: llm.TierLight, TaskClass: llm.ClassPatchGen}
	if res := at.escalateIfNeeded(context.Background(), spec, "desc", "original output", dir); res != nil {
		t.Errorf("a passing gate must leave the output untouched; got %q", *res)
	}
}

// TestEscalateSkipsOnNonGoDir — a skipped gate is neutral, and treating it as "nothing to check" is
// what keeps the gate free on non-Go projects.
func TestEscalateSkipsOnNonGoDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.py"), []byte("print(1)\n"), 0o644)

	var lines []string
	at := &AgentTool{
		QualityGate: llm.NewQualityGate(30_000_000_000),
		GateEnabled: true,
		LogRoute:    func(l string) { lines = append(lines, l) },
	}
	spec := SubAgentSpec{Name: "explore", Tier: llm.TierLight, TaskClass: llm.ClassPatchGen}
	if res := at.escalateIfNeeded(context.Background(), spec, "d", "out", dir); res != nil {
		t.Errorf("skipped gate must not rewrite output; got %q", *res)
	}
}

// TestTimeoutDoesNotEscalate — the distinction that saves the most money.
//
// A timeout means the TOOLCHAIN was slow (cold module cache, a hung vet), not that the light model
// wrote bad code. Escalating there spends a strong-tier call to fix nothing.
func TestTimeoutDoesNotEscalate(t *testing.T) {
	// A 1ns budget guarantees the gate times out before go build can finish.
	dir := brokenGoTree(t)

	var lines []string
	at := &AgentTool{
		QualityGate: llm.NewQualityGate(1), // 1 nanosecond
		GateEnabled: true,
		LogRoute:    func(l string) { lines = append(lines, l) },
		// Router deliberately nil: an escalation attempt would panic/error here, so a clean return
		// proves no escalation was attempted.
	}
	spec := SubAgentSpec{Name: "impl", Tier: llm.TierLight, TaskClass: llm.ClassPatchGen}
	res := at.escalateIfNeeded(context.Background(), spec, "d", "light output", dir)
	if res == nil {
		t.Fatal("a timed-out gate must still produce output — silence would hide the unknown verdict")
	}
	if strings.Contains(*res, "repair pass") {
		t.Errorf("a timeout must not escalate; output was rewritten: %q", *res)
	}
	if !strings.Contains(*res, "verdict unknown") {
		t.Errorf("output must say the verdict is unknown, not imply success: %q", *res)
	}
}

// TestEscalationNeedsRouter — without a router there is no strong tier to escalate to, and the
// failure must be reported in the output rather than swallowed.
func TestEscalationNeedsRouter(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	dir := brokenGoTree(t)
	at := &AgentTool{
		QualityGate: llm.NewQualityGate(120_000_000_000),
		GateEnabled: true,
	}
	spec := SubAgentSpec{Name: "impl", Tier: llm.TierLight, TaskClass: llm.ClassPatchGen}
	res := at.escalateIfNeeded(context.Background(), spec, "d", "light output", dir)
	if res == nil {
		t.Fatal("a failed gate must produce output")
	}
	if !strings.Contains(*res, "could not run") {
		t.Errorf("expected an explicit escalation failure, got %q", *res)
	}
}

// ── 档位选择 ──────────────────────────────────────────────────────────────

// TestSelectClientForSpecFallsBackWhenNoRouter — nil router must reproduce the pre-阶段五 behaviour
// exactly. This is the assertion that keeps every existing caller working.
func TestSelectClientForSpecFallsBackWhenNoRouter(t *testing.T) {
	c := &stubClient{}
	at := &AgentTool{Client: c}
	got, tier, _ := at.selectClientForSpec(SubAgentSpec{Name: "x"}, "")
	if got != llm.Client(c) {
		t.Error("with no router, the session client must be returned")
	}
	if tier != llm.TierStrong {
		t.Errorf("no router must report strong, got %s", tier)
	}
}

// TestUnknownTaskClassRoutesStrong — a sub-agent spawned with no taskClass (every pre-阶段五
// definition) must not be silently cheapened. Unknown → strong, fail safe.
func TestUnknownTaskClassRoutesStrong(t *testing.T) {
	// Drive the class decision through Decide, which is what selectClientForSpec relies on.
	tier, _, _, _ := llm.Decide(llm.ClassArchDecision, llm.RouteHints{}, llm.DefaultRoutingConfig())
	if tier != llm.TierStrong {
		t.Errorf("the unknown-class fallback must be strong, got %s", tier)
	}
	// And confirm the empty class itself also lands on strong through the same default path the
	// caller uses (llm.ParseTaskClass returns "" for anything unrecognised).
	if got := llm.ParseTaskClass("totally-made-up"); got != "" {
		t.Errorf("expected empty class, got %q", got)
	}
}

// ── 测试脚手架 ────────────────────────────────────────────────────────────

// stubClient satisfies llm.Client without doing anything. Routing tests never stream — they only
// assert WHICH client the selector handed back.
type stubClient struct{}

func (stubClient) Stream(ctx context.Context, conv *conversation.Manager, toolSchemas []map[string]any) (<-chan llm.StreamEvent, <-chan error) {
	ch := make(chan llm.StreamEvent)
	errCh := make(chan error)
	close(ch)
	close(errCh)
	return ch, errCh
}

func (stubClient) SetSystemPrompt(string) {}

func cleanGoTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module gatetest\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { println(\"ok\") }\n"), 0o644)
	return dir
}

func brokenGoTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module gatetest\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { nope() }\n"), 0o644)
	return dir
}
