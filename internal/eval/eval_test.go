package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadInstances_JSONAndJSONL(t *testing.T) {
	dir := t.TempDir()

	jsonPath := filepath.Join(dir, "a.json")
	arr := `[{"instance_id":"i-1","repo":"a/b"},{"instance_id":"i-2","repo":"c/d"}]`
	if err := os.WriteFile(jsonPath, []byte(arr), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readInstances(jsonPath)
	if err != nil || len(got) != 2 || got[0].InstanceID != "i-1" {
		t.Fatalf("json array read failed: %v %v", got, err)
	}

	jsonlPath := filepath.Join(dir, "b.jsonl")
	jsonl := "{\"instance_id\":\"j-1\"}\n\n{\"instance_id\":\"j-2\"}\n"
	if err := os.WriteFile(jsonlPath, []byte(jsonl), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = readInstances(jsonlPath)
	if err != nil || len(got) != 2 || got[1].InstanceID != "j-2" {
		t.Fatalf("jsonl read failed: %v %v", got, err)
	}
}

func TestLoadInstances_FilterAndLimit(t *testing.T) {
	s := &DatasetStore{CacheDir: t.TempDir()}
	path := filepath.Join(t.TempDir(), "ds.json")
	data := `[
		{"instance_id":"a-1"},
		{"instance_id":"b-2"},
		{"instance_id":"c-3"}
	]`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	all, err := s.LoadInstances(path, nil, 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("all: %d %v", len(all), err)
	}
	sel, err := s.LoadInstances(path, []string{"b-2", "c-3"}, 0)
	if err != nil || len(sel) != 2 || sel[0].InstanceID != "b-2" {
		t.Fatalf("filter: %v %v", sel, err)
	}
	lim, err := s.LoadInstances(path, nil, 1)
	if err != nil || len(lim) != 1 || lim[0].InstanceID != "a-1" {
		t.Fatalf("limit: %v %v", lim, err)
	}
}

func TestRunStore_PredictionsAndReport(t *testing.T) {
	s, err := NewRunStore(t.TempDir(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SavePrediction(Prediction{InstanceID: "a-1", ModelPatch: "diff --git x", ModelNameOrPath: "tencoder"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePrediction(Prediction{InstanceID: "b-2", ModelPatch: ""}); err != nil {
		t.Fatal(err)
	}
	preds, err := s.LoadPredictions()
	if err != nil || len(preds) != 2 || preds["a-1"].ModelPatch != "diff --git x" {
		t.Fatalf("predictions roundtrip: %v %v", preds, err)
	}

	rep := &Report{RunID: "r1", Total: 2, ResolvedCnt: 1, Results: []InstanceResult{
		{InstanceID: "a-1", Resolved: true},
		{InstanceID: "b-2", EmptyPatch: true},
	}}
	path, err := s.SaveReport(rep)
	if err != nil {
		t.Fatal(err)
	}
	if rep.PassAt1 != 0.5 {
		t.Fatalf("pass@1 = %v, want 0.5", rep.PassAt1)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("report missing: %v", err)
	}
}
