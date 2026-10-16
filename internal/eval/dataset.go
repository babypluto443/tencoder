// Package eval 实现 SWE-bench-Live 自动化评测流水线：
// 数据集加载 → Agent rollout（只见 problem_statement）→ 生成 model_patch
// → Docker 容器内应用补丁并执行测试 → 解析日志判定 resolved → pass@1 汇总。
// 评测协议遵循 SWE-bench：rollout 阶段禁止接触 gold patch / test_patch /
// FAIL_TO_PASS 等字段；test_patch 仅在评测阶段由 harness 应用。
package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Instance 对齐 SWE-bench-Live (HuggingFace SWE-bench-Live/SWE-bench-Live) 的字段结构。
type Instance struct {
	Repo             string   `json:"repo"`
	PullNumber       int      `json:"pull_number"`
	InstanceID       string   `json:"instance_id"`
	IssueNumbers     []string `json:"issue_numbers"`
	BaseCommit       string   `json:"base_commit"`
	Patch            string   `json:"patch"`      // gold patch，仅供评测对照，rollout 不可见
	TestPatch        string   `json:"test_patch"` // 测试改动，仅 harness 在评测阶段应用
	ProblemStatement string   `json:"problem_statement"`
	FAILToPASS       []string `json:"FAIL_TO_PASS"`
	PASSToPASS       []string `json:"PASS_TO_PASS"`
	ImageKey         string   `json:"image_key"`
	TestCmds         []string `json:"test_cmds"`
	LogParser        string   `json:"log_parser"` // 目前官方取值为 pytest
}

// hfRowsResponse 是 HuggingFace datasets-server /rows 接口的响应结构。
type hfRowsResponse struct {
	NumRowsTotal int `json:"num_rows_total"`
	Rows         []struct {
		Row Instance `json:"row"`
	} `json:"rows"`
}

// DatasetStore 负责实例集的获取与缓存。
type DatasetStore struct {
	// CacheDir 缓存目录，默认 <workroot>/.tencoder/eval
	CacheDir string
	// HTTPClient 可注入（测试用），nil 时用默认 http.Client
	HTTPClient HTTPDoer
	// HFMirror HuggingFace 镜像站（datasets-server 不可达时的 parquet fallback），
	// 默认 https://hf-mirror.com
	HFMirror string
}

const hfDatasetURL = "https://datasets-server.huggingface.co/rows"

// FetchSplit 拉取指定 split 的全部实例并写入本地缓存文件。
// 优先走 HuggingFace datasets-server rows API；失败（网络不可达等）
// 自动 fallback 到镜像站的 parquet 分片通道。
// split 取值：lite / verified / full。返回缓存文件路径。
func (s *DatasetStore) FetchSplit(split string) (string, error) {
	if err := os.MkdirAll(s.CacheDir, 0o755); err != nil {
		return "", err
	}
	cachePath := filepath.Join(s.CacheDir, "dataset-"+split+".json")
	if _, err := os.Stat(cachePath); err == nil {
		return cachePath, nil // 已缓存
	}

	instances, primaryErr := s.fetchViaRowsAPI(split)
	if primaryErr != nil || len(instances) == 0 {
		// fallback：镜像站 parquet
		var fbErr error
		instances, fbErr = fetchSplitViaParquet(context.Background(), s.HTTPClient, s.HFMirror, split, s.CacheDir)
		if fbErr != nil {
			return "", fmt.Errorf("rows API: %v; parquet fallback: %w", primaryErr, fbErr)
		}
	}
	if len(instances) == 0 {
		return "", fmt.Errorf("split %q returned 0 instances", split)
	}
	if err := saveInstances(cachePath, instances); err != nil {
		return "", err
	}
	return cachePath, nil
}

// fetchViaRowsAPI 走 datasets-server 分页拉取。
func (s *DatasetStore) fetchViaRowsAPI(split string) ([]Instance, error) {
	var instances []Instance
	const pageLen = 100
	for offset := 0; ; offset += pageLen {
		url := fmt.Sprintf("%s?dataset=SWE-bench-Live/SWE-bench-Live&config=default&split=%s&offset=%d&length=%d",
			hfDatasetURL, split, offset, pageLen)
		body, err := httpGetJSON(s.HTTPClient, url)
		if err != nil {
			return nil, fmt.Errorf("fetch offset=%d: %w", offset, err)
		}
		var resp hfRowsResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("decode offset=%d: %w", offset, err)
		}
		for _, r := range resp.Rows {
			instances = append(instances, r.Row)
		}
		if len(resp.Rows) < pageLen || offset+pageLen >= resp.NumRowsTotal {
			break
		}
	}
	return instances, nil
}

// LoadInstances 从本地缓存文件（JSON 数组或 JSONL）加载实例，
// 并可按 ids 过滤（ids 为空返回全部）、按 limit 截断。
func (s *DatasetStore) LoadInstances(cachePath string, ids []string, limit int) ([]Instance, error) {
	instances, err := readInstances(cachePath)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		want := make(map[string]bool, len(ids))
		for _, id := range ids {
			want[id] = true
		}
		filtered := instances[:0]
		for _, inst := range instances {
			if want[inst.InstanceID] {
				filtered = append(filtered, inst)
			}
		}
		instances = filtered
	}
	if limit > 0 && limit < len(instances) {
		instances = instances[:limit]
	}
	return instances, nil
}

func readInstances(path string) ([]Instance, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// 优先按 JSON 数组解析；失败则按 JSONL 逐行解析。
	var arr []Instance
	if err := json.Unmarshal(data, &arr); err == nil {
		return arr, nil
	}
	var out []Instance
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024) // problem_statement 可能很长
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var inst Instance
		if err := json.Unmarshal([]byte(line), &inst); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		out = append(out, inst)
	}
	return out, sc.Err()
}

func saveInstances(path string, instances []Instance) error {
	data, err := json.MarshalIndent(instances, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// UpdateInstance 按 instance_id 回写单条实例到缓存文件（用于持久化推导出的元数据）。
func (s *DatasetStore) UpdateInstance(cachePath string, updated Instance) error {
	instances, err := readInstances(cachePath)
	if err != nil {
		return err
	}
	found := false
	for i := range instances {
		if instances[i].InstanceID == updated.InstanceID {
			instances[i] = updated
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("instance %s not in cache", updated.InstanceID)
	}
	return saveInstances(cachePath, instances)
}

// SanitizeID 把 instance_id 转成文件名安全形式。
func SanitizeID(id string) string {
	return strings.NewReplacer("/", "_", " ", "_").Replace(id)
}

// RunTimestamp 生成 run 目录用的可读时间戳。
func RunTimestamp() string {
	return time.Now().Format("20060102-150405")
}
