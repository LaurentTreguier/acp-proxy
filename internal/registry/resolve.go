package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"acp-proxy/internal/acp"
)

// Client resolves agent ids to runnable agent commands. It merges static
// agents with registry entries and installs binary distributions on
// demand.
type Client struct {
	url       string
	cacheDir  string
	dir       string // working directory for agent sessions
	perm      string // acp permission policy
	static    map[string]acp.Agent
	cache     cache
	installMu sync.Mutex
	installs  map[string]*sync.Mutex
	http      *http.Client
}

// DefaultCacheDir is where downloaded agent binaries and persisted state
// live when not overridden: the user cache directory.
func DefaultCacheDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "acp-proxy")
	}
	return ".acp-proxy-cache"
}

// New creates a registry client. Static agents (by id) take precedence
// over registry entries; dir and perm apply to every resolved agent. An
// empty url selects the official ACP registry; an empty cacheDir selects
// the user cache directory.
func New(url, cacheDir, dir, perm string, static map[string]acp.Agent) *Client {
	if url == "" {
		url = DefaultURL
	}
	if cacheDir == "" {
		cacheDir = DefaultCacheDir()
	}
	return &Client{
		url:      url,
		cacheDir: cacheDir,
		dir:      dir,
		perm:     perm,
		static:   static,
		installs: make(map[string]*sync.Mutex),
		http:     &http.Client{Timeout: fetchTimeout},
	}
}

// Warmup fetches the registry once; failures are non-fatal, unknown ids
// trigger a refetch on demand.
func (c *Client) Warmup(ctx context.Context) error {
	index, err := fetchIndex(ctx, c.url)
	if err != nil {
		return err
	}
	c.cache.store(index.Agents)
	return nil
}

