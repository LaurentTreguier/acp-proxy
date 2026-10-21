package config

import (
	"strings"
	"testing"
)

func emptyEnv(string) string { return "" }

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseDefaults(t *testing.T) {
	opts, err := Parse(nil, emptyEnv)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Host != "127.0.0.1" || opts.Port != DefaultPort {
		t.Errorf("default addr = %s:%d, want 127.0.0.1:%d", opts.Host, opts.Port, DefaultPort)
	}
	if len(opts.Agents) != 0 {
		t.Errorf("agents = %+v, want none", opts.Agents)
	}
	if opts.AutoPermissions {
		t.Error("auto permissions should default to off")
	}
	if opts.PermissionPolicy() != "cancel" {
		t.Errorf("policy = %q, want cancel", opts.PermissionPolicy())
	}
}

func TestParsePositionalPort(t *testing.T) {
	opts, err := Parse([]string{"9123"}, emptyEnv)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Port != 9123 {
		t.Errorf("port = %d, want 9123", opts.Port)
	}
}

func TestParsePrecedence(t *testing.T) {
	env := envMap(map[string]string{"ACP_PROXY_PORT": "9001"})
	opts, err := Parse([]string{"-port", "9002"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Port != 9002 {
		t.Errorf("flag should override env: port = %d, want 9002", opts.Port)
	}

	opts, err = Parse(nil, env)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Port != 9001 {
		t.Errorf("env port = %d, want 9001", opts.Port)
	}

	opts, err = Parse([]string{"9003"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Port != 9003 {
		t.Errorf("positional port = %d, want 9003", opts.Port)
	}
}

func TestParseAgentFlag(t *testing.T) {
	opts, err := Parse([]string{
		"-agent", "claude-code=npx -y @zed-industries/claude-code-acp",
		"-agent", "gemini=npx -y @zed-industries/gemini-cli-acp",
	}, emptyEnv)
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.Agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(opts.Agents))
	}
	a := opts.Agents[0]
	if a.Model != "claude-code" || a.Command != "npx" || len(a.Args) != 2 || a.Args[0] != "-y" {
		t.Errorf("agent[0] = %+v", a)
	}
	if _, ok := opts.Agent("claude-code"); !ok {
		t.Error("Agent(claude-code) not found")
	}
}

func TestParseUnnamedAgent(t *testing.T) {
	opts, err := Parse([]string{"-agent", "/usr/local/bin/my-agent --flag"}, emptyEnv)
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(opts.Agents))
	}
	a := opts.Agents[0]
	if a.Model != DefaultModel || a.Command != "/usr/local/bin/my-agent" || len(a.Args) != 1 || a.Args[0] != "--flag" {
		t.Errorf("agent = %+v", a)
	}
}

func TestParseAgentsEnv(t *testing.T) {
	env := envMap(map[string]string{
		"ACP_PROXY_AGENTS": "a=cmd1 x y; b=cmd2",
	})
	opts, err := Parse(nil, env)
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.Agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(opts.Agents))
	}
	if opts.Agents[0].Model != "a" || opts.Agents[0].Command != "cmd1" || len(opts.Agents[0].Args) != 2 {
		t.Errorf("agent[0] = %+v", opts.Agents[0])
	}
	if opts.Agents[1].Model != "b" || opts.Agents[1].Command != "cmd2" {
		t.Errorf("agent[1] = %+v", opts.Agents[1])
	}
}

func TestParseAuto(t *testing.T) {
	for _, v := range []string{"1", "true", "YES", "on"} {
		opts, err := Parse(nil, envMap(map[string]string{"ACP_PROXY_AUTO": v}))
		if err != nil {
			t.Fatal(err)
		}
		if !opts.AutoPermissions {
			t.Errorf("ACP_PROXY_AUTO=%q should enable auto", v)
		}
	}
	opts, err := Parse(nil, envMap(map[string]string{"ACP_PROXY_AUTO": "no"}))
	if err != nil {
		t.Fatal(err)
	}
	if opts.AutoPermissions {
		t.Error("ACP_PROXY_AUTO=no should not enable auto")
	}
	opts, err = Parse([]string{"-auto"}, emptyEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.AutoPermissions || opts.PermissionPolicy() != "auto" {
		t.Error("-auto should enable auto policy")
	}
}

func TestParseDuplicateModel(t *testing.T) {
	_, err := Parse([]string{"-agent", "m=cmd1", "-agent", "m=cmd2"}, emptyEnv)
	if err == nil || !strings.Contains(err.Error(), "duplicate model") {
		t.Errorf("expected duplicate model error, got %v", err)
	}
}

func TestParseInvalid(t *testing.T) {
	if _, err := Parse([]string{"notaport"}, emptyEnv); err == nil {
		t.Error("expected error for non-numeric positional")
	}
	if _, err := Parse([]string{"-port", "0"}, emptyEnv); err == nil {
		t.Error("expected error for port 0")
	}
	if _, err := Parse([]string{"-agent", ""}, emptyEnv); err == nil {
		t.Error("expected error for empty agent spec")
	}
	env := envMap(map[string]string{"ACP_PROXY_PORT": "http"})
	if _, err := Parse(nil, env); err == nil {
		t.Error("expected error for invalid ACP_PROXY_PORT")
	}
}

func TestParseUnknownFlag(t *testing.T) {
	if _, err := Parse([]string{"-nope"}, emptyEnv); err == nil {
		t.Error("expected error for unknown flag")
	}
}
