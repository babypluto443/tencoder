// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"tencoder/internal/conversation"
)

// 阶段二反思节点的单测。覆盖：四事件触发检测 / 判定解析 / K=3 处置链 /
// 异步预取的 latest-wins 与过期丢弃 / 目标提取 / 失效静默。

// ── 测试脚手架 ────────────────────────────────────────────────────────────

// scriptedJudge returns a judge that replies with the given verdicts in order, plus a call-count
// getter. The last verdict repeats once the script is exhausted.
//
// The count is exposed as a closure rather than a *int32 + *sync.Mutex pair on purpose: the judge
// runs on a detached goroutine, so the test must read the count through the SAME mutex the judge
// writes with. Handing back a bare counter alongside a mutex invites exactly the race the -race
// detector flags.
func scriptedJudge(replies []string) (ReflectionJudgeFn, func() int32) {
	var mu sync.Mutex
	var calls int32
	i := 0
	fn := func(context.Context, string, string) (string, ReflectionUsage, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		reply := replies[len(replies)-1]
		if i < len(replies) {
			reply = replies[i]
		}
		i++
		return reply, ReflectionUsage{InputTokens: 100, OutputTokens: 20}, nil
	}
	return fn, func() int32 {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

func newTestReflector(judge ReflectionJudgeFn, tweak func(*ReflectionConfig)) *Reflector {
	cfg := DefaultReflectionConfig()
	cfg.MaxWaitMs = 300 // keep the tests fast; production default is 5000
	if tweak != nil {
		tweak(&cfg)
	}
	return NewReflector(judge, cfg)
}

// convWithGoal builds a conversation whose first user message is the task goal.
func convWithGoal(goal string, extra ...string) *conversation.Manager {
	conv := conversation.NewManager()
	conv.AddUserMessage(goal)
	for _, e := range extra {
		conv.AddUserMessage(e)
	}
	return conv
}

// ── 触发事件检测（任务 2.2）────────────────────────────────────────────────

func TestObserveToolsToolFailureTriggers(t *testing.T) {
	r := newTestReflector(scriptedNoopJudge(), nil)
	if got := r.ObserveTools([]string{"Bash"}, true); got != TriggerToolFailure {
		t.Errorf("any errored tool result must trigger, got %q", got)
	}
	// A failing write is both "patch generated" and "tool failure"; the higher-signal one wins.
	if got := r.ObserveTools([]string{"EditFile"}, true); got != TriggerToolFailure {
		t.Errorf("failed write must be attributed to tool_failure, got %q", got)
	}
}

func TestObserveToolsLocalizationDoneAfterReadRun(t *testing.T) {
	r := newTestReflector(scriptedNoopJudge(), nil)
	// 3 read-only tools then a successful write = "finished locating, now acting".
	names := []string{"ReadFile", "Grep", "Glob", "EditFile"}
	if got := r.ObserveTools(names, false); got != TriggerLocalizationDone {
		t.Errorf("read-run then write must trigger localization_done, got %q", got)
	}
}

func TestObserveToolsPatchGeneratedWithoutLongReadRun(t *testing.T) {
	r := newTestReflector(scriptedNoopJudge(), nil)
	// A write with no preceding read run is just a patch.
	if got := r.ObserveTools([]string{"WriteFile"}, false); got != TriggerPatchGenerated {
		t.Errorf("write without read run must trigger patch_generated, got %q", got)
	}
}

func TestObserveToolsLocalizationFiresOnlyOnce(t *testing.T) {
	r := newTestReflector(scriptedNoopJudge(), nil)
	first := r.ObserveTools([]string{"ReadFile", "Grep", "Glob", "EditFile"}, false)
	if first != TriggerLocalizationDone {
		t.Fatalf("first write after read run = %q, want localization_done", first)
	}
	// Subsequent writes in the same locating phase must not re-fire localization_done, otherwise
	// every edit in a long edit sequence would burn a judge call.
	second := r.ObserveTools([]string{"EditFile"}, false)
	if second == TriggerLocalizationDone {
		t.Error("localization_done must not re-fire after the first write")
	}
}

func TestObserveToolsThresholdIsConfigurable(t *testing.T) {
	r := newTestReflector(scriptedNoopJudge(), func(c *ReflectionConfig) {
		c.LocalizationRunThreshold = 1
	})
	if got := r.ObserveTools([]string{"Grep", "EditFile"}, false); got != TriggerLocalizationDone {
		t.Errorf("threshold=1 should trigger on one read, got %q", got)
	}
}

func TestObserveToolsNoTriggerForQuietTurn(t *testing.T) {
	r := newTestReflector(scriptedNoopJudge(), nil)
	if got := r.ObserveTools([]string{"ReadFile"}, false); got != "" {
		t.Errorf("a pure read turn must not trigger, got %q", got)
	}
	if got := r.ObserveTools(nil, false); got != "" {
		t.Errorf("an empty turn must not trigger, got %q", got)
	}
}

func TestObserveToolsDisabledReflectorIsInert(t *testing.T) {
	r := newTestReflector(scriptedNoopJudge(), func(c *ReflectionConfig) { c.Enabled = false })
	if got := r.ObserveTools([]string{"ReadFile", "Grep", "Glob", "EditFile"}, true); got != "" {
		t.Errorf("disabled reflector must never trigger, got %q", got)
	}
}

func TestNotifyPeriodicRespectsConfig(t *testing.T) {
	off := newTestReflector(scriptedNoopJudge(), func(c *ReflectionConfig) { c.Periodic = false })
	if off.NotifyPeriodic(10) {
		t.Error("periodic fallback must be off by default")
	}
	on := newTestReflector(scriptedNoopJudge(), func(c *ReflectionConfig) {
		c.Periodic = true
		c.PeriodicEvery = 5
	})
	if !on.NotifyPeriodic(5) || !on.NotifyPeriodic(10) {
		t.Error("periodic fallback must fire on every Nth turn")
	}
	if on.NotifyPeriodic(7) {
		t.Error("periodic fallback must not fire off-multiple turns")
	}
}

// ── 判定解析（任务 2.1）──────────────────────────────────────────────────

func TestParseReflectionVerdict(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantOK    bool
		onTrack   bool
		deviation string
	}{
		{"plain yes", `{"on_track": true}`, true, true, ""},
		{"fenced yes", "```json\n{\"on_track\": true}\n```", true, true, ""},
		{"prose around json", `Here you go: {"on_track": false, "deviation": "editing an unrelated module"} done`, true, false, "editing an unrelated module"},
		{"no with deviation", `{"on_track": false, "deviation": "refactoring instead of fixing the bug"}`, true, false, "refactoring instead of fixing the bug"},
		// Invalid cases. Each one must be rejected so it cannot touch the K window.
		{"empty", "", false, false, ""},
		{"prose only", "I think the agent looks fine actually.", false, false, ""},
		{"empty object", `{}`, false, false, ""},
		{"no without deviation", `{"on_track": false}`, false, false, ""},
		{"truncated json", `{"on_track": tru`, false, false, ""},
	}
	for _, c := range cases {
		got, ok := parseReflectionVerdict(c.raw)
		if ok != c.wantOK {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.onTrack != c.onTrack {
			t.Errorf("%s: onTrack = %v, want %v", c.name, got.onTrack, c.onTrack)
		}
		if got.deviation != c.deviation {
			t.Errorf("%s: deviation = %q, want %q", c.name, got.deviation, c.deviation)
		}
	}
}

func TestParseReflectionVerdictCollapsesMultilineDeviation(t *testing.T) {
	got, ok := parseReflectionVerdict(`{"on_track": false, "deviation": "line one\nline two"}`)
	if !ok {
		t.Fatal("expected a valid verdict")
	}
	if strings.ContainsAny(got.deviation, "\n\r") {
		t.Errorf("deviation must be a single line, got %q", got.deviation)
	}
}

// ── 异步预取通道（任务 2.1）──────────────────────────────────────────────

func TestFireDoesNotBlockAndConsumeAppliesVerdict(t *testing.T) {
	judge, calls := scriptedJudge([]string{`{"on_track": false, "deviation": "editing unrelated files"}`})
	r := newTestReflector(judge, nil)

	conv := convWithGoal("fix the failing login test")
	conv.AddAssistantMessage("Looking at the login module.")
	conv.AddToolUseMessage("", "t1", "EditFile", map[string]any{"file_path": "internal/auth/login.go"})

	r.Fire(TriggerPatchGenerated, conv) // must return immediately
	r.Consume(conv)

	n := calls()
	if n != 1 {
		t.Errorf("judge called %d times, want 1", n)
	}

	st := r.Stats()
	if st.Reminders != 1 {
		t.Errorf("one No should inject one reminder, got %d", st.Reminders)
	}
	if st.Drift != 1 {
		t.Errorf("drift count = %d, want 1", st.Drift)
	}
	// The reminder must reach the conversation so the next LLM call actually sees it.
	found := false
	for _, m := range conv.GetMessages() {
		if strings.Contains(m.Content, "Original goal:") {
			found = true
		}
	}
	if !found {
		t.Error("drift reminder was not injected into the conversation")
	}
}

func TestConsumeDiscardsVerdictOnTimeout(t *testing.T) {
	// Judge blocks past the consume window.
	block := make(chan struct{})
	judge := func(ctx context.Context, _, _ string) (string, ReflectionUsage, error) {
		select {
		case <-block:
		case <-time.After(2 * time.Second):
		}
		return `{"on_track": false, "deviation": "too late to matter"}`, ReflectionUsage{}, nil
	}
	r := newTestReflector(judge, func(c *ReflectionConfig) { c.MaxWaitMs = 50 })

	conv := convWithGoal("do the thing")
	conv.AddAssistantMessage("working")
	r.Fire(TriggerPatchGenerated, conv)
	start := time.Now()
	r.Consume(conv)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Consume blocked for %s; the window must bound the wait", elapsed)
	}

	st := r.Stats()
	if st.Timeouts != 1 {
		t.Errorf("timeouts = %d, want 1", st.Timeouts)
	}
	if st.Drift != 0 || st.Reminders != 0 {
		t.Errorf("a timed-out verdict must not touch the K window (drift=%d reminders=%d)", st.Drift, st.Reminders)
	}
	close(block)
}

func TestLateVerdictAfterTimeoutIsDropped(t *testing.T) {
	release := make(chan struct{})
	judge := func(context.Context, string, string) (string, ReflectionUsage, error) {
		<-release
		return `{"on_track": false, "deviation": "stale verdict"}`, ReflectionUsage{}, nil
	}
	r := newTestReflector(judge, func(c *ReflectionConfig) { c.MaxWaitMs = 40 })

	conv := convWithGoal("goal")
	conv.AddAssistantMessage("action")
	r.Fire(TriggerPatchGenerated, conv)
	r.Consume(conv) // times out, marks the seq abandoned

	close(release)
	time.Sleep(80 * time.Millisecond) // let the late verdict land in the buffer

	// A second Consume must refuse the abandoned seq rather than apply a stale judgment.
	r.Consume(conv)
	if st := r.Stats(); st.Drift != 0 || st.Reminders != 0 {
		t.Errorf("a late verdict for an abandoned seq must be dropped (drift=%d reminders=%d)", st.Drift, st.Reminders)
	}
}

func TestLatestVerdictOverwritesOlder(t *testing.T) {
	// The out-of-order race this guards against: the OLDER fire's judge call finishes LAST, so
	// naive "last arrival wins" eviction would hand the main loop a judgment about a trajectory it
	// has already moved past.
	//
	// Keyed on the SNAPSHOT CONTENT rather than call index: goroutine scheduling makes "call 1"
	// unreliable (fire #2's goroutine may run first), but the snapshot is deterministic —
	// "state two" only exists in fire #2's trace.
	judge := func(_ context.Context, _, userMsg string) (string, ReflectionUsage, error) {
		if strings.Contains(userMsg, "state two") {
			return `{"on_track": true}`, ReflectionUsage{}, nil // newer judgment, lands first
		}
		time.Sleep(150 * time.Millisecond) // older judgment about "state one", lands last
		return `{"on_track": false, "deviation": "stale verdict about an older trajectory"}`, ReflectionUsage{}, nil
	}
	r := newTestReflector(judge, func(c *ReflectionConfig) { c.MaxWaitMs = 500 })

	conv := convWithGoal("goal")
	conv.AddAssistantMessage("state one")
	r.Fire(TriggerPatchGenerated, conv)

	conv.AddAssistantMessage("state two")
	r.Fire(TriggerToolFailure, conv)

	// Wait past both calls: the buffer now holds only the newer (Yes) verdict.
	time.Sleep(250 * time.Millisecond)
	r.Consume(conv)

	st := r.Stats()
	if st.Drift != 0 || st.Reminders != 0 {
		t.Errorf("the stale No verdict was applied; latest-wins broken (drift=%d reminders=%d)", st.Drift, st.Reminders)
	}
	if st.Overwrites == 0 {
		t.Errorf("expected an eviction to be recorded, got %+v", st)
	}
}

func TestFireSkipsWhenConversationHasNoGoal(t *testing.T) {
	judge, calls := scriptedJudge([]string{`{"on_track": true}`})
	r := newTestReflector(judge, nil)
	// An empty conversation has no goal and no trace — nothing worth a judge round trip.
	r.Fire(TriggerPatchGenerated, conversation.NewManager())
	n := calls()
	if n != 0 {
		t.Errorf("judge must not be called without a goal, got %d calls", n)
	}
}

// ── K=3 处置链（任务 2.3）────────────────────────────────────────────────

func TestThreeConsecutiveNoTriggersReplan(t *testing.T) {
	judge, _ := scriptedJudge([]string{`{"on_track": false, "deviation": "editing unrelated modules"}`})
	r := newTestReflector(judge, nil)

	var drifts []string
	r.OnDrift = func(taskName, output string) {
		drifts = append(drifts, taskName+"\n"+output)
	}

	conv := convWithGoal("fix the three-file test restoration")
	conv.AddAssistantMessage("step")

	for i := 0; i < 3; i++ {
		conv.AddAssistantMessage(fmt.Sprintf("step %d", i))
		r.Fire(TriggerPatchGenerated, conv)
		r.Consume(conv)
	}

	st := r.Stats()
	if st.Replans != 1 {
		t.Fatalf("replans = %d, want exactly 1 after three consecutive No", st.Replans)
	}
	// The window resets after a replan, so a 4th No must not trip it again.
	conv.AddAssistantMessage("after replan")
	r.Fire(TriggerPatchGenerated, conv)
	r.Consume(conv)
	if r.Stats().Replans != 1 {
		t.Error("the K window must reset after a replan")
	}

	if len(drifts) != 1 {
		t.Fatalf("drift sink called %d times, want 1", len(drifts))
	}
	if !strings.HasPrefix(drifts[0], "goal-drift:") {
		t.Errorf("drift task name must be prefixed goal-drift:, got %q", firstLine(drifts[0]))
	}
	if !strings.Contains(drifts[0], "fix the three-file test restoration") {
		t.Error("drift record must carry the goal")
	}

	// The replan instruction must actually reach the conversation.
	replan := false
	for _, m := range conv.GetMessages() {
		if strings.Contains(m.Content, "STOP.") && strings.Contains(m.Content, "Original goal:") {
			replan = true
		}
	}
	if !replan {
		t.Error("replan instruction was not injected")
	}
}

func TestYesResetsDriftWindow(t *testing.T) {
	// No, No, Yes, No, No → five judgments, never three consecutive.
	judge, _ := scriptedJudge([]string{
		`{"on_track": false, "deviation": "drift one"}`,
		`{"on_track": false, "deviation": "drift two"}`,
		`{"on_track": true}`,
		`{"on_track": false, "deviation": "drift three"}`,
		`{"on_track": false, "deviation": "drift four"}`,
	})
	r := newTestReflector(judge, nil)
	conv := convWithGoal("goal")
	conv.AddAssistantMessage("step")

	for i := 0; i < 5; i++ {
		r.Fire(TriggerPatchGenerated, conv)
		r.Consume(conv)
	}

	st := r.Stats()
	if st.Replans != 0 {
		t.Errorf("a Yes in the middle must reset the window; got %d replans", st.Replans)
	}
	if st.OnTrack != 1 || st.Drift != 4 {
		t.Errorf("counts wrong: onTrack=%d drift=%d", st.OnTrack, st.Drift)
	}
	if st.Reminders != 4 {
		t.Errorf("four No verdicts below the threshold should each inject a reminder, got %d", st.Reminders)
	}
}

func TestInvalidVerdictDoesNotConsumeWindow(t *testing.T) {
	// Two valid No, then garbage, then a third valid No. The garbage must be discarded entirely —
	// a broken judge must not look like a confident No. (scriptedJudge repeats the LAST reply once
	// exhausted, so the fourth verdict is scripted explicitly.)
	judge, _ := scriptedJudge([]string{
		`{"on_track": false, "deviation": "one"}`,
		`{"on_track": false, "deviation": "two"}`,
		`I'm not sure, sorry!`,
		`{"on_track": false, "deviation": "three"}`,
	})
	r := newTestReflector(judge, nil)
	conv := convWithGoal("goal")
	conv.AddAssistantMessage("step")

	for i := 0; i < 3; i++ {
		r.Fire(TriggerPatchGenerated, conv)
		r.Consume(conv)
	}
	if r.Stats().Replans != 0 {
		t.Error("an unparseable verdict must not count toward the K threshold")
	}

	// The garbage verdict was dropped, so this No is the third consecutive VALID No → replan.
	r.Fire(TriggerPatchGenerated, conv)
	r.Consume(conv)
	if r.Stats().Replans != 1 {
		t.Errorf("three VALID consecutive No should replan; got %d replans (drift=%d invalid=%d)",
			r.Stats().Replans, r.Stats().Drift, r.Stats().Invalid)
	}
}

func TestJudgeErrorIsSilentAndNotADrift(t *testing.T) {
	judge := func(context.Context, string, string) (string, ReflectionUsage, error) {
		return "", ReflectionUsage{}, fmt.Errorf("side query unavailable")
	}
	r := newTestReflector(judge, nil)
	conv := convWithGoal("goal")
	conv.AddAssistantMessage("step")

	r.Fire(TriggerPatchGenerated, conv)
	r.Consume(conv)

	st := r.Stats()
	if st.Drift != 0 || st.Reminders != 0 || st.Replans != 0 {
		t.Errorf("a judge failure must produce no disposition (drift=%d reminders=%d replans=%d)", st.Drift, st.Reminders, st.Replans)
	}
	// It should still be counted as a call, since a call was attempted.
	if st.Calls != 1 {
		t.Errorf("calls = %d, want 1 (the attempt happened)", st.Calls)
	}
}

// ── 目标与轨迹输入组装 ────────────────────────────────────────────────────

func TestExtractTaskGoalSkipsRemindersAndMemoryHeader(t *testing.T) {
	conv := conversation.NewManager()
	conv.InjectLongTermMemory("# tencoderMd stuff", "# autoMemory stuff") // prepends a header block
	conv.AddSystemReminder("deferred tools are available")
	conv.AddUserMessage("THE REAL GOAL: restore the three-file test")

	got := extractTaskGoal(conv.GetMessages())
	if got != "THE REAL GOAL: restore the three-file test" {
		t.Errorf("goal = %q; must skip system-reminders and the auto-memory header", got)
	}
}

func TestCaptureGoalIsStableAcrossFires(t *testing.T) {
	judge, _ := scriptedJudge([]string{`{"on_track": true}`})
	r := newTestReflector(judge, nil)
	conv := convWithGoal("the original goal")
	conv.AddAssistantMessage("work")

	first := r.captureGoal(conv)
	conv.AddUserMessage("actually, forget that, do something else entirely")
	second := r.captureGoal(conv)

	if first != second {
		t.Errorf("goal must not drift mid-run: %q → %q", first, second)
	}
}

func TestBuildTraceShowsCallsAndResults(t *testing.T) {
	r := newTestReflector(scriptedNoopJudge(), nil)
	conv := conversation.NewManager()
	conv.AddUserMessage("goal")
	conv.AddAssistantMessage("I will inspect the auth package.")
	conv.AddAssistantMessageWithTools("", []conversation.ToolUseBlock{
		{ToolUseID: "t1", ToolName: "EditFile", Arguments: map[string]any{"file_path": "internal/auth/login.go"}},
	})
	conv.AddToolResultsMessage([]conversation.ToolResultBlock{
		{ToolUseID: "t1", Content: "ok", IsError: false},
	})

	trace := r.buildTrace(conv)
	for _, want := range []string{"assistant:", "call: EditFile", "internal/auth/login.go", "result[ok]"} {
		if !strings.Contains(trace, want) {
			t.Errorf("trace missing %q:\n%s", want, trace)
		}
	}
}

func TestBuildTraceMarksErrors(t *testing.T) {
	r := newTestReflector(scriptedNoopJudge(), nil)
	conv := conversation.NewManager()
	conv.AddUserMessage("goal")
	conv.AddToolResultsMessage([]conversation.ToolResultBlock{
		{ToolUseID: "t1", Content: "Error: unknown tool", IsError: true},
	})
	if !strings.Contains(r.buildTrace(conv), "result[ERROR]") {
		t.Errorf("error results must be visibly marked in the trace:\n%s", r.buildTrace(conv))
	}
}

func TestBuildTraceIsByteCapped(t *testing.T) {
	r := newTestReflector(scriptedNoopJudge(), func(c *ReflectionConfig) { c.TraceMaxBytes = 200 })
	conv := conversation.NewManager()
	conv.AddUserMessage("goal")
	for i := 0; i < 50; i++ {
		conv.AddAssistantMessage(strings.Repeat("x", 500))
	}
	if got := len([]rune(r.buildTrace(conv))); got > 260 {
		t.Errorf("trace length %d runes, expected it capped near 200", got)
	}
}

// ── nil 安全与埋点 ────────────────────────────────────────────────────────

func TestNilReflectorIsSafe(t *testing.T) {
	var r *Reflector
	if r.Enabled() {
		t.Error("nil reflector must report disabled")
	}
	r.Fire(TriggerPatchGenerated, convWithGoal("g"))
	r.Consume(convWithGoal("g"))
	r.NotifyAnomalyCallback()
	if got := r.ObserveTools([]string{"EditFile"}, true); got != "" {
		t.Errorf("nil reflector ObserveTools = %q, want empty", got)
	}
	if r.NotifyPeriodic(5) {
		t.Error("nil reflector NotifyPeriodic must be false")
	}
	if st := r.Stats(); st.Calls != 0 {
		t.Error("nil reflector stats must be zero")
	}
}

func TestStatsSnapshotIsIndependentCopy(t *testing.T) {
	judge, _ := scriptedJudge([]string{`{"on_track": true}`})
	r := newTestReflector(judge, nil)
	conv := convWithGoal("goal")
	conv.AddAssistantMessage("step")
	r.Fire(TriggerPatchGenerated, conv)
	r.Consume(conv)

	st := r.Stats()
	if st.Triggers == nil {
		t.Fatal("triggers map should be populated")
	}
	st.Triggers["injected"] = 999
	if r.Stats().Triggers["injected"] != 0 {
		t.Error("Stats() must return a copy; mutating it must not affect the Reflector")
	}
}

func TestUsageIsAccumulated(t *testing.T) {
	judge, _ := scriptedJudge([]string{`{"on_track": true}`}) // returns in=100 out=20
	r := newTestReflector(judge, nil)
	conv := convWithGoal("goal")
	conv.AddAssistantMessage("step")
	r.Fire(TriggerPatchGenerated, conv)
	r.Consume(conv)

	st := r.Stats()
	if st.InputTokens != 100 || st.OutputTokens != 20 {
		t.Errorf("usage not accumulated: in=%d out=%d, want 100/20", st.InputTokens, st.OutputTokens)
	}
}

// ── 辅助 ─────────────────────────────────────────────────────────────────

func scriptedNoopJudge() ReflectionJudgeFn {
	return func(context.Context, string, string) (string, ReflectionUsage, error) {
		return `{"on_track": true}`, ReflectionUsage{}, nil
	}
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}
