//go:build ignore

// Server-side chat history (from MaynStay feature/ai-chatbot), kept for a
// later version and excluded from the default build: V1 keeps history in
// the browser. Its owner is still the MaynStay signed-in user
// (middleware.GetSignupIDFromCtx); enabling it needs a guest identity first.

package chatbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maynstayy/ai-chatbot-backend/internal/middleware"
	"github.com/Maynstayy/ai-chatbot-backend/pkg/cache"
)

// memHistory mirrors the MySQL repository's rules: rows scoped by owner,
// idempotent client ids, message order by id, newest-first listing.
type memHistory struct {
	mu    sync.Mutex
	convs map[string]*memConv
	seq   int64
	clock time.Time
	fail  error // every call fails with this when set
}

type memConv struct {
	sum     ConversationSummary
	owner   int64
	custom  bool
	context []Turn
	msgs    []StoredMessage
}

func newMemHistory() *memHistory {
	return &memHistory{convs: map[string]*memConv{}, clock: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}
}

func (m *memHistory) tick() time.Time { m.clock = m.clock.Add(time.Second); return m.clock }

func (m *memHistory) own(id string, owner int64) (*memConv, error) {
	c, ok := m.convs[id]
	if !ok || c.owner != owner {
		return nil, ErrHistoryNotFound
	}
	return c, nil
}

func (m *memHistory) ensure(id string, owner int64) {
	if _, ok := m.convs[id]; !ok {
		now := m.tick()
		m.convs[id] = &memConv{owner: owner, sum: ConversationSummary{ID: id, Title: defaultChatTitle, CreatedAt: now, UpdatedAt: now}}
	}
}

func (m *memHistory) Create(_ context.Context, id string, owner int64) (*ConversationSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	m.ensure(id, owner)
	s := m.convs[id].sum
	return &s, nil
}

func (m *memHistory) Summary(_ context.Context, id string, owner int64) (*ConversationSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	c, err := m.own(id, owner)
	if err != nil {
		return nil, err
	}
	s := c.sum
	return &s, nil
}

func (m *memHistory) List(_ context.Context, owner int64, search string, after *HistoryCursor, limit int) ([]ConversationSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	var out []ConversationSummary
	for _, c := range m.convs {
		if c.owner != owner || c.sum.MessageCount == 0 {
			continue
		}
		if after != nil && !(c.sum.UpdatedAt.Before(after.UpdatedAt) || (c.sum.UpdatedAt.Equal(after.UpdatedAt) && c.sum.ID < after.ID)) {
			continue
		}
		if search != "" {
			hit := strings.Contains(strings.ToLower(c.sum.Title), strings.ToLower(search))
			for _, msg := range c.msgs {
				hit = hit || strings.Contains(strings.ToLower(msg.Content), strings.ToLower(search))
			}
			if !hit {
				continue
			}
		}
		out = append(out, c.sum)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID > out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memHistory) Messages(_ context.Context, id string, owner int64, beforeID int64, limit int) ([]StoredMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	c, err := m.own(id, owner)
	if err != nil {
		return []StoredMessage{}, nil // the SQL join returns no rows for another owner
	}
	var page []StoredMessage
	for i := len(c.msgs) - 1; i >= 0 && len(page) < limit; i-- {
		if beforeID == 0 || c.msgs[i].ID < beforeID {
			page = append([]StoredMessage{c.msgs[i]}, page...)
		}
	}
	return page, nil
}

func (m *memHistory) Append(_ context.Context, id string, owner int64, msgs []NewMessage, title string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return 0, m.fail
	}
	m.ensure(id, owner)
	c, err := m.own(id, owner)
	if err != nil {
		return 0, err
	}
	before := c.sum.MessageCount
	saved, last := 0, ""
	for _, n := range msgs {
		dup := false
		for _, have := range c.msgs {
			dup = dup || have.ClientID == n.ClientID
		}
		if dup {
			continue
		}
		m.seq++
		c.msgs = append(c.msgs, StoredMessage{ID: m.seq, ClientID: n.ClientID, Role: n.Role, Content: n.Content, Metadata: n.Metadata, CreatedAt: m.clock})
		saved++
		last = n.Content
	}
	if saved > 0 {
		c.sum.MessageCount += saved
		c.sum.Preview = chatPreview(last)
		c.sum.UpdatedAt = m.tick()
		if before == 0 && !c.custom {
			c.sum.Title = title
		}
	}
	return saved, nil
}

