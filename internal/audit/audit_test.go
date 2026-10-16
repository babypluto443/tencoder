// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package audit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tencoder/internal/permissions"
	"tencoder/internal/tools"
)

// 阶段四的验证重点：审计必须**永不阻塞**权限检查，且只读升级路径的每一步都可追溯。

// syncSink collects records synchronously (test double for the async writer).
type syncSink struct {
	mu      sync.Mutex
	records []permissions.AuditEntry
}

func (s *syncSink) Record(e permissions.AuditEntry) {
	s.mu.Lock()
	s.records = append(s.records, e)
	s.mu.Unlock()
}

func (s *syncSink) all() []permissions.AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]permissions.AuditEntry, len(s.records))
	copy(out, s.records)
	return out
}

func (s *syncSink) outcomes() []string {
	var out []string
	for _, r := range s.all() {
		out = append(out, r.Outcome)
	}
	return out
}

func newAuditedChecker(t *testing.T, main, ro string) (*permissions.Checker, *syncSink) {
	t.Helper()
	sb := permissions.NewPathSandbox(main)
	sb.AddReadonlyRoot(ro, "pluto_feed")
	sink := &syncSink{}
	c := permissions.NewChecker(sb, &permissions.RuleEngine{}, permissions.ModeDefault)
	c.Audit = sink
	c.SessionID = "sess-1"
	c.AgentName = "main"
	return c, sink
}

// ── 只写记录写类决策 ──────────────────────────────────────────────────────

func TestAuditRecordsWriteButNotRead(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "main")
	os.MkdirAll(main, 0o755)
	c, sink := newAuditedChecker(t, main, filepath.Join(base, "ro"))

	c.Check(&fakeWrite{}, map[string]any{"file_path": filepath.Join(main, "a.go")})
	c.Check(&fakeRead{}, map[string]any{"file_path": filepath.Join(main, "a.go")})

	recs := sink.all()
	if len(recs) != 1 {
		t.Fatalf("only write decisions should be audited, got %d records: %+v", len(recs), recs)
	}
	if recs[0].Tool != "WriteFile" {
		t.Errorf("Tool = %q", recs[0].Tool)
	}
	if recs[0].SessionID != "sess-1" || recs[0].Agent != "main" {
		t.Errorf("identity not stamped: %+v", recs[0])
	}
	if recs[0].Time == "" {
		t.Error("Time must be stamped")
	}
	if recs[0].Target == "" {
		t.Error("Target must be recorded (acceptance standard 4 is file-level traceability)")
	}
	// ModeDefault asks for writes, so the recorded outcome is "pending-approval" — the audit
	// records what the permission layer DECIDED, not what happened afterwards.
	if recs[0].Outcome != "pending-approval" {
		t.Errorf("Outcome = %q, want pending-approval", recs[0].Outcome)
	}
}

func TestAuditRecordsPendingReadonlyApproval(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "main")
	ro := filepath.Join(base, "ro")
	os.MkdirAll(main, 0o755)
	os.MkdirAll(ro, 0o755)
	c, sink := newAuditedChecker(t, main, ro)

	d := c.Check(&fakeWrite{}, map[string]any{"file_path": filepath.Join(ro, "x.go")})
	if d.Effect != permissions.Ask || !d.ReadonlyWrite {
		t.Fatalf("expected a readonly Ask, got %+v", d)
	}
	if got := sink.outcomes(); len(got) != 1 || got[0] != "pending-readonly-approval" {
		t.Errorf("outcomes = %v, want [pending-readonly-approval]", got)
	}
}

// TestAuditFullReadonlyLifecycle is acceptance standard 3 end-to-end: a write attempt against a
// read-only project must leave BOTH the prompt and the user's refusal in the trail.
//
// Recording only the prompt would leave "asked" indistinguishable from "denied", which is exactly
// the gap that makes an audit log unusable when someone actually needs it.
func TestAuditFullReadonlyLifecycle(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "main")
	ro := filepath.Join(base, "ro")
	os.MkdirAll(main, 0o755)
	os.MkdirAll(ro, 0o755)
	c, sink := newAuditedChecker(t, main, ro)

	roTarget := filepath.Join(ro, "x.go")
	d := c.Check(&fakeWrite{}, map[string]any{"file_path": roTarget})
	// User says no.
	c.RecordApproval("WriteFile", roTarget, d.Workspace, false, d.ReadonlyWrite)

	outcomes := sink.outcomes()
	if len(outcomes) != 2 {
		t.Fatalf("expected prompt + approval records, got %v", outcomes)
	}
	if outcomes[0] != "pending-readonly-approval" {
		t.Errorf("first = %q", outcomes[0])
	}
	if outcomes[1] != "denied-readonly" {
		t.Errorf("second = %q, want denied-readonly (acceptance standard 3 names this outcome)", outcomes[1])
	}
}

func TestAuditRecordsApprovedReadonlyWrite(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "main")
	ro := filepath.Join(base, "ro")
	os.MkdirAll(main, 0o755)
	os.MkdirAll(ro, 0o755)
	c, sink := newAuditedChecker(t, main, ro)

	roTarget := filepath.Join(ro, "x.go")
	d := c.Check(&fakeWrite{}, map[string]any{"file_path": roTarget})
	c.RecordApproval("WriteFile", roTarget, d.Workspace, true, d.ReadonlyWrite)

	outcomes := sink.outcomes()
	if outcomes[len(outcomes)-1] != "approved-by-user" {
		t.Errorf("outcomes = %v, want approved-by-user", outcomes)
	}
	// Acceptance standard 4: an APPROVED readonly write must still be traceable.
	found := false
	for _, r := range sink.all() {
		if r.Workspace == "pluto_feed" && r.Outcome == "approved-by-user" {
			found = true
		}
	}
	if !found {
		t.Errorf("an approved readonly write must carry its workspace and outcome, got %+v", sink.all())
	}
}

