package openai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"acp-proxy/internal/acp"
)

// turn is one conversation entry in a stored transcript.
type turn struct {
	Role string `json:"role"` // user, assistant, system, tool
	Text string `json:"text"`
}

// storedResponse is one stored Responses API record: the ACP session
// the response ran on, the response itself, and the conversation
// transcript accumulated so far.
type storedResponse struct {
	AgentID    string             `json:"agent_id"`
	Session    *acp.Session       `json:"-"` // nil after a restart
	Response   *responsesResponse `json:"response"`
	Transcript []turn             `json:"transcript"`
}

// live reports whether the stored record carries a live ACP session.
func (s storedResponse) live() bool { return s.Session != nil }

// responseStore keeps the ACP session and final response for each
// created response, enabling previous_response_id follow-ups. Live
// entries are held in memory (capped, oldest evicted); every entry is
// also written to disk so follow-ups survive a proxy restart — replayed
// into a fresh ACP session instead of the original one.
type responseStore struct {
	mu    sync.Mutex
	order []string
	items map[string]storedResponse
	limit int
	// dir is the on-disk persistence directory; empty disables disk
	// persistence.
	dir string
}

func newResponseStore(limit int, dir string) *responseStore {
	return &responseStore{items: make(map[string]storedResponse), limit: limit, dir: dir}
}

// get returns the stored response: from memory when the session is
// still live, otherwise from disk (with a nil session, marking a cold
// follow-up).
func (s *responseStore) get(id string) (storedResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item, ok := s.items[id]; ok {
		return item, true
	}
	return s.readFromDisk(id)
}

func (s *responseStore) put(id string, item storedResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[id] = item
	s.order = append(s.order, id)
	for len(s.order) > s.limit {
		oldest := s.order[0]
		s.order = s.order[1:]
		if evicted, ok := s.items[oldest]; ok {
			if evicted.Session != nil {
				evicted.Session.Close()
			}
			delete(s.items, oldest)
		}
	}
	s.writeToDisk(id, item)
}

func (s *responseStore) readFromDisk(id string) (storedResponse, bool) {
	if s.dir == "" {
		return storedResponse{}, false
	}
	data, err := os.ReadFile(s.diskPath(id))
	if err != nil {
		return storedResponse{}, false
	}
	var rec storedResponse
	if err := json.Unmarshal(data, &rec); err != nil {
		return storedResponse{}, false
	}
	return rec, true
}

func (s *responseStore) writeToDisk(id string, item storedResponse) {
	if s.dir == "" {
		return
	}
	data, err := json.Marshal(item)
	if err != nil {
		return
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(s.diskPath(id), data, 0o644)
}

func (s *responseStore) diskPath(id string) string {
	// Response ids are proxy-generated hex; sanitize anyway.
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return -1
		}
	}, id)
	return filepath.Join(s.dir, safe+".json")
}
