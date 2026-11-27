//go:build ignore

// Server-side chat history (from MaynStay feature/ai-chatbot), kept for a
// later version and excluded from the default build: V1 keeps history in
// the browser. Its owner is still the MaynStay signed-in user
// (middleware.GetSignupIDFromCtx); enabling it needs a guest identity first.

package chatbot

// The model context of a signed-in guest's conversation lives in MySQL
// (chat_conversations.ai_context), so a conversation reopened days later,
// after a restart or on another device, continues where it left off. Guests'
// conversations stay in the short-lived store (Redis or memory) only. If the
// database is unavailable the short-lived store keeps the chat working.

import (
	"context"
	"log/slog"
)

type persistentStore struct {
	cache ConversationStore
	repo  HistoryRepository
}

// NewPersistentConversationStore keeps signed-in conversations' context in
// repo, with cache (the short-lived store) for guests and as a fallback.
func NewPersistentConversationStore(cache ConversationStore, repo HistoryRepository) ConversationStore {
	return &persistentStore{cache: cache, repo: repo}
}

func (s *persistentStore) Get(ctx context.Context, id string) (*Conversation, bool) {
	owner, turns, found, err := s.repo.LoadContext(ctx, id)
	switch {
	case err != nil:
		slog.Warn("chat history context load failed; using the short-lived store", "err", err.Error())
	case found:
		return &Conversation{ID: id, OwnerID: owner, Turns: turns}, true
	}
	return s.cache.Get(ctx, id)
}

func (s *persistentStore) Save(ctx context.Context, c *Conversation) {
	s.cache.Save(ctx, c)
	if c.OwnerID <= 0 {
		return
	}
	turns := make([]Turn, len(c.Turns))
	for i, t := range c.Turns {
		turns[i] = Turn{Role: t.Role, Text: redactSensitive(t.Text)}
	}
	if err := s.repo.SaveContext(ctx, c.ID, c.OwnerID, turns); err != nil {
		slog.Warn("chat history context save failed", "err", err.Error())
	}
}
