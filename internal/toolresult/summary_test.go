// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package toolresult

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"tencoder/internal/conversation"
)

// 阶段三 · 任务 3.2 的验证：preview 位升级为 LLM 摘要，失败必须回退，且两条路径都要冻结。

// convWithBigResult builds a conversation holding one tool result well over SingleResultLimit,
// paired with the tool_use that produced it.
func convWithBigResult(id, toolName, content string) *conversation.Manager {
	conv := conversation.NewManager()
	conv.AddUserMessage("fix the failing auth test")
	conv.AddAssistantMessageWithTools("running the suite", []conversation.ToolUseBlock{
		{ToolUseID: id, ToolName: toolName, Arguments: map[string]any{"file_path": "/repo/main.go"}},
	})
	conv.AddToolResultsMessage([]conversation.ToolResultBlock{
		{ToolUseID: id, Content: content, IsError: true},
	})
	return conv
}

// bigContent produces output comfortably above SingleResultLimit (50_000 chars) so the spill path
// actually engages. The shape matters too: bulk noise first, the actionable failure LAST — which is
// exactly what the head-truncating mechanical preview throws away, and what tailExcerpt preserves.
func bigContent() string {
	var b strings.Builder
	for i := 0; i < 1200; i++ {
		fmt.Fprintf(&b, "line %d: loading config /etc/app/config-%04d.yaml\n", i, i)
	}
	b.WriteString("--- FAIL: TestLoginRejectsExpiredToken (0.42s) ---\n")
	b.WriteString("    main_test.go:88: expected status 401, got 200\n")
	return b.String()
}

// staticSummarizer returns a summarizer whose fn always yields the given text.
func staticSummarizer(reply string, err error, captured *string) SummarizerFn {
	return func(_ context.Context, sys, user string) (string, SummaryUsage, error) {
		if captured != nil {
			*captured = sys + "\n---USER---\n" + user
		}
		if err != nil {
			return "", SummaryUsage{}, err
		}
		return reply, SummaryUsage{InputTokens: 500, OutputTokens: 80}, nil
	}
}

// TestSummaryReplacesMechanicalPreview is the headline behaviour: with a summarizer wired, the
// context gets the LLM summary instead of a head-truncated blob.
func TestSummaryReplacesMechanicalPreview(t *testing.T) {
	dir := t.TempDir()
	conv := convWithBigResult("t1", "Bash", bigContent())
	state := New()

	var captured string
	s := NewSummarizer(staticSummarizer("FAIL TestLoginRejectsExpiredToken: main_test.go:88 expected status 401, got 200", nil, &captured), 0)

	if _, err := ApplyWithSummary(conv, dir, state, s, "fix the failing auth test"); err != nil {
		t.Fatalf("apply: %v", err)
	}

	got := conv.GetMessages()[2].ToolResults[0].Content
	if !isSummarizedReplacement(got) {
		t.Errorf("expected a <summarized-output> replacement, got:\n%s", got[:min(200, len(got))])
	}
	if !strings.Contains(got, "expected status 401, got 200") {
		t.Errorf("the actionable assertion detail must survive the summary:\n%s", got)
	}
	// The disk fallback must stay advertised — it is what makes summary hallucination recoverable.
	if !strings.Contains(got, "ReadFile") || !strings.Contains(got, SpillSubdir) {
		t.Errorf("summary must tell the agent how to re-read the full text:\n%s", got)
	}
	// And the task goal must have reached the summarizer (that's the whole advantage over a
	// mechanical preview).
	if !strings.Contains(captured, "fix the failing auth test") {
		t.Errorf("the summarizer did not receive the task goal:\n%s", captured)
	}
}

// TestSummaryFallsBackOnFailure — the "绝不会更坏" invariant.
func TestSummaryFallsBackOnFailure(t *testing.T) {
	dir := t.TempDir()

	t.Run("summarizer errors", func(t *testing.T) {
		conv := convWithBigResult("t1", "Bash", bigContent())
		s := NewSummarizer(staticSummarizer("", fmt.Errorf("side query unavailable"), nil), 0)
		if _, err := ApplyWithSummary(conv, dir, New(), s, "goal"); err != nil {
			t.Fatalf("apply must not error: %v", err)
		}
		got := conv.GetMessages()[2].ToolResults[0].Content
		if isSummarizedReplacement(got) {
			t.Error("a failed summary must fall back to the mechanical preview")
		}
		if !isAlreadyReplaced(got) {
			t.Errorf("fallback must still be a spill replacement, got:\n%s", got[:min(200, len(got))])
		}
		if s.Stats().FellBack != 1 {
			t.Errorf("FellBack = %d, want 1", s.Stats().FellBack)
		}
	})

	t.Run("summarizer returns empty", func(t *testing.T) {
		conv := convWithBigResult("t1", "Bash", bigContent())
		s := NewSummarizer(staticSummarizer("   \n  ", nil, nil), 0)
		ApplyWithSummary(conv, dir, New(), s, "goal")
		if isSummarizedReplacement(conv.GetMessages()[2].ToolResults[0].Content) {
			t.Error("an empty summary must fall back, not write an empty block")
		}
	})

	t.Run("nil summarizer", func(t *testing.T) {
		conv := convWithBigResult("t1", "Bash", bigContent())
		var s *Summarizer // unwired
		ApplyWithSummary(conv, dir, New(), s, "goal")
		got := conv.GetMessages()[2].ToolResults[0].Content
		if isSummarizedReplacement(got) {
			t.Error("nil summarizer must behave exactly like pre-阶段三")
		}
	})
}

