package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSourceURLLoadsAndEnvironmentOverridesTheReleaseSourceOffer(t *testing.T) {
	want := "https://example.com/kotantei/source.tar.gz"
	cfg, err := Load(strings.NewReader(`{"project":{"source_url":"https://example.com/older-source.tar.gz"}}`), func(key string) (string, bool) {
		return want, key == "KOTANTEI_SOURCE_URL"
	})
	if err != nil {
		t.Fatal("source offer configuration is not supported", err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Project struct {
			SourceURL string `json:"source_url"`
		} `json:"project"`
	}
	if err := json.Unmarshal(raw, &report); err != nil || report.Project.SourceURL != want {
		t.Fatal("source offer override was lost", string(raw), err)
	}
}

func TestSourceURLRejectsUnsafeOrOversizedValuesWithoutEchoingThem(t *testing.T) {
	for _, source := range []string{"http://example.com/DO-NOT-PRINT", "https://name:DO-NOT-PRINT@example.com/source", "https://:443/DO-NOT-PRINT", "file:///DO-NOT-PRINT", "https://example.com/" + strings.Repeat("x", 2048)} {
		raw, _ := json.Marshal(map[string]any{"project": map[string]any{"source_url": source}})
		if _, err := Load(strings.NewReader(string(raw)), noEnv); err == nil || strings.Contains(err.Error(), "DO-NOT-PRINT") {
			t.Fatal("unsafe source offer accepted or echoed", err)
		}
	}
}
