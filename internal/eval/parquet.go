package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"
)

// pqInstance 是 SWE-bench-Live parquet 分片的行结构（与 HuggingFace schema 对齐）。
type pqInstance struct {
	Repo             string   `parquet:"repo"`
	PullNumber       int64    `parquet:"pull_number"`
	InstanceID       string   `parquet:"instance_id"`
	IssueNumbers     []string `parquet:"issue_numbers"`
	BaseCommit       string   `parquet:"base_commit"`
	Patch            string   `parquet:"patch"`
	TestPatch        string   `parquet:"test_patch"`
	ProblemStatement string   `parquet:"problem_statement"`
	FAILToPASS       []string `parquet:"FAIL_TO_PASS"`
	PASSToPASS       []string `parquet:"PASS_TO_PASS"`
	ImageKey         string   `parquet:"image_key"`
	TestCmds         []string `parquet:"test_cmds"`
	LogParser        string   `parquet:"log_parser"`
}

func (p *pqInstance) toInstance() Instance {
	return Instance{
		Repo:             p.Repo,
		PullNumber:       int(p.PullNumber),
		InstanceID:       p.InstanceID,
		IssueNumbers:     p.IssueNumbers,
		BaseCommit:       p.BaseCommit,
		Patch:            p.Patch,
		TestPatch:        p.TestPatch,
		ProblemStatement: p.ProblemStatement,
		FAILToPASS:       p.FAILToPASS,
		PASSToPASS:       p.PASSToPASS,
		ImageKey:         p.ImageKey,
		TestCmds:         p.TestCmds,
		LogParser:        p.LogParser,
	}
}

const defaultHFMirror = "https://hf-mirror.com"

// fetchParquetShardURLs 通过 HF API 列出 split 的全部 parquet 分片 URL。
// mirrorHost 为空时直连 huggingface.co。
func fetchParquetShardURLs(client HTTPDoer, mirrorHost, dataset, split string) ([]string, error) {
	if mirrorHost == "" {
		mirrorHost = defaultHFMirror
	}
	api := fmt.Sprintf("%s/api/datasets/%s/parquet/default/%s", strings.TrimSuffix(mirrorHost, "/"), dataset, split)
	body, err := httpGetJSON(client, api)
	if err != nil {
		return nil, err
	}
	var urls []string
	if err := json.Unmarshal(body, &urls); err != nil {
		return nil, fmt.Errorf("decode shard list: %w", err)
	}
	// 列表返回的是 huggingface.co 域名，重写为镜像域名
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		u = strings.Replace(u, "https://huggingface.co", strings.TrimSuffix(mirrorHost, "/"), 1)
		out = append(out, u)
	}
	return out, nil
}

// fetchSplitViaParquet 走镜像站下载 parquet 分片并解析为实例列表。
// 这是 datasets-server 不可达（如国内网络）时的 fallback 通道。
func fetchSplitViaParquet(ctx context.Context, client HTTPDoer, mirrorHost, split, cacheDir string) ([]Instance, error) {
	shards, err := fetchParquetShardURLs(client, mirrorHost, "SWE-bench-Live/SWE-bench-Live", split)
	if err != nil {
		return nil, err
	}
	if len(shards) == 0 {
		return nil, fmt.Errorf("split %q has no parquet shards", split)
	}
	if err := os.MkdirAll(filepath.Join(cacheDir, "parquet"), 0o755); err != nil {
		return nil, err
	}

	var instances []Instance
	for i, shard := range shards {
		local := filepath.Join(cacheDir, "parquet", fmt.Sprintf("%s-%d.parquet", split, i))
		if err := downloadIfNeeded(ctx, client, shard, local); err != nil {
			return nil, fmt.Errorf("download shard %d: %w", i, err)
		}
		rows, err := readParquetInstances(local)
		if err != nil {
			return nil, fmt.Errorf("parse shard %d: %w", i, err)
		}
		instances = append(instances, rows...)
	}
	return instances, nil
}

func downloadIfNeeded(ctx context.Context, client HTTPDoer, rawURL, local string) error {
	if st, err := os.Stat(local); err == nil && st.Size() > 0 {
		return nil
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, sanitizeURL(rawURL))
	}
	f, err := os.Create(local)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func sanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Host + u.Path
}

// readParquetInstances 读取一个 parquet 文件中的全部实例。
func readParquetInstances(path string) ([]Instance, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := parquet.NewReader(f)
	var out []Instance
	for {
		var row pqInstance
		err := r.Read(&row)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if row.InstanceID == "" {
			continue
		}
		out = append(out, row.toInstance())
	}
	return out, nil
}
