// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

//go:build media

// 这些用例针对 tencoder/internal/media 包，而该包目前尚未实现。
// 不加这个 tag 的话，本文件的 import 会让整个 internal/tools 的测试二进制编译失败，
// 连带把该包里所有其他用例（延迟加载 benchmark、阶段三驱逐测试等）一起堵死。
// 用 build tag 而不是删文件：等 media 包落地后，用 `-tags media` 即可把它们放回来。

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tencoder/internal/media"
)

// fakeImageProvider returns canned results without any network call.
type fakeImageProvider struct {
	results []media.ImageResult
	err     error
}

func (f *fakeImageProvider) Name() string { return "fake" }
func (f *fakeImageProvider) GenerateImage(context.Context, media.ImageRequest) ([]media.ImageResult, error) {
	return f.results, f.err
}

// fakeVideoProvider fails the test if GenerateVideo is ever called.
type fakeVideoProvider struct{ t *testing.T }

func (f *fakeVideoProvider) Name() string { return "fake" }
func (f *fakeVideoProvider) GenerateVideo(context.Context, media.VideoRequest, media.ProgressFn) (*media.VideoResult, error) {
	f.t.Fatal("provider should not be reached")
	return nil, nil
}

func TestGenerateImageTool_NoProvider(t *testing.T) {
	tool := &GenerateImageTool{WorkDir: t.TempDir()}
	res := tool.Execute(context.Background(), map[string]any{"prompt": "x"})
	if !res.IsError || !strings.Contains(res.Output, "no image media provider configured") {
		t.Fatalf("expected configuration error, got %+v", res)
	}
}

func TestGenerateImageTool_SavesFile(t *testing.T) {
	dir := t.TempDir()
	tool := &GenerateImageTool{
		WorkDir:  dir,
		Provider: &fakeImageProvider{results: []media.ImageResult{{Bytes: []byte("PNGDATA"), Ext: "png"}}},
	}
	res := tool.Execute(context.Background(), map[string]any{"prompt": "a cat", "size": "512x512"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "Saved image to") {
		t.Fatalf("output missing saved path: %q", res.Output)
	}

	path := strings.TrimSpace(strings.TrimPrefix(res.Output, "Saved image to "))
	if filepath.Dir(path) != filepath.Join(dir, ".tencoder", "media") {
		t.Errorf("file saved to %q, want under %s", path, filepath.Join(dir, ".tencoder", "media"))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading saved file: %v", err)
	}
	if string(data) != "PNGDATA" {
		t.Errorf("saved content = %q, want PNGDATA", data)
	}
}

func TestGenerateVideoTool_BadNumFrames(t *testing.T) {
	tool := &GenerateVideoTool{
		WorkDir:  t.TempDir(),
		Provider: &fakeVideoProvider{t: t}, // would t.Fatal if reached
	}
	res := tool.Execute(context.Background(), map[string]any{
		"prompt":     "a cat",
		"num_frames": 100, // not 8n+1 — must be rejected before the provider call
	})
	if !res.IsError || !strings.Contains(res.Output, "8n+1") {
		t.Fatalf("expected num_frames validation error, got %+v", res)
	}
}

func TestGenerateVideoTool_RejectsNonURLInput(t *testing.T) {
	cases := map[string]string{
		"local path": ".tencoder/media/ref.png",
		"data URI":   "data:image/png;base64,iVBORw0KGgoAAAANSU=",
		"absolute":   "/tmp/ref.png",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			tool := &GenerateVideoTool{
				WorkDir:  t.TempDir(),
				Provider: &fakeVideoProvider{t: t}, // must NOT be reached
			}
			res := tool.Execute(context.Background(), map[string]any{
				"prompt":       "x",
				"input_images": []any{in},
			})
			if !res.IsError || !strings.Contains(res.Output, "public http(s) URLs") {
				t.Fatalf("expected URL-only validation error, got %+v", res)
			}
		})
	}
}
