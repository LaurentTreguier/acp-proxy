// Package config parses acp-proxy options from command-line flags and
// environment variables. There is no configuration file: every option is
// available as a flag and as an ACP_PROXY_* environment variable.
package config

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
)

// Env var prefix shared by every option.
const envPrefix = "ACP_PROXY_"

// DefaultModel is the model name given to an agent spec without an
// explicit "name=" prefix.
const DefaultModel = "default"

// Agent maps an OpenAI model name to an ACP agent command.
type Agent struct {
	// Model is the name exposed through the OpenAI-compatible API.
	Model string
	// Command is the executable to spawn as the ACP agent.
	Command string
	// Args are the command-line arguments passed to the agent.
	Args []string
}

// Options is the fully resolved server configuration.
type Options struct {
	Host string
	Port int
	// Agents lists statically defined ACP agents, by id. They take
	// precedence over registry entries with the same id.
	Agents []Agent
	// Dir is the working directory for agent processes and ACP sessions
	// ("" means the current directory).
	Dir string
	// AutoPermissions auto-approves agent permission requests (selecting
	// the first allow_* option). When false, permission requests are
	// answered "cancelled".
	AutoPermissions bool
	// RegistryURL is the ACP registry index URL.
	RegistryURL string
	// CacheDir is where downloaded agent binaries are stored
	// ("" means the user cache directory).
	CacheDir string
}

// Parse resolves options from flags, environment variables and one
// optional positional port argument. Precedence, highest first:
// positional port, flags, environment variables, defaults.
func Parse(args []string, getenv func(string) string) (*Options, error) {
	opts := &Options{}

	portDefault := 8080
	if v := getenv(envPrefix + "PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid %sPORT %q", envPrefix, v)
		}
		portDefault = p
	}
	hostDefault := "127.0.0.1"
	if v := getenv(envPrefix + "HOST"); v != "" {
		hostDefault = v
	}

	var agents []Agent
	if v := getenv(envPrefix + "AGENTS"); v != "" {
		for _, spec := range strings.Split(v, ";") {
			a, err := ParseAgentSpec(spec)
			if err != nil {
				return nil, fmt.Errorf("%sAGENTS: %w", envPrefix, err)
			}
			agents = append(agents, a)
		}
	}

	fs := flag.NewFlagSet("acp-proxy", flag.ContinueOnError)
	fs.Usage = func() { printUsage(fs) }
	fs.IntVar(&opts.Port, "port", portDefault, "listen `port`")
	fs.StringVar(&opts.Host, "host", hostDefault, "listen `host`")
	fs.Func("agent", "ACP agent spec `[name=]command [args...]`, repeatable", func(s string) error {
		a, err := ParseAgentSpec(s)
		if err != nil {
			return err
		}
		agents = append(agents, a)
		return nil
	})
	fs.StringVar(&opts.Dir, "dir", getenv(envPrefix+"DIR"), "working `directory` for agent sessions")
	fs.BoolVar(&opts.AutoPermissions, "auto", parseBoolEnv(getenv(envPrefix+"AUTO")),
		"auto-approve agent permission requests")
	fs.StringVar(&opts.RegistryURL, "registry", getenv(envPrefix+"REGISTRY"),
		"`URL` of the ACP registry index (default: the official ACP registry)")
	fs.StringVar(&opts.CacheDir, "cache-dir", getenv(envPrefix+"CACHE_DIR"),
		"`directory` for downloaded agent binaries (default: the user cache directory)")

	// The standard flag package stops at the first non-flag argument, so
	// loop to allow the positional port anywhere: "acp-proxy 8080 -auto"
	// and "acp-proxy -auto 8080" both work.
	rest := args
	var portArg int
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		p, err := strconv.Atoi(rest[0])
		if err != nil {
			return nil, fmt.Errorf("unexpected argument %q", rest[0])
		}
		portArg = p
		rest = rest[1:]
	}
	if portArg != 0 {
		opts.Port = portArg
	}

	opts.Agents = agents
	if err := opts.validate(); err != nil {
		return nil, err
	}
	return opts, nil
}

func (o *Options) validate() error {
	if o.Port < 1 || o.Port > 65535 {
		return fmt.Errorf("invalid port %d", o.Port)
	}
	seen := make(map[string]bool, len(o.Agents))
	for i, a := range o.Agents {
		if seen[a.Model] {
			return fmt.Errorf("agent %d: duplicate model %q", i, a.Model)
		}
		seen[a.Model] = true
	}
	return nil
}

// Agent returns the agent configured for a model name.
func (o *Options) Agent(model string) (Agent, bool) {
	for _, a := range o.Agents {
		if a.Model == model {
			return a, true
		}
	}
	return Agent{}, false
}

// PermissionPolicy returns the acp permission policy implied by
// AutoPermissions: "auto" or "cancel".
func (o *Options) PermissionPolicy() string {
	if o.AutoPermissions {
		return "auto"
	}
	return "cancel"
}

// ParseAgentSpec parses an agent spec: `[name=]command [args...]`.
// Without a "name=" prefix the agent is exposed as model "default".
// The command and its arguments are split on whitespace.
func ParseAgentSpec(spec string) (Agent, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Agent{}, fmt.Errorf("empty agent spec")
	}
	name := ""
	if i := strings.Index(spec, "="); i >= 0 {
		name = strings.TrimSpace(spec[:i])
		spec = strings.TrimSpace(spec[i+1:])
	}
	fields := strings.Fields(spec)
	if len(fields) == 0 {
		return Agent{}, fmt.Errorf("agent spec %q has no command", spec)
	}
	if name == "" {
		name = DefaultModel
	}
	return Agent{Model: name, Command: fields[0], Args: fields[1:]}, nil
}

func parseBoolEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func printUsage(fs *flag.FlagSet) {
	fmt.Fprintf(fs.Output(), `acp-proxy: an OpenAI-compatible API backed by ACP agents

Usage:
  acp-proxy [port] [flags]

Every flag has an %s* environment variable equivalent. Precedence,
highest first: positional port, flags, environment, defaults.

`, envPrefix)
	fs.PrintDefaults()
	fmt.Fprintf(fs.Output(), `
Agents are resolved on demand from the ACP registry
(https://agentclientprotocol.com) by id, via the URL prefix:
/{agent}/v1/chat/completions. Agents are downloaded automatically on
first use. Statically declared agents (see -agent) override registry
entries with the same id.

Examples:
  acp-proxy
  acp-proxy 8080
  curl http://127.0.0.1:8080/claude-acp/v1/chat/completions ...
  acp-proxy -agent "claude-code=npx -y @zed-industries/claude-code-acp"

Environment:
  ACP_PROXY_PORT       listen port (default 8080)
  ACP_PROXY_HOST       listen host (default 127.0.0.1)
  ACP_PROXY_AGENTS     static agent specs separated by ";"
  ACP_PROXY_DIR        working directory for agent sessions
  ACP_PROXY_AUTO       auto-approve permission requests ("1"/"true"/"yes"/"on")
  ACP_PROXY_REGISTRY   ACP registry index URL
  ACP_PROXY_CACHE_DIR  directory for downloaded agent binaries
`)
}
