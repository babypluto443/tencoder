// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package agent

import (
	"testing"
)

func TestActivateAndClearSkills(t *testing.T) {
	a := &Agent{}
	a.ActivateSkill("commit", "do git stuff")
	a.ActivateSkill("review", "audit changes")

	if got := a.GetActiveSkills(); len(got) != 2 {
		t.Errorf("expected 2 active skills, got %d", len(got))
	}

	a.ClearActiveSkills()
	if got := a.GetActiveSkills(); len(got) != 0 {
		t.Errorf("ClearActiveSkills did not empty the map; got %d", len(got))
	}
}
