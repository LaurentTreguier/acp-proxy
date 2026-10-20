package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	path := writeTemp(t, `{"agents":[{"model":"a","command":"echo"}]}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:8080" {
		t.Errorf("addr = %q, want default", cfg.Addr)
	}
	if cfg.Agents[0].Permissions != PermissionCancel {
		t.Errorf("permissions = %q, want %q", cfg.Agents[0].Permissions, PermissionCancel)
	}
	if _, ok := cfg.Agent("a"); !ok {
		t.Error("Agent(a) not found")
	}
	if _, ok := cfg.Agent("b"); ok {
		t.Error("Agent(b) should not exist")
	}
}

func TestLoadRejectsDuplicates(t *testing.T) {
	path := writeTemp(t, `{"agents":[{"model":"a","command":"x"},{"model":"a","command":"y"}]}`)
	if _, err := Load(path); err == nil {
		t.Error("expected error for duplicate model")
	}
}

func TestLoadRejectsUnknownPolicy(t *testing.T) {
	path := writeTemp(t, `{"agents":[{"model":"a","command":"x","permissions":"yolo"}]}`)
	if _, err := Load(path); err == nil {
		t.Error("expected error for unknown permissions policy")
	}
}

func TestLoadRejectsMissingCommand(t *testing.T) {
	path := writeTemp(t, `{"agents":[{"model":"a"}]}`)
	if _, err := Load(path); err == nil {
		t.Error("expected error for missing command")
	}
}
