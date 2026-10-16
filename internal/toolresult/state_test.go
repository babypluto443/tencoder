// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package toolresult

import "testing"

func TestNewReturnsEmpty(t *testing.T) {
	s := New()
	if len(s.SeenIDs) != 0 {
		t.Fatalf("SeenIDs not empty: %v", s.SeenIDs)
	}
	if len(s.Replacements) != 0 {
		t.Fatalf("Replacements not empty: %v", s.Replacements)
	}
}

func TestCloneIndependent(t *testing.T) {
	src := New()
	src.SeenIDs["a"] = struct{}{}
	src.Replacements["a"] = "preview_a"

	cloned := src.Clone()
	cloned.SeenIDs["b"] = struct{}{}
	cloned.Replacements["b"] = "preview_b"

	if _, ok := src.SeenIDs["b"]; ok {
		t.Fatal("source SeenIDs mutated by clone write")
	}
	if _, ok := src.Replacements["b"]; ok {
		t.Fatal("source Replacements mutated by clone write")
	}
	if _, ok := cloned.SeenIDs["a"]; !ok {
		t.Fatal("clone missing source-seeded id 'a'")
	}
	if cloned.Replacements["a"] != "preview_a" {
		t.Fatal("clone preview_a not preserved")
	}
}