// List returns every known agent: static definitions plus registry
// entries, sorted by id. The registry is fetched on first use.
func (c *Client) List(ctx context.Context) ([]acp.AgentInfo, error) {
	byID, fetched := c.cache.snapshot()
	if !fetched {
		if err := c.Warmup(ctx); err != nil {
			return nil, err
		}
		byID, _ = c.cache.snapshot()
	}
	seen := make(map[string]bool, len(byID))
	var out []acp.AgentInfo
	for _, e := range byID {
		out = append(out, acp.AgentInfo{
			ID:          e.ID,
			Name:        e.Name,
			Version:     e.Version,
			Description: e.Description,
		})
		seen[e.ID] = true
	}
	for id := range c.static {
		if !seen[id] {
			out = append(out, acp.AgentInfo{ID: id, Name: id})
		}
	}
	// Deterministic order for clients and tests.
	slices.SortFunc(out, func(a, b acp.AgentInfo) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// Resolve returns the runnable agent for an id. Static agents win over
// registry entries; the registry is fetched on first use, and an id
// still missing after that is reported unknown.
func (c *Client) Resolve(ctx context.Context, id string) (acp.Agent, error) {
	if a, ok := c.static[id]; ok {
		return a, nil
	}
	entry, ok := c.cache.get(id)
	if !ok {
		if _, fetched := c.cache.snapshot(); !fetched {
			if err := c.Warmup(ctx); err != nil {
				return acp.Agent{}, fmt.Errorf("resolve %q: %w", id, err)
			}
			entry, ok = c.cache.get(id)
		}
	}
	if !ok {
		return acp.Agent{}, &acp.UnknownAgentError{ID: id}
	}

	switch {
	case entry.Distribution.Npx != nil:
		return c.npxAgent(entry), nil
	case len(entry.Distribution.Binary) > 0:
		return c.binaryAgent(ctx, entry)
	default:
		return acp.Agent{}, fmt.Errorf("agent %q has no supported distribution", id)
	}
}

func (c *Client) npxAgent(entry Entry) acp.Agent {
	d := entry.Distribution.Npx
	env := make([]string, 0, len(d.Env))
	for k, v := range d.Env {
		env = append(env, k+"="+v)
	}
	return acp.Agent{
		ID:          entry.ID,
		Command:     "npx",
		Args:        append([]string{"-y", d.Package}, d.Args...),
		Env:         env,
		Dir:         c.dir,
		Permissions: c.perm,
	}
}

func (c *Client) binaryAgent(ctx context.Context, entry Entry) (acp.Agent, error) {
	d, ok := entry.Distribution.Binary[platformKey()]
	if !ok {
		return acp.Agent{}, fmt.Errorf("agent %q has no binary distribution for %s",
			entry.ID, platformKey())
	}
	dir, err := c.ensureInstalled(ctx, entry, d)
	if err != nil {
		return acp.Agent{}, err
	}
	cmd := d.Cmd
	if !filepath.IsAbs(cmd) {
		cmd = filepath.Join(dir, strings.TrimPrefix(cmd, "./"))
	}
	return acp.Agent{
		ID:          entry.ID,
		Command:     cmd,
		Args:        d.Args,
		Dir:         c.dir,
		Permissions: c.perm,
	}, nil
}

// extractorVersion is written into the install marker; a marker from an
// older extractor (e.g. one that dropped symlinks) forces re-extraction.
const extractorVersion = "extractor-v2\n"

// ensureInstalled downloads, verifies and extracts the agent archive once
// per agent and version; concurrent callers wait for the winner.
func (c *Client) ensureInstalled(ctx context.Context, entry Entry, d *BinaryDist) (string, error) {
	key := entry.ID + "@" + entry.Version
	dir := filepath.Join(c.cacheDir, "agents", key)
	marker := filepath.Join(dir, ".installed")
	archivePath := filepath.Join(dir, "archive"+archiveExt(d.Archive))

	mu := c.mutexFor(key)
	mu.Lock()
	defer mu.Unlock()

	if markerOK(marker, d.Archive) {
		return dir, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("install %s: %w", key, err)
	}
	// Reuse an already-downloaded archive when it still matches the
	// expected checksum.
	if !archiveValid(archivePath, d.SHA256) {
		if err := c.download(ctx, d.Archive, archivePath, d.SHA256); err != nil {
			return "", fmt.Errorf("install %s: %w", key, err)
		}
	}
	if err := extract(archivePath, dir); err != nil {
		return "", fmt.Errorf("install %s: %w", key, err)
	}
	if err := os.WriteFile(marker, []byte(extractorVersion+d.Archive), 0o644); err != nil {
		return "", fmt.Errorf("install %s: %w", key, err)
	}
	return dir, nil
}

func markerOK(marker, archive string) bool {
	data, err := os.ReadFile(marker)
	if err != nil {
		return false
	}
	return string(data) == extractorVersion+archive
}

func archiveValid(path, sha string) bool {
	if sha == "" {
		st, err := os.Stat(path)
		return err == nil && st.Size() > 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == strings.ToLower(sha)
}

func (c *Client) mutexFor(key string) *sync.Mutex {
	c.installMu.Lock()
	defer c.installMu.Unlock()
	mu, ok := c.installs[key]
	if !ok {
		mu = &sync.Mutex{}
		c.installs[key] = mu
	}
	return mu
}

func (c *Client) download(ctx context.Context, url, dest, sha string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", url, resp.StatusCode)
	}

	f, err := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		return fmt.Errorf("download %s: %w", url, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if sha != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if got != strings.ToLower(sha) {
			return fmt.Errorf("download %s: sha256 mismatch: got %s, want %s", url, got, sha)
		}
	}
	return os.Rename(tmp, dest)
}

// platformKey maps the running platform to the registry's binary keys.
func platformKey() string {
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	}
	return runtime.GOOS + "-" + arch
}

func archiveExt(url string) string {
	switch {
	case strings.HasSuffix(url, ".tar.gz"), strings.HasSuffix(url, ".tgz"):
		return ".tar.gz"
	case strings.HasSuffix(url, ".zip"):
		return ".zip"
	default:
		return ".bin"
	}
}
