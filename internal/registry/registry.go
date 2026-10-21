// Package registry resolves ACP agent ids to runnable agent commands,
// using the ACP registry (https://agentclientprotocol.com) for automatic
// download and execution of known agents, plus optional static agent
// definitions that take precedence over registry entries.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// DefaultURL is the official ACP registry index.
const DefaultURL = "https://cdn.agentclientprotocol.com/registry/v1/latest/registry.json"

// fetchTimeout bounds registry downloads and archive downloads.
const fetchTimeout = 60 * time.Second

// Index is the registry JSON document.
type Index struct {
	Version string  `json:"version"`
	Agents  []Entry `json:"agents"`
}

// Entry is one agent in the registry.
type Entry struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	Description  string       `json:"description"`
	Distribution Distribution `json:"distribution"`
}

// Distribution describes how to obtain and run the agent.
type Distribution struct {
	Npx    *NpxDist               `json:"npx,omitempty"`
	Binary map[string]*BinaryDist `json:"binary,omitempty"`
}

// NpxDist runs the agent through npx, which downloads the package on
// first use.
type NpxDist struct {
	Package string            `json:"package"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// BinaryDist downloads a platform archive and runs a command from it.
type BinaryDist struct {
	Archive string   `json:"archive"`
	Cmd     string   `json:"cmd"`
	Args    []string `json:"args,omitempty"`
	SHA256  string   `json:"sha256,omitempty"`
}

// fetchIndex downloads and parses a registry document.
func fetchIndex(ctx context.Context, url string) (*Index, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry: fetch %s: status %d", url, resp.StatusCode)
	}
	var index Index
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&index); err != nil {
		return nil, fmt.Errorf("registry: parse %s: %w", url, err)
	}
	return &index, nil
}

// cache holds the last successfully fetched index.
type cache struct {
	mu      sync.RWMutex
	byID    map[string]Entry
	fetched bool
}

func (c *cache) snapshot() (map[string]Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.byID, c.fetched
}

func (c *cache) store(entries []Entry) {
	byID := make(map[string]Entry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	c.mu.Lock()
	c.byID = byID
	c.fetched = true
	c.mu.Unlock()
}

func (c *cache) get(id string) (Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.byID[id]
	return e, ok
}