func TestNoAuditSinkIsSilentButFunctional(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "main")
	os.MkdirAll(main, 0o755)
	sb := permissions.NewPathSandbox(main)
	c := permissions.NewChecker(sb, &permissions.RuleEngine{}, permissions.ModeDefault)
	// No Audit sink wired — must behave exactly as before 阶段四.
	if d := c.Check(&fakeWrite{}, map[string]any{"file_path": filepath.Join(main, "a.go")}); d.Effect == "" {
		t.Error("decision must still be produced without an audit sink")
	}
	c.RecordApproval("WriteFile", "x", "", true, false) // must not panic
}

// ── 异步 writer ──────────────────────────────────────────────────────────

func TestWriterPersistsAndFlushesOnClose(t *testing.T) {
	root := t.TempDir()
	w, err := NewWriter(root)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		w.Record(Record{Tool: "WriteFile", Target: "/a/b.go", Decision: "allow", Outcome: "auto-allowed"})
	}
	w.Close()

	recs, err := ReadAll(filepath.Join(root, ".tencoder", Subdir))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 20 {
		t.Fatalf("expected 20 records after Close, got %d", len(recs))
	}
	if written, dropped := w.Stats(); written != 20 || dropped != 0 {
		t.Errorf("Stats = (%d, %d), want (20, 0)", written, dropped)
	}
}

func TestWriterGroupsByDay(t *testing.T) {
	root := t.TempDir()
	w, _ := NewWriter(root)
	day1 := time.Now().AddDate(0, 0, -3).Format(time.RFC3339)
	w.Record(Record{Time: day1, Tool: "T1", Decision: "allow"})
	w.Record(Record{Time: day1, Tool: "T2", Decision: "allow"})
	w.Record(Record{Tool: "T3", Decision: "allow"})
	w.Close()

	dir := filepath.Join(root, ".tencoder", Subdir)
	entries, _ := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
	if len(entries) != 2 {
		t.Errorf("expected 2 daily files, got %d: %v", len(entries), entries)
	}
}

func TestRecordDoesNotBlockWhenChannelIsFull(t *testing.T) {
	// The contract that matters: a saturated audit queue must never stall a permission check.
	root := t.TempDir()
	w, _ := NewWriter(root)
	defer w.Close()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 20000; i++ { // far more than the 1024 buffer
			w.Record(Record{Tool: "T", Decision: "allow"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record blocked when the channel was full — audit must never stall the main loop")
	}
	if _, dropped := w.Stats(); dropped == 0 {
		t.Error("expected drops to be counted when the buffer saturates")
	}
}

func TestWriterNilSafe(t *testing.T) {
	var w *Writer
	w.Record(Record{Tool: "x"}) // must not panic
	w.Close()
	if written, dropped := w.Stats(); written != 0 || dropped != 0 {
		t.Error("nil writer stats must be zero")
	}
	if w.LogPath() != "" {
		t.Error("nil writer has no log path")
	}
}

func TestReadAllSkipsTornLine(t *testing.T) {
	// A crash mid-append can leave a partial last line. One unreadable line must not make the
	// whole trail unreadable.
	dir := t.TempDir()
	good := `{"time":"2026-10-16T00:00:00Z","tool":"WriteFile","decision":"allow"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "audit-20261016.jsonl"), []byte(good+`{"tool":"trunc`), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, err := ReadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Tool != "WriteFile" {
		t.Errorf("expected the intact record, got %+v", recs)
	}
}

func TestSummarizeActionIsDistinguishing(t *testing.T) {
	// An audit line that only says "WriteFile /a.go" leaves the reader guessing. The action
	// summary must carry the distinguishing argument.
	got := permissions.SummarizeAction("WriteFile", map[string]any{
		"file_path": "/a.go", "content": "package main\nfunc main() {}",
	})
	if !strings.Contains(got, "package main") {
		t.Errorf("WriteFile summary should include the first content line, got %q", got)
	}
	got = permissions.SummarizeAction("Bash", map[string]any{"command": "go test ./..."})
	if !strings.Contains(got, "go test") {
		t.Errorf("Bash summary should include the command, got %q", got)
	}
}

// ── 测试桩 ───────────────────────────────────────────────────────────────

type fakeWrite struct{}

func (f *fakeWrite) Name() string                 { return "WriteFile" }
func (f *fakeWrite) Description() string          { return "write" }
func (f *fakeWrite) Category() tools.ToolCategory { return tools.CategoryWrite }
func (f *fakeWrite) Schema() map[string]any       { return map[string]any{"name": "WriteFile"} }
func (f *fakeWrite) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	return tools.ToolResult{Output: "ok"}
}

type fakeRead struct{}

func (f *fakeRead) Name() string                 { return "ReadFile" }
func (f *fakeRead) Description() string          { return "read" }
func (f *fakeRead) Category() tools.ToolCategory { return tools.CategoryRead }
func (f *fakeRead) Schema() map[string]any       { return map[string]any{"name": "ReadFile"} }
func (f *fakeRead) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	return tools.ToolResult{Output: "ok"}
}