func (m *memHistory) Rename(_ context.Context, id string, owner int64, title string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	c, err := m.own(id, owner)
	if err != nil {
		return err
	}
	c.sum.Title, c.custom = title, true
	return nil
}

func (m *memHistory) Delete(_ context.Context, id string, owner int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	if _, err := m.own(id, owner); err != nil {
		return err
	}
	delete(m.convs, id)
	return nil
}

func (m *memHistory) LoadContext(_ context.Context, id string) (int64, []Turn, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return 0, nil, false, m.fail
	}
	c, ok := m.convs[id]
	if !ok {
		return 0, nil, false, nil
	}
	return c.owner, append([]Turn(nil), c.context...), true, nil
}

func (m *memHistory) SaveContext(_ context.Context, id string, owner int64, turns []Turn) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.ensure(id, owner)
	if c := m.convs[id]; c.owner == owner {
		c.context = append([]Turn(nil), turns...)
	}
	return nil
}

const (
	convA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	convB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func msg(id, role, content string) NewMessage {
	return NewMessage{ClientID: id, Role: role, Content: content}
}

// --- service ------------------------------------------------------------------

func TestHistorySaveListReopen(t *testing.T) {
	repo := newMemHistory()
	svc := NewHistoryService(repo)
	ctx := context.Background()

	sum, saved, err := svc.Append(ctx, 7, convA, []NewMessage{
		msg("m-00000001", "user", "Show me hotels in Agra"),
		{ClientID: "m-00000002", Role: "assistant", Content: "Sure! Here are some hotels available in Agra.",
			Metadata: &MessageMeta{Intent: "hotel_search", Hotels: []MetaHotel{{ID: 41, Name: "Hotel The First"}}}},
	})
	if err != nil || saved != 2 {
		t.Fatalf("append: %v saved=%d", err, saved)
	}
	if sum.Title != "Hotels in Agra" || sum.Preview != "Sure! Here are some hotels available in Agra." || sum.MessageCount != 2 {
		t.Fatalf("summary: %+v", sum)
	}

	// A retried save of the same messages adds nothing; a new one is appended in order.
	_, saved, _ = svc.Append(ctx, 7, convA, []NewMessage{msg("m-00000002", "assistant", "dup"), msg("m-00000003", "user", "Which has parking?")})
	if saved != 1 {
		t.Fatalf("retry duplicated messages: saved=%d", saved)
	}
	_, msgs, more, err := svc.Messages(ctx, 7, convA, 0, 0)
	if err != nil || more || len(msgs) != 3 {
		t.Fatalf("messages: %v more=%v %+v", err, more, msgs)
	}
	for i, want := range []string{"Show me hotels in Agra", "Sure! Here are some hotels available in Agra.", "Which has parking?"} {
		if msgs[i].Content != want {
			t.Errorf("message %d = %q, want %q (chronological order)", i, msgs[i].Content, want)
		}
	}
	if msgs[1].Metadata == nil || msgs[1].Metadata.Hotels[0].ID != 41 {
		t.Errorf("metadata lost: %+v", msgs[1].Metadata)
	}
	// The first message set the title; later ones don't change it.
	list, _, _ := svc.List(ctx, 7, "", "", 0)
	if len(list) != 1 || list[0].Title != "Hotels in Agra" || list[0].Preview != "Which has parking?" {
		t.Fatalf("list: %+v", list)
	}
}

func TestHistoryOwnershipIsolation(t *testing.T) {
	repo := newMemHistory()
	svc := NewHistoryService(repo)
	ctx := context.Background()
	_, _, _ = svc.Append(ctx, 7, convA, []NewMessage{msg("m-00000001", "user", "My private question")})

	// Another guest can't read, list, write to, rename or delete it: always "not found".
	if _, _, _, err := svc.Messages(ctx, 8, convA, 0, 0); !errors.Is(err, ErrHistoryNotFound) {
		t.Errorf("read other's conversation: %v", err)
	}
	if list, _, _ := svc.List(ctx, 8, "", "", 0); len(list) != 0 {
		t.Errorf("listed other's conversation: %+v", list)
	}
	if _, _, err := svc.Append(ctx, 8, convA, []NewMessage{msg("m-00000009", "user", "injected")}); !errors.Is(err, ErrHistoryNotFound) {
		t.Errorf("wrote to other's conversation: %v", err)
	}
	if _, err := svc.Rename(ctx, 8, convA, "hijacked"); !errors.Is(err, ErrHistoryNotFound) {
		t.Errorf("renamed other's conversation: %v", err)
	}
	if err := svc.Delete(ctx, 8, convA); !errors.Is(err, ErrHistoryNotFound) {
		t.Errorf("deleted other's conversation: %v", err)
	}
	if _, msgs, _, _ := svc.Messages(ctx, 7, convA, 0, 0); len(msgs) != 1 || msgs[0].Content != "My private question" {
		t.Fatalf("owner's conversation changed: %+v", msgs)
	}
	// Guests (no signup id) have no history at all.
	if _, _, err := svc.List(ctx, 0, "", "", 0); err == nil {
		t.Error("guest listed history")
	}
}

func TestHistoryRenameDelete(t *testing.T) {
	repo := newMemHistory()
	svc := NewHistoryService(repo)
	ctx := context.Background()
	_, _, _ = svc.Append(ctx, 7, convA, []NewMessage{msg("m-00000001", "user", "Customer support")})

	sum, err := svc.Rename(ctx, 7, convA, "  Booking   help  ")
	if err != nil || sum.Title != "Booking help" {
		t.Fatalf("rename: %v %+v", err, sum)
	}
	// A renamed title is kept as messages are added.
	_, _, _ = svc.Append(ctx, 7, convA, []NewMessage{msg("m-00000002", "user", "Show hotels in Agra")})
	if s, _ := repo.Summary(ctx, convA, 7); s.Title != "Booking help" {
		t.Fatalf("custom title overwritten: %q", s.Title)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("x", 121)} {
		if _, err := svc.Rename(ctx, 7, convA, bad); !errors.As(err, new(HistoryInputError)) {
			t.Errorf("rename %q accepted: %v", bad, err)
		}
	}
	if err := svc.Delete(ctx, 7, convA); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := svc.Messages(ctx, 7, convA, 0, 0); !errors.Is(err, ErrHistoryNotFound) {
		t.Fatalf("deleted conversation still readable: %v", err)
	}
}

func TestHistorySearchAndPagination(t *testing.T) {
	repo := newMemHistory()
	svc := NewHistoryService(repo)
	ctx := context.Background()
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("%032x", i+1)
		text := "Show hotels in Agra"
		if i%5 == 0 {
			text = "Cancel my booking please"
		}
		_, _, _ = svc.Append(ctx, 7, id, []NewMessage{msg(fmt.Sprintf("m-%08d", i), "user", text)})
	}
	// Pages of 10, newest first, no repeats, until the cursor runs out.
	seen := map[string]bool{}
	cursor, pages := "", 0
	var prev time.Time
	for {
		list, next, err := svc.List(ctx, 7, "", cursor, 10)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, c := range list {
			if seen[c.ID] {
				t.Fatalf("conversation %s repeated across pages", c.ID)
			}
			if !prev.IsZero() && c.UpdatedAt.After(prev) {
				t.Fatalf("not newest first")
			}
			seen[c.ID], prev = true, c.UpdatedAt
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 25 || pages != 3 {
		t.Fatalf("paged %d conversations in %d pages", len(seen), pages)
	}
	// Search matches titles and message text, case-insensitively.
	hits, _, _ := svc.List(ctx, 7, "CANCEL", "", 50)
	if len(hits) != 5 {
		t.Fatalf("search found %d, want 5", len(hits))
	}
	if _, _, err := svc.List(ctx, 7, "", "not-a-cursor", 10); !errors.As(err, new(HistoryInputError)) {
		t.Fatalf("bad cursor accepted: %v", err)
	}

	// Long conversations page backwards through older messages.
	var batch []NewMessage
	for i := 0; i < 20; i++ {
		batch = append(batch, msg(fmt.Sprintf("p-%08d", i), "user", "message "+strconv.Itoa(i)))
	}
	_, _, _ = svc.Append(ctx, 7, convB, batch)
	_, page1, more, _ := svc.Messages(ctx, 7, convB, 0, 8)
	_, page2, _, _ := svc.Messages(ctx, 7, convB, page1[0].ID, 8)
	if !more || page1[0].Content != "message 12" || page1[7].Content != "message 19" || page2[7].Content != "message 11" {
		t.Fatalf("message paging: %v %q..%q / %q", more, page1[0].Content, page1[7].Content, page2[7].Content)
	}
}

func TestHistoryValidationAndMasking(t *testing.T) {
	svc := NewHistoryService(newMemHistory())
	ctx := context.Background()
	bad := [][]NewMessage{
		nil,
		{msg("short", "user", "hi")},
		{msg("m-00000001", "system", "hi")},
		{msg("m-00000001", "user", "   ")},
		make([]NewMessage, 21),
	}
	for i, b := range bad {
		if _, _, err := svc.Append(ctx, 7, convA, b); !errors.As(err, new(HistoryInputError)) {
			t.Errorf("bad batch %d accepted: %v", i, err)
		}
	}
	if _, _, err := svc.Append(ctx, 7, "not-an-id", []NewMessage{msg("m-00000001", "user", "hi")}); err == nil {
		t.Error("invalid conversation id accepted")
	}

	_, _, err := svc.Append(ctx, 7, convA, []NewMessage{
		msg("m-00000001", "user", "My card is 4111 1111 1111 1111 and password: hunter2, OTP 482913, token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiI3In0.abcdefghijklmnop"),
		msg("m-00000002", "user", "Booking 100, call me on 9876543210"),
		msg("m-00000003", "user", strings.Repeat("a", 5000)),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, msgs, _, _ := svc.Messages(ctx, 7, convA, 0, 0)
	stored := msgs[0].Content
	for _, leaked := range []string{"4111", "hunter2", "482913", "eyJhbGci"} {
		if strings.Contains(stored, leaked) {
			t.Errorf("stored sensitive %q: %s", leaked, stored)
		}
	}
	if !strings.Contains(stored, "[card number removed]") || !strings.Contains(stored, "password [removed]") {
		t.Errorf("masking: %s", stored)
	}
	// Booking ids and phone numbers are not card numbers.
	if msgs[1].Content != "Booking 100, call me on 9876543210" {
		t.Errorf("over-masked: %s", msgs[1].Content)
	}
	if len([]rune(msgs[2].Content)) != maxHistoryMessageRune {
		t.Errorf("long message not capped: %d", len([]rune(msgs[2].Content)))
	}
}

func TestAutoTitle(t *testing.T) {
	cases := map[string]string{
		"Show me hotels in Agra":                      "Hotels in Agra",
		"Can you please find me a hotel in Varanasi?": "Hotel in Varanasi",
		"Customer support":                            "Customer support",
		"How do I cancel my booking?":                 "How do I cancel my booking",
		"hi":                                          "Hi",
		strings.Repeat("very long words ", 10):        "Very long words very long words very long words very long…",
	}
	for in, want := range cases {
		if got := autoTitle(in); got != want {
			t.Errorf("autoTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHistoryDatabaseFailure(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	repo := newMemHistory()
	repo.fail = errors.New("dial tcp 127.0.0.1:13306: connection refused")
	svc := NewHistoryService(repo)
	_, _, err := svc.Append(context.Background(), 7, convA, []NewMessage{msg("m-00000001", "user", "secret plans for my stay")})
	if !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("database failure not reported as unavailable: %v", err)
	}
	if strings.Contains(buf.String(), "secret plans") {
		t.Fatalf("message content logged: %s", buf.String())
	}
}

// --- handlers -------------------------------------------------------------------

// testAuth stands in for RequireAuth: X-Test-User is the verified signup id.
func testAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.Header.Get("X-Test-User"))
		if id <= 0 {
			http.Error(w, `{"error":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), middleware.ContextKeySignupID, id)))
	}
}

func historyMux(repo HistoryRepository) *http.ServeMux {
	mux := http.NewServeMux()
	RegisterHistoryRoutes(mux, NewHistoryHandler(NewHistoryService(repo), NewRateLimiter(cache.New(""), 1000)), testAuth)
	return mux
}

func call(mux http.Handler, user int, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if user > 0 {
		req.Header.Set("X-Test-User", strconv.Itoa(user))
	}
	req.RemoteAddr = "203.0.113.9:5000"
	mux.ServeHTTP(rec, req)
	return rec
}

func TestHistoryHandlers(t *testing.T) {
	repo := newMemHistory()
	mux := historyMux(repo)

	// Signed out: 401 everywhere.
	if rec := call(mux, 0, "GET", "/api/v1/chatbot/conversations", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: %d", rec.Code)
	}

	// Create, save, list, read.
	rec := call(mux, 7, "POST", "/api/v1/chatbot/conversations", "")
	var created struct {
		Data struct {
			Conversation ConversationSummary `json:"conversation"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created.Data.Conversation.ID
	if rec.Code != http.StatusCreated || !conversationIDPattern.MatchString(id) {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	body := `{"messages":[{"client_message_id":"m-00000001","role":"user","content":"Show me hotels in Agra"},{"client_message_id":"m-00000002","role":"assistant","content":"Sure!","metadata":{"hotels":[{"id":41,"name":"Hotel The First"}],"evil":"x"}}]}`
	if rec := call(mux, 7, "POST", "/api/v1/chatbot/conversations/"+id+"/messages", body); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"saved":2`) {
		t.Fatalf("append: %d %s", rec.Code, rec.Body)
	}
	rec = call(mux, 7, "GET", "/api/v1/chatbot/conversations?q=agra", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"title":"Hotels in Agra"`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	rec = call(mux, 7, "GET", "/api/v1/chatbot/conversations/"+id+"/messages", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"Show me hotels in Agra"`) || strings.Contains(rec.Body.String(), "evil") {
		t.Fatalf("messages: %d %s", rec.Code, rec.Body)
	}

	// Another guest: 404 for every operation on this conversation.
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/chatbot/conversations/" + id + "/messages", ""},
		{"POST", "/api/v1/chatbot/conversations/" + id + "/messages", body},
		{"PUT", "/api/v1/chatbot/conversations/" + id, `{"title":"mine now"}`},
		{"DELETE", "/api/v1/chatbot/conversations/" + id, ""},
	} {
		if rec := call(mux, 8, c.method, c.path, c.body); rec.Code != http.StatusNotFound {
			t.Errorf("other guest %s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}

	// Rename and delete by the owner.
	if rec := call(mux, 7, "PUT", "/api/v1/chatbot/conversations/"+id, `{"title":"Agra trip"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"title":"Agra trip"`) {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body)
	}
	if rec := call(mux, 7, "DELETE", "/api/v1/chatbot/conversations/"+id, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := call(mux, 7, "GET", "/api/v1/chatbot/conversations/"+id+"/messages", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted conversation: %d", rec.Code)
	}

	// Bad input and a broken database get clear, safe errors.
	if rec := call(mux, 7, "POST", "/api/v1/chatbot/conversations/"+convA+"/messages", `{"messages":[{"client_message_id":"x","role":"user","content":"hi"}]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid message: %d", rec.Code)
	}
	repo.fail = errors.New("Error 1146: Table 'chat_messages' doesn't exist")
	rec = call(mux, 7, "GET", "/api/v1/chatbot/conversations", "")
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "1146") || strings.Contains(rec.Body.String(), "chat_messages") {
		t.Fatalf("database failure: %d %s", rec.Code, rec.Body)
	}
}

// --- continuing conversations ---------------------------------------------------

// A signed-in guest's conversation keeps the website's id and its model
// context survives the short-lived store being lost (restart, new device).
func TestContinueConversationAcrossRestart(t *testing.T) {
	repo := newMemHistory()
	f := newFixture()
	f.svc.store = NewPersistentConversationStore(&hybridStore{redis: cache.New(""), mem: newMemoryStore()}, repo)

	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "Show me hotels in Haridwar", ConversationID: convA, UserID: 7})
	if err != nil || out.ConversationID != convA {
		t.Fatalf("first message: %v id=%s", err, out.ConversationID)
	}
	// "Restart": a fresh short-lived store; the context comes back from MySQL.
	f.svc.store = NewPersistentConversationStore(&hybridStore{redis: cache.New(""), mem: newMemoryStore()}, repo)
	f.ai.reply = "It has parking."
	out, err = f.svc.HandleMessage(context.Background(), Input{Message: "Does it have parking?", ConversationID: convA, UserID: 7})
	if err != nil || out.ConversationID != convA {
		t.Fatalf("continue: %v id=%s", err, out.ConversationID)
	}
	if n := len(f.ai.lastReq.History); n < 3 {
		t.Fatalf("model context not restored: %d turns", n)
	}

	// Another guest using the same id gets a fresh conversation, not the context.
	out, _ = f.svc.HandleMessage(context.Background(), Input{Message: "Does it have parking?", ConversationID: convA, UserID: 8})
	if out.ConversationID == convA || len(f.ai.lastReq.History) != 1 {
		t.Fatalf("another guest reached the conversation: id=%s history=%d", out.ConversationID, len(f.ai.lastReq.History))
	}
	if _, turns, _, _ := repo.LoadContext(context.Background(), convA); len(turns) == 0 || turns[len(turns)-1].Text == "Does it have parking?" && len(turns) < 3 {
		t.Fatalf("owner's context overwritten: %+v", turns)
	}

	// Guests (not signed in) never get a permanent conversation.
	out, _ = f.svc.HandleMessage(context.Background(), Input{Message: "Show me hotels in Haridwar", ConversationID: convB})
	if out.ConversationID == convB {
		t.Fatal("guest adopted a client id")
	}
	if _, _, found, _ := repo.LoadContext(context.Background(), out.ConversationID); found {
		t.Fatal("guest conversation stored permanently")
	}
}

// With MySQL down, the chat still answers from the short-lived store.
func TestChatWorksWhenHistoryDatabaseFails(t *testing.T) {
	repo := newMemHistory()
	repo.fail = errors.New("connection refused")
	f := newFixture()
	mem := newMemoryStore()
	mem.now = f.svc.now // the fixture's clock, so the cached context isn't "expired"
	f.svc.store = NewPersistentConversationStore(&hybridStore{redis: cache.New(""), mem: mem}, repo)
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "Show me hotels in Haridwar", ConversationID: convA, UserID: 7})
	if err != nil || len(out.Hotels) == 0 || out.ConversationID != convA {
		t.Fatalf("chat failed with the database down: %v %+v", err, out)
	}
	f.ai.reply = "Yes."
	if _, err := f.svc.HandleMessage(context.Background(), Input{Message: "Does it have parking?", ConversationID: convA, UserID: 7}); err != nil || len(f.ai.lastReq.History) < 3 {
		t.Fatalf("context lost with the database down: %v %d", err, len(f.ai.lastReq.History))
	}
}

func TestStoredContextIsMasked(t *testing.T) {
	repo := newMemHistory()
	store := NewPersistentConversationStore(&hybridStore{redis: cache.New(""), mem: newMemoryStore()}, repo)
	store.Save(context.Background(), &Conversation{ID: convA, OwnerID: 7, Turns: []Turn{{Role: "user", Text: "card 4111111111111111 please"}}})
	_, turns, _, _ := repo.LoadContext(context.Background(), convA)
	if len(turns) != 1 || strings.Contains(turns[0].Text, "4111") {
		t.Fatalf("context stored unmasked: %+v", turns)
	}
}
