// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package config

import "testing"

// TestValidateMediaProviders covers the optional media_providers section:
// empty is valid, each entry needs a name and a known type, and names must be
// unique.
func TestValidateMediaProviders(t *testing.T) {
	tests := []struct {
		name    string
		entries []MediaProviderConfig
		wantErr bool
	}{
		{name: "empty is allowed", entries: nil, wantErr: false},
		{
			name:    "valid image + video",
			entries: []MediaProviderConfig{{Name: "img", Type: "agnes-image"}, {Name: "vid", Type: "agnes-video"}},
			wantErr: false,
		},
		{name: "missing name", entries: []MediaProviderConfig{{Type: "agnes-image"}}, wantErr: true},
		{name: "bad type", entries: []MediaProviderConfig{{Name: "x", Type: "nope"}}, wantErr: true},
		{
			name:    "duplicate name",
			entries: []MediaProviderConfig{{Name: "dup", Type: "agnes-image"}, {Name: "dup", Type: "agnes-video"}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMediaProviders(&AppConfig{MediaProviders: tt.entries})
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateMediaProviders() err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

// TestMediaResolveAPIKey verifies the explicit key wins, then the env var, then
// empty.
func TestMediaResolveAPIKey(t *testing.T) {
	explicit := &MediaProviderConfig{Type: "agnes-image", APIKey: "explicit"}
	if got := explicit.ResolveAPIKey(); got != "explicit" {
		t.Errorf("explicit key: got %q, want %q", got, "explicit")
	}

	t.Setenv("AGNES_API_KEY", "from-env")
	envOnly := &MediaProviderConfig{Type: "agnes-video"}
	if got := envOnly.ResolveAPIKey(); got != "from-env" {
		t.Errorf("env key: got %q, want %q", got, "from-env")
	}

	t.Setenv("AGNES_API_KEY", "")
	none := &MediaProviderConfig{Type: "agnes-image"}
	if got := none.ResolveAPIKey(); got != "" {
		t.Errorf("no key: got %q, want empty", got)
	}
}

// TestMediaResolveBaseURL verifies explicit URLs are trailing-slash trimmed and
// the per-type default is used otherwise.
func TestMediaResolveBaseURL(t *testing.T) {
	explicit := &MediaProviderConfig{Type: "agnes-image", BaseURL: "https://example.com/api/"}
	if got := explicit.ResolveBaseURL(); got != "https://example.com/api" {
		t.Errorf("explicit base url: got %q, want trimmed", got)
	}

	def := &MediaProviderConfig{Type: "agnes-video"}
	if got := def.ResolveBaseURL(); got != "https://apihub.agnes-ai.com" {
		t.Errorf("default base url: got %q", got)
	}
}

// TestMergeConfig_MediaProviders verifies override entries replace same-named
// base entries and append new ones, like MCP servers.
func TestMergeConfig_MediaProviders(t *testing.T) {
	base := &AppConfig{MediaProviders: []MediaProviderConfig{
		{Name: "img", Type: "agnes-image", Model: "old"},
	}}
	override := &AppConfig{MediaProviders: []MediaProviderConfig{
		{Name: "img", Type: "agnes-image", Model: "new"}, // replaces
		{Name: "vid", Type: "agnes-video"},               // appends
	}}

	merged := mergeConfig(base, override)
	if len(merged.MediaProviders) != 2 {
		t.Fatalf("expected 2 media providers, got %d", len(merged.MediaProviders))
	}
	byName := map[string]MediaProviderConfig{}
	for _, m := range merged.MediaProviders {
		byName[m.Name] = m
	}
	if byName["img"].Model != "new" {
		t.Errorf("override should replace same-named entry: got model %q, want %q", byName["img"].Model, "new")
	}
	if _, ok := byName["vid"]; !ok {
		t.Errorf("override should append new entry 'vid'")
	}
}
