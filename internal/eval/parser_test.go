package eval

import (
	"strings"
	"testing"
)

const sampleTestPatch = `diff --git a/tests/test_core.py b/tests/test_core.py
index 111..222 100644
--- a/tests/test_core.py
+++ b/tests/test_core.py
@@ -1,3 +1,6 @@
+import pytest
+
 def test_existing():
-    assert f() == 1
+    assert f() == 2
diff --git a/tests/conftest.py b/tests/conftest.py
--- /dev/null
+++ b/tests/conftest.py
@@ -0,0 +1,2 @@
+fixture stuff
`

const sampleModelPatch = `diff --git a/core/engine.py a/core/engine.py
index aaa..bbb 100644
--- a/core/engine.py
+++ a/core/engine.py
@@ -10,7 +10,7 @@
-def f():
-    return 1
+def f():
+    return 2
diff --git a/tests/test_core.py b/tests/test_core.py
index 111..333 100644
--- a/tests/test_core.py
+++ b/tests/test_core.py
@@ -1,2 +1,2 @@
-bad touch
+worst touch
`

func TestTestFilePaths(t *testing.T) {
	files := TestFilePaths(sampleTestPatch)
	if !files["tests/test_core.py"] || !files["tests/conftest.py"] {
		t.Fatalf("unexpected file set: %v", files)
	}
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d: %v", len(files), files)
	}
}

func TestStripTestFiles(t *testing.T) {
	files := TestFilePaths(sampleTestPatch)
	got := StripTestFiles(sampleModelPatch, files)
	if strings.Contains(got, "tests/test_core.py") {
		t.Fatalf("test file diff not stripped:\n%s", got)
	}
	if !strings.Contains(got, "core/engine.py") {
		t.Fatalf("source diff lost:\n%s", got)
	}
}

func TestStripTestFiles_KeepsAllWhenNoTestFiles(t *testing.T) {
	got := StripTestFiles(sampleModelPatch, map[string]bool{})
	if got != sampleModelPatch {
		t.Fatalf("patch should be unchanged")
	}
}

func TestParsePytestLog_Verbose(t *testing.T) {
	log := strings.Join([]string{
		"============================= test session starts =============================",
		"platform darwin -- Python 3.11.0, pytest-8.0.0",
		"collected 3 items",
		"",
		"tests/test_core.py::test_existing PASSED                             [ 33%]",
		"tests/test_core.py::test_new_feature[case1] PASSED                    [ 66%]",
		"tests/test_core.py::test_new_feature[case2] FAILED                    [100%]",
		"",
		"================================== FAILURES ===================================",
	}, "\n")
	st := ParsePytestLog(log)
	if st["tests/test_core.py::test_existing"] != StatusPassed {
		t.Fatalf("test_existing: %v", st)
	}
	if st["tests/test_core.py::test_new_feature[case1]"] != StatusPassed {
		t.Fatalf("param case1: %v", st)
	}
	if st["tests/test_core.py::test_new_feature[case2]"] != StatusFailed {
		t.Fatalf("param case2: %v", st)
	}
}

func TestParsePytestLog_ShortSummaryOverrides(t *testing.T) {
	log := strings.Join([]string{
		"tests/test_core.py::test_a PASSED                              [ 50%]",
		"tests/test_core.py::test_b PASSED                              [100%]",
		"=========================== short test summary info ===========================",
		"FAILED tests/test_core.py::test_a - AssertionError: expected 2",
	}, "\n")
	st := ParsePytestLog(log)
	if st["tests/test_core.py::test_a"] != StatusFailed {
		t.Fatalf("summary should override to FAILED, got %v", st)
	}
	if st["tests/test_core.py::test_b"] != StatusPassed {
		t.Fatalf("test_b: %v", st)
	}
}

func TestIsResolved(t *testing.T) {
	inst := &Instance{
		FAILToPASS: []string{"tests/test_core.py::test_new"},
		PASSToPASS: []string{"tests/test_core.py::test_old"},
	}
	st := map[string]string{
		"tests/test_core.py::test_new": StatusPassed,
		"tests/test_core.py::test_old": StatusPassed,
	}
	if ok, missing := IsResolved(inst, st); !ok || len(missing) != 0 {
		t.Fatalf("should resolve, missing=%v", missing)
	}
	st["tests/test_core.py::test_new"] = StatusFailed
	if ok, missing := IsResolved(inst, st); ok || len(missing) != 1 {
		t.Fatalf("should not resolve, missing=%v", missing)
	}
	// SKIPPED 严格视为未通过
	st["tests/test_core.py::test_new"] = "SKIPPED"
	if ok, _ := IsResolved(inst, st); ok {
		t.Fatal("SKIPPED must not count as passed")
	}
}

func TestImageName(t *testing.T) {
	cases := []struct{ id, want string }{
		{"amoffat__sh-744", "starryzhang/sweb.eval.x86_64.amoffat_1776_sh-744"},
		{"scikit-learn__scikit-learn-13779", "starryzhang/sweb.eval.x86_64.scikit-learn_1776_scikit-learn-13779"},
	}
	for _, c := range cases {
		if got := ImageName(c.id, ""); got != c.want {
			t.Errorf("ImageName(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}
