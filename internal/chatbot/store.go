package chatbot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sync"
	"time"

	"github.com/Maynstayy/ai-chatbot-backend/pkg/cache"
)

const (
	// conversationTTL is how long an idle conversation is remembered.
	conversationTTL = 30 * time.Minute
	// maxStoredTurns keeps only the recent context (user + assistant turns).
	maxStoredTurns = 12
	// maxStoredTurnRunes trims each stored turn.
	maxStoredTurnRunes = 2000
	// maxMemoryConversations bounds the in-memory fallback.
	maxMemoryConversations = 5000
)

var conversationIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

// Conversation is the short-lived chat context. OwnerID is the signed-in
// user (0 for a guest); a conversation is only ever continued by its owner.
// Only message text is kept - no tool results, booking data or tokens.
type Conversation struct {
	ID        string    `json:"id"`
	OwnerID   int64     `json:"owner_id"`
	Turns     []Turn    `json:"turns"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ConversationStore keeps conversations between messages.
type ConversationStore interface {
	Get(ctx context.Context, id string) (*Conversation, bool)
	Save(ctx context.Context, c *Conversation)
}

func newConversationID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// trimTurns keeps the newest turns, starting on a user turn.
func trimTurns(turns []Turn) []Turn {
	if len(turns) > maxStoredTurns {
		turns = turns[len(turns)-maxStoredTurns:]
	}
	for len(turns) > 0 && turns[0].Role != "user" {
		turns = turns[1:]
	}
	out := make([]Turn, len(turns))
	for i, t := range turns {
		if r := []rune(t.Text); len(r) > maxStoredTurnRunes {
			t.Text = string(r[:maxStoredTurnRunes])
		}
		out[i] = t
	}
	return out
}

// hybridStore uses Redis when configured (shared by every app instance,
// survives restarts) and falls back to process memory, like the auth module's
// login limiter.
type hybridStore struct {
	redis *cache.Cache
	mem   *memoryStore
}

// NewConversationStore returns the store used by the app.
func NewConversationStore(redis *cache.Cache) ConversationStore {
	return &hybridStore{redis: redis, mem: newMemoryStore()}
}

func redisKey(id string) string { return "chatbot:conv:" + id }

func (s *hybridStore) Get(ctx context.Context, id string) (*Conversation, bool) {
	if s.redis.Enabled() {
		v, found, err := s.redis.Get(ctx, redisKey(id))
		if err == nil {
			if !found {
				return nil, false
			}
			var c Conversation
			if json.Unmarshal([]byte(v), &c) != nil {
				return nil, false
			}
			return &c, true
		}
	}
	return s.mem.Get(ctx, id)
}

func (s *hybridStore) Save(ctx context.Context, c *Conversation) {
	if s.redis.Enabled() {
		if b, err := json.Marshal(c); err == nil {
			if s.redis.Set(ctx, redisKey(c.ID), string(b), conversationTTL) == nil {
				return
			}
		}
	}
	s.mem.Save(ctx, c)
}

type memoryStore struct {
	mu    sync.Mutex
	items map[string]*Conversation
	now   func() time.Time
}

func newMemoryStore() *memoryStore {
	return &memoryStore{items: map[string]*Conversation{}, now: time.Now}
}

func (m *memoryStore) Get(_ context.Context, id string) (*Conversation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.items[id]
	if !ok {
		return nil, false
	}
	if m.now().Sub(c.UpdatedAt) > conversationTTL {
		delete(m.items, id)
		return nil, false
	}
	cp := *c
	cp.Turns = append([]Turn(nil), c.Turns...)
	return &cp, true
}

func (m *memoryStore) Save(_ context.Context, c *Conversation) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if _, exists := m.items[c.ID]; !exists && len(m.items) >= maxMemoryConversations {
		var oldestID string
		var oldest time.Time
		for id, v := range m.items {
			if now.Sub(v.UpdatedAt) > conversationTTL {
				delete(m.items, id)
				continue
			}
			if oldestID == "" || v.UpdatedAt.Before(oldest) {
				oldestID, oldest = id, v.UpdatedAt
			}
		}
		if len(m.items) >= maxMemoryConversations && oldestID != "" {
			delete(m.items, oldestID)
		}
	}
	cp := *c
	cp.Turns = append([]Turn(nil), c.Turns...)
	m.items[c.ID] = &cp
}
