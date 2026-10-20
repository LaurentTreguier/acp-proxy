// Package config loads the acp-proxy server configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Permission policies for agent-initiated permission requests.
const (
	// PermissionCancel answers every permission request with "cancelled".
	// This is the default: a headless proxy has no user to ask.
	PermissionCancel = "cancel"
	// PermissionAuto selects the first allow_* option the agent offers
	// (falling back to the first option), i.e. it lets the agent proceed.
	PermissionAuto = "auto"
)

// AgentConfig maps an OpenAI model name to an ACP agent command.
type AgentConfig struct {
	// Model is the name exposed through the OpenAI-compatible API.
	Model string `json:"model"`
	// Command is the executable to spawn as the ACP agent.
	Command string `json:"command"`
	// Args are the command-line arguments passed to the agent.
	Args []string `json:"args,omitempty"`
	// Dir is the working directory for the agent process and the session cwd.
	// Defaults to the server's working directory.
	Dir string `json:"dir,omitempty"`
	// Env contains extra environment variables layered over the server's
	// environment.
	Env map[string]string `json:"env,omitempty"`
	// Permissions selects the policy for session/request_permission calls:
	// "cancel" (default) or "auto".
	Permissions string `json:"permissions,omitempty"`
}

// Config is the whole server configuration.
type Config struct {
	// Addr is the HTTP listen address (host:port).
	Addr string `json:"addr"`
	// Agents lists the ACP agents exposed as models.
	Agents []AgentConfig `json:"agents"`
}

// Load reads a JSON config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.Addr == "" {
		c.Addr = "127.0.0.1:8080"
	}
	seen := make(map[string]bool, len(c.Agents))
	for i := range c.Agents {
		a := &c.Agents[i]
		if a.Model == "" {
			return fmt.Errorf("agent %d: model is required", i)
		}
		if seen[a.Model] {
			return fmt.Errorf("agent %d: duplicate model %q", i, a.Model)
		}
		seen[a.Model] = true
		if a.Command == "" {
			return fmt.Errorf("agent %d (%s): command is required", i, a.Model)
		}
		switch a.Permissions {
		case "", PermissionCancel:
			a.Permissions = PermissionCancel
		case PermissionAuto:
		default:
			return fmt.Errorf("agent %d (%s): unknown permissions policy %q", i, a.Model, a.Permissions)
		}
	}
	return nil
}

// Agent returns the agent configured for a model name.
func (c *Config) Agent(model string) (AgentConfig, bool) {
	for _, a := range c.Agents {
		if a.Model == model {
			return a, true
		}
	}
	return AgentConfig{}, false
}
