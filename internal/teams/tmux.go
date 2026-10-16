// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com


package teams

import (
	"fmt"
	"os/exec"
	"strings"
)

func spawnTmuxTeammate(teamName, memberName, cliCommand string) (string, error) {
	paneName := fmt.Sprintf("%s-%s", teamName, memberName)

	// Create a new tmux window (not split) for the teammate
	cmd := exec.Command("tmux", "new-window", "-d", "-n", paneName, cliCommand)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return "", fmt.Errorf("tmux new-window: %s: %s", err, strings.TrimSpace(string(output)))
	}
	return paneName, nil
}

func stopTmuxTeammate(paneName string) {
	exec.Command("tmux", "send-keys", "-t", paneName, "C-c", "").Run()
	exec.Command("tmux", "kill-window", "-t", paneName).Run()
}