// TestSummaryIsFrozenByteForByte — the prompt-cache invariant. The summary must replay verbatim on
// later turns; a re-summarized (differently worded) block would invalidate the cache prefix.
func TestSummaryIsFrozenByteForByte(t *testing.T) {
	dir := t.TempDir()
	conv := convWithBigResult("t1", "Bash", bigContent())
	state := New()

	calls := 0
	s := NewSummarizer(func(context.Context, string, string) (string, SummaryUsage, error) {
		calls++
		return fmt.Sprintf("summary attempt %d", calls), SummaryUsage{}, nil
	}, 0)

	ApplyWithSummary(conv, dir, state, s, "goal")
	first := conv.GetMessages()[2].ToolResults[0].Content

	// Later turns re-apply the same decision.
	ApplyWithSummary(conv, dir, state, s, "goal")
	ApplyWithSummary(conv, dir, state, s, "goal")
	third := conv.GetMessages()[2].ToolResults[0].Content

	if first != third {
		t.Errorf("summary must replay byte-for-byte:\nfirst: %q\nthird: %q", first, third)
	}
	if calls != 1 {
		t.Errorf("summarizer called %d times; the decision must be frozen after the first", calls)
	}
}

// TestApplyWithoutSummarizerIsUnchanged — back-compat: the 2-arg Apply path must behave exactly as
// it did before 阶段三.
func TestApplyWithoutSummarizerIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	conv := convWithBigResult("t1", "Bash", bigContent())
	if _, err := Apply(conv, dir, New()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got := conv.GetMessages()[2].ToolResults[0].Content
	if isSummarizedReplacement(got) {
		t.Error("Apply (no summarizer) must produce a mechanical preview")
	}
	if !strings.Contains(got, "persisted-output") {
		t.Errorf("expected the legacy <persisted-output> format:\n%s", got[:min(200, len(got))])
	}
}

// TestSanitizeSummary strips the wrappers chat models add.
func TestSanitizeSummary(t *testing.T) {
	cases := map[string]string{
		"```\nplain body\n```":                 "plain body",
		"```markdown\nplain body\n```":         "plain body",
		"  plain body  ":                       "plain body",
		"Here is the summary: the thing broke": "the thing broke",
		"here's the summary:\n- a\n- b":        "- a\n- b",
		"SUMMARY: short":                       "short",
		"":                                     "",
	}
	for in, want := range cases {
		if got := sanitizeSummary(in); got != want {
			t.Errorf("sanitizeSummary(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTailExcerptKeepsTheEnd — for log/diff output the conclusion lives at the end, which is exactly
// what buildSpillPreview's head truncation throws away.
func TestTailExcerptKeepsTheEnd(t *testing.T) {
	head := strings.Repeat("A", 100)
	tail := "PANIC: the assertion failed"
	full := head + tail

	got := tailExcerpt(full, 40)
	if !strings.HasSuffix(got, tail) {
		t.Errorf("tailExcerpt must preserve the tail, got %q", got)
	}
	if strings.Contains(got, strings.Repeat("A", 100)) {
		t.Error("tailExcerpt must not keep the head")
	}
	if tailExcerpt("short", 40) != "short" {
		t.Error("content under the cap must pass through unchanged")
	}
}

// TestSummaryBlockIsRecognizedByIsAlreadyReplaced — a summary restored from a session log must be
// recognised as already-decided, not re-summarized on the next turn.
func TestSummaryBlockIsRecognizedByIsAlreadyReplaced(t *testing.T) {
	preview := buildSummaryPreview("body", "/repo/.tencoder/tool_results/t1", 200000)
	if !isAlreadyReplaced(preview) {
		t.Error("a summary block must satisfy isAlreadyReplaced so restored sessions don't re-summarize")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
