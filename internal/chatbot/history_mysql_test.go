//go:build ignore

// Server-side chat history (from MaynStay feature/ai-chatbot), kept for a
// later version and excluded from the default build: V1 keeps history in
// the browser. Its owner is still the MaynStay signed-in user
// (middleware.GetSignupIDFromCtx); enabling it needs a guest identity first.

package chatbot

// Integration test of the MySQL chat history repository. It runs only when
// CHAT_HISTORY_TEST_DSN points at a test database with migrations 0001 and
// 0047 applied and two signup rows, e.g.
//
//	CHAT_HISTORY_TEST_DSN='root@tcp(127.0.0.1:13399)/chat_test?parseTime=true&loc=UTC&charset=utf8mb4' \
//	CHAT_HISTORY_TEST_USERS=1,2 go test ./internal/chatbot -run MySQL
//
// It creates and deletes its own conversations only. Never point it at
// production.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

func mysqlHistory(t *testing.T) (*HistoryService, HistoryRepository, *sql.DB, int64, int64) {
	t.Helper()
	dsn := os.Getenv("CHAT_HISTORY_TEST_DSN")
	if dsn == "" {
		t.Skip("CHAT_HISTORY_TEST_DSN not set")
	}
	users := strings.Split(os.Getenv("CHAT_HISTORY_TEST_USERS"), ",")
	if len(users) != 2 {
		t.Fatal("CHAT_HISTORY_TEST_USERS must be two signup ids, e.g. 1,2")
	}
	a, _ := strconv.ParseInt(users[0], 10, 64)
	b, _ := strconv.ParseInt(users[1], 10, 64)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM chat_conversations WHERE signup_id IN (?, ?)`, a, b)
		db.Close()
	})
	_, _ = db.Exec(`DELETE FROM chat_conversations WHERE signup_id IN (?, ?)`, a, b)
	repo := NewHistoryRepository(db)
	return NewHistoryService(repo), repo, db, a, b
}

func TestMySQLHistoryRepository(t *testing.T) {
	svc, repo, db, alice, bob := mysqlHistory(t)
	ctx := context.Background()
	id := newConversationID()

	// First save creates the conversation, sets the title and preview.
	sum, saved, err := svc.Append(ctx, alice, id, []NewMessage{
		msg("m-00000001", "user", "Show me hotels in Agra"),
		{ClientID: "m-00000002", Role: "assistant", Content: "Sure! Here are some hotels available in Agra.",
			Metadata: &MessageMeta{Hotels: []MetaHotel{{ID: 41, Name: "Hotel The First"}}}},
	})
	if err != nil || saved != 2 || sum.Title != "Hotels in Agra" || sum.MessageCount != 2 {
		t.Fatalf("first save: %v saved=%d %+v", err, saved, sum)
	}
	// A retried save is ignored; new messages keep their order.
	_, saved, err = svc.Append(ctx, alice, id, []NewMessage{msg("m-00000002", "assistant", "dup"), msg("m-00000003", "user", "Card 4111 1111 1111 1111")})
	if err != nil || saved != 1 {
		t.Fatalf("retry: %v saved=%d", err, saved)
	}
	_, msgs, _, err := svc.Messages(ctx, alice, id, 0, 0)
	if err != nil || len(msgs) != 3 || msgs[0].Content != "Show me hotels in Agra" || msgs[2].Content != "Card [card number removed]" {
		t.Fatalf("messages: %v %+v", err, msgs)
	}
	if msgs[1].Metadata == nil || msgs[1].Metadata.Hotels[0].Name != "Hotel The First" {
		t.Fatalf("metadata: %+v", msgs[1].Metadata)
	}
	var count int
	_ = db.QueryRow(`SELECT message_count FROM chat_conversations WHERE conversation_id = ?`, id).Scan(&count)
	if count != 3 {
		t.Fatalf("message_count = %d, want 3", count)
	}

	// Bob can't see or touch it; his save to Alice's id is refused.
	if _, _, _, err := svc.Messages(ctx, bob, id, 0, 0); !errors.Is(err, ErrHistoryNotFound) {
		t.Fatalf("bob read alice: %v", err)
	}
	if _, _, err := svc.Append(ctx, bob, id, []NewMessage{msg("m-00000009", "user", "injected")}); !errors.Is(err, ErrHistoryNotFound) {
		t.Fatalf("bob wrote to alice: %v", err)
	}
	if _, err := svc.Rename(ctx, bob, id, "mine"); !errors.Is(err, ErrHistoryNotFound) {
		t.Fatalf("bob renamed alice: %v", err)
	}
	if err := svc.Delete(ctx, bob, id); !errors.Is(err, ErrHistoryNotFound) {
		t.Fatalf("bob deleted alice: %v", err)
	}

	// Model context: saved and loaded for the owner, never overwritten by
	// anyone else using the same id.
	if err := repo.SaveContext(ctx, id, alice, []Turn{{Role: "user", Text: "hi"}, {Role: "assistant", Text: "hello"}}); err != nil {
		t.Fatal(err)
	}
	_ = repo.SaveContext(ctx, id, bob, []Turn{{Role: "user", Text: "hijack"}})
	owner, turns, found, err := repo.LoadContext(ctx, id)
	if err != nil || !found || owner != alice || len(turns) != 2 || turns[0].Text != "hi" {
		t.Fatalf("context: %v %v owner=%d %+v", err, found, owner, turns)
	}

	// Rename (same title twice is fine), then delete removes the messages too.
	if _, err := svc.Rename(ctx, alice, id, "Agra trip"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Rename(ctx, alice, id, "Agra trip"); err != nil {
		t.Fatalf("unchanged rename reported as error: %v", err)
	}
	_, _, _ = svc.Append(ctx, alice, id, []NewMessage{msg("m-00000004", "user", "One more")})
	if s, _ := svc.repo.Summary(ctx, id, alice); s.Title != "Agra trip" {
		t.Fatalf("custom title replaced: %q", s.Title)
	}
	if err := svc.Delete(ctx, alice, id); err != nil {
		t.Fatal(err)
	}
	_ = db.QueryRow(`SELECT COUNT(*) FROM chat_messages WHERE conversation_id = ?`, id).Scan(&count)
	if count != 0 {
		t.Fatalf("%d messages left after delete", count)
	}
}

func TestMySQLHistoryListSearchPaging(t *testing.T) {
	svc, _, db, alice, bob := mysqlHistory(t)
	ctx := context.Background()
	for i := 0; i < 23; i++ {
		text := fmt.Sprintf("Show hotels in Agra %d", i)
		if i%4 == 0 {
			text = fmt.Sprintf("Cancel 100%% of booking_%d", i) // LIKE wildcards must be literal
		}
		if _, _, err := svc.Append(ctx, alice, newConversationID(), []NewMessage{msg(fmt.Sprintf("m-%08d", i), "user", text)}); err != nil {
			t.Fatal(err)
		}
	}
	_, _, _ = svc.Append(ctx, bob, newConversationID(), []NewMessage{msg("m-99999999", "user", "Show hotels in Agra (bob)")})
	// An empty conversation (no messages yet) isn't listed.
	_, _ = db.Exec(`INSERT INTO chat_conversations (conversation_id, signup_id) VALUES (?, ?)`, newConversationID(), alice)

	seen := map[string]bool{}
	cursor, pages := "", 0
	for {
		list, next, err := svc.List(ctx, alice, "", cursor, 10)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, c := range list {
			if seen[c.ID] || strings.Contains(c.Preview, "bob") {
				t.Fatalf("repeat or other user's conversation: %+v", c)
			}
			seen[c.ID] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 23 || pages != 3 {
		t.Fatalf("paged %d in %d pages", len(seen), pages)
	}
	hits, _, _ := svc.List(ctx, alice, "100%", "", 50)
	if len(hits) != 6 {
		t.Fatalf("search '100%%' found %d, want 6 (wildcards not escaped?)", len(hits))
	}
	// "_" is literal: "a_1" appears nowhere, though as a wildcard it would
	// match "Agra 1..." in many titles.
	hits, _, _ = svc.List(ctx, alice, "a_1", "", 50)
	if len(hits) != 0 {
		t.Fatalf("search 'a_1' found %d, want 0 (underscore treated as a wildcard)", len(hits))
	}
	hits, _, _ = svc.List(ctx, alice, "booking_16", "", 50)
	if len(hits) != 1 {
		t.Fatalf("search 'booking_16' found %d, want 1", len(hits))
	}
}

// Concurrent saves of the same messages (a retry racing the original) store
// each message once and keep the count right.
func TestMySQLHistoryConcurrentSaves(t *testing.T) {
	svc, _, db, alice, _ := mysqlHistory(t)
	ctx := context.Background()
	id := newConversationID()
	batch := []NewMessage{msg("m-00000001", "user", "Hello"), msg("m-00000002", "assistant", "Hi!")}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _, _ = svc.Append(ctx, alice, id, batch) }()
	}
	wg.Wait()
	var rows, count int
	_ = db.QueryRow(`SELECT COUNT(*) FROM chat_messages WHERE conversation_id = ?`, id).Scan(&rows)
	_ = db.QueryRow(`SELECT message_count FROM chat_conversations WHERE conversation_id = ?`, id).Scan(&count)
	if rows != 2 || count != 2 {
		t.Fatalf("rows=%d message_count=%d, want 2 and 2", rows, count)
	}
}
