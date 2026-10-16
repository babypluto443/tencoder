package eval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Prediction 是 SWE-bench 官方 predictions 格式。
type Prediction struct {
	InstanceID      string `json:"instance_id"`
	ModelPatch      string `json:"model_patch"`
	ModelNameOrPath string `json:"model_name_or_path"`
}

// InstanceResult 单实例结果汇总。
type InstanceResult struct {
	InstanceID   string   `json:"instance_id"`
	Resolved     bool     `json:"resolved"`
	EmptyPatch   bool     `json:"empty_patch"`
	ApplyFailed  string   `json:"apply_failed,omitempty"`
	Error        string   `json:"error,omitempty"`
	Missing      []string `json:"missing,omitempty"`
	DurationSec  float64  `json:"duration_sec"`
	Trajectory   string   `json:"trajectory,omitempty"`
	InputTokens  int      `json:"input_tokens"`
	OutputTokens int      `json:"output_tokens"`
}

// Report 一次评测 run 的汇总报告。
type Report struct {
	RunID       string           `json:"run_id"`
	Model       string           `json:"model"`
	Total       int              `json:"total"`
	ResolvedCnt int              `json:"resolved"`
	PassAt1     float64          `json:"pass_at_1"`
	Results     []InstanceResult `json:"results"`
}

// RunStore 管理一次 run 的输出目录（predictions.jsonl / report.json / logs/）。
type RunStore struct {
	Dir string
}

// NewRunStore 创建 <root>/runs/<runID> 目录。
func NewRunStore(root, runID string) (*RunStore, error) {
	dir := filepath.Join(root, "runs", runID)
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		return nil, err
	}
	return &RunStore{Dir: dir}, nil
}

// SavePrediction 追加一条 prediction 到 predictions.jsonl。
func (s *RunStore) SavePrediction(p Prediction) error {
	f, err := os.OpenFile(filepath.Join(s.Dir, "predictions.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// SaveLog 落盘单实例完整测试日志。
func (s *RunStore) SaveLog(instanceID, log string) (string, error) {
	path := filepath.Join(s.Dir, "logs", SanitizeID(instanceID)+".log")
	if err := os.WriteFile(path, []byte(log), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// SaveReport 写入汇总报告并计算 pass@1。
func (s *RunStore) SaveReport(rep *Report) (string, error) {
	if rep.Total > 0 {
		rep.PassAt1 = float64(rep.ResolvedCnt) / float64(rep.Total)
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(s.Dir, "report.json")
	return path, os.WriteFile(path, b, 0o644)
}

// LoadPredictions 读取 predictions.jsonl（评测模式：rollout 与评测分离执行时用）。
func (s *RunStore) LoadPredictions() (map[string]Prediction, error) {
	f, err := os.Open(filepath.Join(s.Dir, "predictions.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]Prediction{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 32*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var p Prediction
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return nil, fmt.Errorf("parse prediction: %w", err)
		}
		out[p.InstanceID] = p
	}
	return out, sc.Err()
}
