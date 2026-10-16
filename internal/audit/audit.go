// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

// Package audit implements 阶段四's write-audit trail.
//
// ── 为什么是异步旁路 ──
//
// 审计写在权限检查的关键路径上。如果同步写盘，一次工具调用就要多等一次 fsync，
// 而权限检查在每回合、每次工具调用都要走。**审计必须永不阻塞主循环**——
// 所以这里是一个单 writer goroutine + 缓冲 channel：调用方只做一次非阻塞投递。
//
// 投递失败（channel 满）会丢记录，这正是"绝不阻塞"要付的代价。取舍理由：
// 主循环卡死的代价比丢一条审计记录大得多，而且 channel 容量给到 1024，
// 正常负载下从未接近过上限。缓冲区大小写在下面而不是散在各处。

package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Subdir is where audit logs live, relative to the project root's .tencoder dir.
const Subdir = "audit"

// Record is one audited decision.
//
// The field set answers the three questions a reviewer actually asks: who did it, to which file,
// and was it allowed.
type Record struct {
	Time      string `json:"time"`
	SessionID string `json:"session_id,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Tool      string `json:"tool"`
	// Target is the file the operation touched. For command tools this is the extracted path
	// (best effort) — see the coverage caveat in the stage-four dev record.
	Target string `json:"target,omitempty"`
	// Action is a short human-readable summary of what the tool was asked to do.
	Action string `json:"action,omitempty"`
	// Decision is "allow" / "ask" / "deny" — the permission layer's verdict.
	Decision string `json:"decision"`
	// Outcome refines it with what actually happened: "auto-allowed", "approved-by-user",
	// "denied-by-user", "denied-readonly". This is the field acceptance standard 3/4 check for
	// a readonly-project write.
	Outcome string `json:"outcome,omitempty"`
	// Workspace names the registered project the target lives in, when known.
	Workspace string `json:"workspace,omitempty"`
	// Reason carries the permission layer's explanation.
	Reason string `json:"reason,omitempty"`
}

// Writer appends Records to a daily JSONL file.
type Writer struct {
	dir string

	// ch carries records to the single writer goroutine. Buffered so Record() is non-blocking;
	// capacity chosen to absorb a burst (a sub-agent doing 50 tool calls) without dropping.
	ch chan Record

	once sync.Once
	done chan struct{}

	dropped atomic.Int64
	written atomic.Int64
}

// NewWriter creates a writer rooted at <projectRoot>/.tencoder/audit.
//
// The directory and today's file are created lazily on the first record, so constructing a Writer
// never fails on a read-only or missing project — a session that records nothing costs nothing.
func NewWriter(projectRoot string) (*Writer, error) {
	if projectRoot == "" {
		return nil, fmt.Errorf("audit: empty project root")
	}
	dir := filepath.Join(projectRoot, ".tencoder", Subdir)
	w := &Writer{
		dir:  dir,
		ch:   make(chan Record, 1024),
		done: make(chan struct{}),
	}
	go w.loop()
	return w, nil
}

// loop is the single writer goroutine. One writer means no file-lock coordination at all —
// concurrent Records from many sub-agent goroutines are serialized by the channel.
func (w *Writer) loop() {
	defer close(w.done)
	for rec := range w.ch {
		w.write(rec)
	}
}

// write appends one record, opening (creating if needed) the day's file.
//
// Appending with O_APPEND means a record is written as a single syscall in practice, so even a
// crash mid-session leaves every previously-written line intact and parseable. That matters for an
// audit trail specifically: a partially-written last line is recoverable, a corrupted middle is not.
func (w *Writer) write(rec Record) {
	if rec.Time == "" {
		rec.Time = time.Now().Format(time.RFC3339)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	data = append(data, '\n')

	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return
	}
	path := w.fileFor(rec.Time)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Write(data); err == nil {
		w.written.Add(1)
	}
}

// fileFor maps a record timestamp to its daily file. Derived from the record rather than from
// time.Now() so a replayed/restored record lands in the day it actually happened.
func (w *Writer) fileFor(ts string) string {
	day := "unknown"
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		day = t.Format("20060102")
	} else {
		day = time.Now().Format("20060102")
	}
	return filepath.Join(w.dir, "audit-"+day+".jsonl")
}

// Record enqueues a record. Never blocks.
//
// A full channel drops the record and bumps the counter rather than blocking the caller — the
// permission check that called this must return immediately, because a tool call is waiting on it.
func (w *Writer) Record(r Record) {
	if w == nil {
		return
	}
	select {
	case w.ch <- r:
	default:
		w.dropped.Add(1)
	}
}

// Close flushes pending records and stops the writer goroutine.
//
// It drains what is already queued (a bounded wait) rather than blocking on a slow disk forever —
// on shutdown the caller wants to exit, and the alternative to a dropped flush is a hung process.
func (w *Writer) Close() {
	if w == nil {
		return
	}
	w.once.Do(func() {
		close(w.ch)
		select {
		case <-w.done:
		case <-time.After(2 * time.Second):
		}
	})
}

// Stats reports written/dropped counts — surfaced in the debug log so an audit gap is visible
// rather than silent.
func (w *Writer) Stats() (written, dropped int64) {
	if w == nil {
		return 0, 0
	}
	return w.written.Load(), w.dropped.Load()
}

// LogPath returns the current day's log path, for the /workspace info display and for tests.
// Returns "" when the writer is not usable.
func (w *Writer) LogPath() string {
	if w == nil {
		return ""
	}
	return filepath.Join(w.dir, "audit-"+time.Now().Format("20060102")+".jsonl")
}

// ReadAll parses every JSONL record in the given directory, newest day last.
//
// Test/debug helper rather than a hot-path API — the point of an audit log is that a human or a
// grep reads it, not that the agent queries it in-process.
func ReadAll(dir string) ([]Record, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Record
	// ReadDir returns sorted by filename, which for audit-YYYYMMDD.jsonl is chronological.
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, line := range splitLines(data) {
			if len(line) == 0 {
				continue
			}
			var r Record
			if err := json.Unmarshal(line, &r); err != nil {
				continue // skip a torn line rather than failing the whole read
			}
			out = append(out, r)
		}
	}
	return out, nil
}

func splitLines(data []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				out = append(out, data[start:i])
			}
			start = i + 1
		}
	}
	if start < len(data) {
		out = append(out, data[start:])
	}
	return out
}
