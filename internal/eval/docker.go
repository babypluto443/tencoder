package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// HTTPDoer 抽象 HTTP 客户端，便于测试注入。
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

func httpGetJSON(d HTTPDoer, url string) ([]byte, error) {
	if d == nil {
		d = &http.Client{Timeout: 10 * time.Minute}
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tencoder-eval/1.0")
	resp, err := d.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return io.ReadAll(resp.Body)
}

// ImageName 按 SWE-bench-Live 官方规则解析实例级 Docker 镜像名：
// starryzhang/sweb.eval.x86_64.<instance_id 小写，"__" 替换为 "_1776_">
func ImageName(instanceID, namespace string) string {
	if namespace == "" {
		namespace = "starryzhang"
	}
	name := strings.ToLower(instanceID)
	name = strings.ReplaceAll(name, "__", "_1776_")
	return fmt.Sprintf("%s/sweb.eval.x86_64.%s", namespace, name)
}

// ResolveImage 优先使用数据集自带的 image_key，否则按规则推导。
func ResolveImage(inst *Instance, namespace string) string {
	if inst.ImageKey != "" {
		return inst.ImageKey
	}
	return ImageName(inst.InstanceID, namespace)
}

// Docker 封装 docker CLI 的最小能力集，全部走 exec.Command，
// 不引入 docker SDK，保持零额外依赖。
type Docker struct {
	Bin string // 默认 "docker"
}

func (d *Docker) binary() string {
	if d.Bin != "" {
		return d.Bin
	}
	return "docker"
}

// runOut 执行 docker 命令并返回 stdout（合并 stderr 到错误信息）。
func (d *Docker) runOut(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, d.binary(), args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("docker %s: %s", strings.Join(args[:min2(len(args), 2)], " "), msg)
	}
	return out.String(), nil
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Pull 拉取镜像（镜像较大，注意 ctx 超时给足）。
func (d *Docker) Pull(ctx context.Context, image string) error {
	_, err := d.runOut(ctx, nil, "pull", image)
	return err
}

// StartContainer 以 sleep infinity 启动常驻容器，返回容器名。
func (d *Docker) StartContainer(ctx context.Context, image, name string) (string, error) {
	if name == "" {
		name = "tencoder-eval-" + fmt.Sprint(time.Now().UnixNano())
	}
	if _, err := d.runOut(ctx, nil, "rm", "-f", name); err != nil {
		return "", err
	}
	if _, err := d.runOut(ctx, nil, "run", "-d", "--name", name, image, "sleep", "infinity"); err != nil {
		return "", err
	}
	return name, nil
}

// CopyToDir 把宿主机目录复制进容器的指定路径（docker cp）。
func (d *Docker) CopyToDir(ctx context.Context, container, srcDir, dstDir string) error {
	// 先确保目标父目录存在
	if _, err := d.runOut(ctx, nil, "exec", container, "mkdir", "-p", dstDir); err != nil {
		return err
	}
	_, err := d.runOut(ctx, nil, "cp", srcDir+"/.", container+":"+dstDir)
	return err
}

// Exec 在容器内执行 shell 命令，stdin 可用于通过管道传入补丁内容。
func (d *Docker) Exec(ctx context.Context, container, workdir, shellCmd string, stdin io.Reader) (string, error) {
	args := []string{"exec"}
	if stdin != nil {
		args = append(args, "-i")
	}
	if workdir != "" {
		args = append(args, "-w", workdir)
	}
	args = append(args, container, "bash", "-c", shellCmd)
	return d.runOut(ctx, stdin, args...)
}

// RemoveContainer 强制删除容器（幂等）。
func (d *Docker) RemoveContainer(ctx context.Context, container string) {
	_, _ = d.runOut(ctx, nil, "rm", "-f", container)
}

// Available 检查 docker 是否可用。
func (d *Docker) Available() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := d.runOut(ctx, nil, "info", "--format", "{{.ServerVersion}}")
	return err == nil
}

// ContainerInfo 调试用：容器状态 JSON。
func (d *Docker) ContainerInfo(ctx context.Context, container string) (string, error) {
	out, err := d.runOut(ctx, nil, "inspect", "--format", "{{.State.Status}}", container)
	return strings.TrimSpace(out), err
}

// marshalJSONLine 输出单行 JSON（用于轨迹文件）。
func marshalJSONLine(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(fmt.Sprintf(`{"type":"marshal_error","error":%q}`, err.Error()))
	}
	return b
}
