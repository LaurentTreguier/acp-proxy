package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"acp-proxy/internal/acp"
)

func TestParseIndex(t *testing.T) {
	raw := `{
	  "version": "1.0.0",
	  "agents": [
	    {
	      "id": "claude-acp",
	      "name": "Claude Agent",
	      "version": "0.94.0",
	      "description": "wrapper",
	      "distribution": {"npx": {"package": "@agentclientprotocol/claude-agent-acp@0.94.0"}}
	    },
	    {
	      "id": "amp-acp",
	      "name": "Amp",
	      "version": "0.10.0",
	      "distribution": {"binary": {"darwin-aarch64": {"archive": "https://example.com/a.tar.gz", "cmd": "./amp-acp", "sha256": "abc"}}}
	    }
	  ]
	}`
	var index Index
	if err := json.Unmarshal([]byte(raw), &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Agents) != 2 {
		t.Fatalf("agents = %d", len(index.Agents))
	}
	if index.Agents[0].Distribution.Npx.Package != "@agentclientprotocol/claude-agent-acp@0.94.0" {
		t.Errorf("npx package = %q", index.Agents[0].Distribution.Npx.Package)
	}
	if index.Agents[1].Distribution.Binary["darwin-aarch64"].Cmd != "./amp-acp" {
		t.Errorf("binary cmd = %q", index.Agents[1].Distribution.Binary["darwin-aarch64"].Cmd)
	}
}

func TestPlatformKey(t *testing.T) {
	got := platformKey()
	wantArch := runtime.GOARCH
	switch wantArch {
	case "amd64":
		wantArch = "x86_64"
	case "arm64":
		wantArch = "aarch64"
	}
	if got != runtime.GOOS+"-"+wantArch {
		t.Errorf("platformKey = %q", got)
	}
}

// newRegistryServer serves a registry index with an npx agent and a
// binary agent whose archive is served by the same server.
func newRegistryServer(t *testing.T, archive []byte) (*httptest.Server, *Entry) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/registry.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Index{Version: "1.0.0", Agents: []Entry{
			{
				ID: "npx-agent", Name: "Npx Agent", Version: "1.0.0",
				Distribution: Distribution{Npx: &NpxDist{
					Package: "some-acp@1.0.0", Args: []string{"--acp"},
					Env: map[string]string{"FOO": "bar"},
				}},
			},
			{
				ID: "bin-agent", Name: "Binary Agent", Version: "2.0.0",
				Distribution: Distribution{Binary: map[string]*BinaryDist{
					platformKey(): {
						Archive: "http://" + r.Host + "/archive.tar.gz",
						Cmd:     "./fake-agent", Args: []string{"--flag"},
					},
				}},
			},
		}})
	})
	mux.HandleFunc("/archive.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(archive)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, nil
}

func makeArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	content := "#!/bin/sh\necho agent\n"
	if err := tw.WriteHeader(&tar.Header{
		Name: "fake-agent", Mode: 0o755, Size: int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	// Packaging tools (PyInstaller) rely on symlinked runtimes.
	if err := tw.WriteHeader(&tar.Header{
		Name: "fake-agent", Linkname: "real-agent", Typeflag: tar.TypeSymlink,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "real-agent", Mode: 0o755, Size: int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return New(srv.URL+"/registry.json", t.TempDir(), ".", "cancel", nil)
}

func TestResolveNpx(t *testing.T) {
	srv, _ := newRegistryServer(t, nil)
	c := newClient(t, srv)
	if err := c.Warmup(context.Background()); err != nil {
		t.Fatal(err)
	}
	agent, err := c.Resolve(context.Background(), "npx-agent")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Command != "npx" {
		t.Errorf("command = %q, want npx", agent.Command)
	}
	if len(agent.Args) != 3 || agent.Args[0] != "-y" || agent.Args[1] != "some-acp@1.0.0" || agent.Args[2] != "--acp" {
		t.Errorf("args = %v", agent.Args)
	}
	if len(agent.Env) != 1 || agent.Env[0] != "FOO=bar" {
		t.Errorf("env = %v", agent.Env)
	}
	if agent.Permissions != "cancel" {
		t.Errorf("permissions = %q", agent.Permissions)
	}
}

func TestResolveBinaryInstallsOnce(t *testing.T) {
	archive := makeArchive(t)
	srv, _ := newRegistryServer(t, archive)
	c := newClient(t, srv)
	if err := c.Warmup(context.Background()); err != nil {
		t.Fatal(err)
	}

	agent, err := c.Resolve(context.Background(), "bin-agent")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(agent.Command) != "fake-agent" {
		t.Errorf("command = %q", agent.Command)
	}
	if _, err := os.Stat(agent.Command); err != nil {
		t.Errorf("installed binary missing: %v", err)
	}
	// fake-agent is a symlink into the archive; it must resolve.
	linkTarget, err := os.Readlink(agent.Command)
	if err != nil {
		t.Errorf("fake-agent should be a symlink: %v", err)
	} else if linkTarget != "real-agent" {
		t.Errorf("symlink target = %q, want real-agent", linkTarget)
	}
	if _, err := os.Stat(agent.Command); err != nil {
		t.Errorf("symlinked binary does not resolve: %v", err)
	}
	if len(agent.Args) != 1 || agent.Args[0] != "--flag" {
		t.Errorf("args = %v", agent.Args)
	}

	// Second resolve must reuse the install, not download again.
	before, err := os.Stat(agent.Command)
	if err != nil {
		t.Fatal(err)
	}
	agent2, err := c.Resolve(context.Background(), "bin-agent")
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(agent2.Command)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("binary was reinstalled on second resolve")
	}
}

func TestResolveUnknown(t *testing.T) {
	srv, _ := newRegistryServer(t, nil)
	c := newClient(t, srv)
	if err := c.Warmup(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := c.Resolve(context.Background(), "nope")
	var unknown *acp.UnknownAgentError
	if !errors.As(err, &unknown) || unknown.ID != "nope" {
		t.Errorf("expected UnknownAgentError, got %v", err)
	}
}

func TestStaticAgentsTakePrecedence(t *testing.T) {
	srv, _ := newRegistryServer(t, nil)
	static := map[string]acp.Agent{
		"npx-agent": {Command: "/my/own/wrapper", Args: []string{"--acp"}},
	}
	c := New(srv.URL+"/registry.json", t.TempDir(), ".", "cancel", static)
	if err := c.Warmup(context.Background()); err != nil {
		t.Fatal(err)
	}
	agent, err := c.Resolve(context.Background(), "npx-agent")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Command != "/my/own/wrapper" {
		t.Errorf("command = %q, want static override", agent.Command)
	}
}

func TestListIncludesStaticAndRegistry(t *testing.T) {
	srv, _ := newRegistryServer(t, nil)
	static := map[string]acp.Agent{"local-agent": {Command: "x"}}
	c := New(srv.URL+"/registry.json", t.TempDir(), ".", "cancel", static)
	infos, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, i := range infos {
		ids[i.ID] = true
	}
	for _, want := range []string{"npx-agent", "bin-agent", "local-agent"} {
		if !ids[want] {
			t.Errorf("List missing %q: %+v", want, infos)
		}
	}
}

func TestExtractRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	if _, err := safeJoin(dir, ""); err != nil {
		t.Fatal(err)
	}
	// Traversal is neutralized: the target stays inside the base dir.
	got, err := safeJoin(dir, "../../etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(dir, got)
	if err != nil {
		t.Fatal(err)
	}
	if rel == ".." || (len(rel) >= 2 && rel[:2] == "..") {
		t.Errorf("safeJoin escaped dir: rel=%q", rel)
	}
}
