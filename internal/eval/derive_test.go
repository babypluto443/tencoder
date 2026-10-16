package eval

import (
	"reflect"
	"strings"
	"testing"
)

func TestParsePytestLog_ShortSummaryPASSEDFirst(t *testing.T) {
	// pytest -rA 的 short summary：状态在前（非 -v 模式的主要信息来源）
	log := strings.Join([]string{
		"=========================== short test summary info ===========================",
		"PASSED test/unit/test_x.py::test_validator[Success-schema0-hello-expected0]",
		"FAILED test/unit/test_x.py::test_broken - AssertionError: boom",
		"PASSED test/unit/test_x.py::test_ok",
	}, "\n")
	st := ParsePytestLog(log)
	if st["test/unit/test_x.py::test_validator[Success-schema0-hello-expected0]"] != StatusPassed {
		t.Fatalf("PASSED-first summary not parsed: %v", st)
	}
	if st["test/unit/test_x.py::test_broken"] != StatusFailed {
		t.Fatalf("FAILED-first summary not parsed: %v", st)
	}
	if st["test/unit/test_x.py::test_ok"] != StatusPassed {
		t.Fatalf("test_ok: %v", st)
	}
}

func TestNormalizeStatus_XFAILIsPass(t *testing.T) {
	// 官方口径：XFAIL/XPASS 视为 pass；SKIPPED 不算 pass
	if normalizeStatus("XFAIL") != StatusPassed {
		t.Fatal("XFAIL should map to PASSED")
	}
	if normalizeStatus("XPASS") != StatusPassed {
		t.Fatal("XPASS should map to PASSED")
	}
	if normalizeStatus("SKIPPED") == StatusPassed {
		t.Fatal("SKIPPED must not map to PASSED")
	}
	if normalizeStatus("ERROR") != StatusError {
		t.Fatal("ERROR should stay ERROR")
	}
}

func TestParsePytestLog_XFAILVerbose(t *testing.T) {
	log := "tests/test_core.py::test_edge XFAIL                                [100%]"
	st := ParsePytestLog(log)
	if st["tests/test_core.py::test_edge"] != StatusPassed {
		t.Fatalf("XFAIL verbose line: %v", st)
	}
}

func TestDeriveTestSets(t *testing.T) {
	s0 := map[string]string{ // 基线：仅 test_patch
		"tests/test_a.py::test_new":      StatusFailed, // gold 后应过 → F2P
		"tests/test_a.py::test_existing": StatusPassed, // 两次都过 → P2P
		"tests/test_a.py::test_flaky":    StatusError,  // gold 后仍挂 → 忽略
		"tests/test_b.py::test_stable":   StatusPassed, // 两次都过 → P2P
	}
	s1 := map[string]string{ // gold：test_patch + gold patch
		"tests/test_a.py::test_new":      StatusPassed,
		"tests/test_a.py::test_existing": StatusPassed,
		"tests/test_a.py::test_flaky":    StatusError,
		"tests/test_b.py::test_stable":   StatusPassed,
	}
	f2p, p2p := DeriveTestSets(s0, s1)
	if !reflect.DeepEqual(f2p, []string{"tests/test_a.py::test_new"}) {
		t.Fatalf("f2p = %v", f2p)
	}
	wantP2P := []string{"tests/test_a.py::test_existing", "tests/test_b.py::test_stable"}
	if !reflect.DeepEqual(p2p, wantP2P) {
		t.Fatalf("p2p = %v, want %v", p2p, wantP2P)
	}
}

func TestEffectiveTestCmds(t *testing.T) {
	inst := &Instance{TestPatch: sampleTestPatch}
	cmds := EffectiveTestCmds(inst)
	if len(cmds) != 1 {
		t.Fatalf("want 1 cmd, got %v", cmds)
	}
	// 文件按字母序排列，conftest.py 在 test_core.py 前
	want := "python -m pytest --no-header -rA --tb=no -p no:cacheprovider tests/conftest.py tests/test_core.py"
	if cmds[0] != want {
		t.Fatalf("cmd = %q, want %q", cmds[0], want)
	}
	// 已有 test_cmds 时直接用数据集字段
	inst2 := &Instance{TestCmds: []string{"make test"}}
	if got := EffectiveTestCmds(inst2); !reflect.DeepEqual(got, inst2.TestCmds) {
		t.Fatalf("dataset cmds must take priority, got %v", got)
	}
}

func TestIsResolved_WithDerivedMetadata(t *testing.T) {
	inst := &Instance{
		FAILToPASS: []string{"tests/test_a.py::test_new"},
		PASSToPASS: []string{"tests/test_a.py::test_existing"},
	}
	// 模型补丁后：new 过了，existing 也保住了 → resolved
	log := "tests/test_a.py::test_new PASSED   [ 50%]\ntests/test_a.py::test_existing PASSED   [100%]"
	ok, missing := IsResolved(inst, ParsePytestLog(log))
	if !ok || len(missing) != 0 {
		t.Fatalf("resolved expected, missing=%v", missing)
	}
	// existing 被改坏 → not resolved
	log2 := "tests/test_a.py::test_new PASSED   [ 50%]\ntests/test_a.py::test_existing FAILED   [100%]"
	ok, missing = IsResolved(inst, ParsePytestLog(log2))
	if ok || len(missing) != 1 {
		t.Fatalf("not resolved expected, missing=%v", missing)
	}
}
