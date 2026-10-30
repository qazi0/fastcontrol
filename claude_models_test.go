package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestClaudeCatalogExpiresAndPreservesNativeNames(t *testing.T) {
	h := NewHub(NewIPCClient(""), "")
	m := newClaudeManager(h)
	m.bin = filepath.Join(t.TempDir(), "missing-claude")
	m.setModels([]any{map[string]any{"value": "opus", "resolvedModel": "claude-opus-5-5", "displayName": "Opus 5.5", "supportsEffort": true, "supportedEffortLevels": []any{"low", "high", "max"}}, map[string]any{"value": "fable", "resolvedModel": "claude-fable-5-1", "displayName": "Fable 5.1", "supportsEffort": true, "supportedEffortLevels": []any{"low", "high", "max"}}, map[string]any{"value": "legacy", "supportsEffort": false, "supportedEffortLevels": []any{"high"}}})
	if err := m.discoverModels(); err != nil {
		t.Fatal("fresh catalog unnecessarily started CLI", err)
	}
	if m.models[0].Name != "Opus 5.5" || m.models[1].Name != "Fable 5.1" || len(m.models[2].Efforts) != 0 || m.models[2].DefaultEffort != "" {
		t.Fatalf("catalog lost names or capabilities: %+v", m.models)
	}
	m.modelsAt = time.Now().Add(-2 * time.Minute)
	if err := m.discoverModels(); err == nil {
		t.Fatal("stale catalog never refreshed")
	}
	if m.models[0].Name != "Opus 5.5" {
		t.Fatal("failed refresh erased usable catalog")
	}
}
