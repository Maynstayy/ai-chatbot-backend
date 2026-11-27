//go:build ignore

// Server-side chat history (from MaynStay feature/ai-chatbot), kept for a
// later version and excluded from the default build: V1 keeps history in
// the browser. Its owner is still the MaynStay signed-in user
// (middleware.GetSignupIDFromCtx); enabling it needs a guest identity first.

package chatbot

// Permanent chat history for signed-in guests: list, search, reopen,
// continue, rename and delete their own conversations (MySQL tables
// chat_conversations and chat_messages, migration 0047).
//
// Every operation is scoped to the caller's signup id; a conversation that
// isn't theirs is reported exactly like one that doesn't exist. Guests who
// aren't signed in have no permanent history.
//
// The transcript is what the guest saw in the chat window, saved by the
// website after each turn with a client message id, so a retried save never
// duplicates a message. Content is cleaned and sensitive values (card
// numbers, tokens, stated passwords and OTPs) are masked before storing.

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var (
	ErrHistoryNotFound    = errors.New("conversation not found")
	ErrHistoryUnavailable = errors.New("chat history is temporarily unavailable")
)

// HistoryInputError is a request the history API rejects as invalid.
type HistoryInputError struct{ Msg string }

func (e HistoryInputError) Error() string { return e.Msg }

const (
	defaultChatTitle      = "New chat"
	maxChatTitleRunes     = 120
	autoTitleRunes        = 60
	maxPreviewRunes       = 200
	maxHistoryMessageRune = 4000
	maxMessagesPerSave    = 20
	maxSearchRunes        = 100
	defaultListLimit      = 20
	maxListLimit          = 50
	defaultMessagesLimit  = 50
	maxMessagesLimit      = 100
	maxMetaHotels         = 5
)

// ConversationSummary is one row of the history list.
type ConversationSummary struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Preview      string    `json:"preview"`
	MessageCount int       `json:"message_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// MessageMeta is the only extra data kept with a message: display hints for
// reopening it. Never prices, availability, booking details or tool output.
type MessageMeta struct {
	Intent   string      `json:"intent,omitempty"`
	Hotels   []MetaHotel `json:"hotels,omitempty"`
	Support  bool        `json:"support,omitempty"`
	Bookings bool        `json:"bookings,omitempty"`
}

// MetaHotel is a hotel a reply showed, so a reopened chat can link to it.
type MetaHotel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// StoredMessage is one saved message.
type StoredMessage struct {
	ID        int64        `json:"id"`
	ClientID  string       `json:"client_message_id"`
	Role      string       `json:"role"`
	Content   string       `json:"content"`
	Metadata  *MessageMeta `json:"metadata,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
}

// NewMessage is a message the website asks to save.
type NewMessage struct {
	ClientID string       `json:"client_message_id"`
	Role     string       `json:"role"`
	Content  string       `json:"content"`
	Metadata *MessageMeta `json:"metadata,omitempty"`
}

// HistoryCursor is the position after the last listed conversation.
type HistoryCursor struct {
	UpdatedAt time.Time
	ID        string
}

// HistoryRepository is the storage behind the history (MySQL in the app).
// Every method that takes an owner only ever touches that owner's rows and
// returns ErrHistoryNotFound for anyone else's.
type HistoryRepository interface {
	Create(ctx context.Context, id string, owner int64) (*ConversationSummary, error)
	Summary(ctx context.Context, id string, owner int64) (*ConversationSummary, error)
	List(ctx context.Context, owner int64, search string, after *HistoryCursor, limit int) ([]ConversationSummary, error)
	Messages(ctx context.Context, id string, owner int64, beforeID int64, limit int) ([]StoredMessage, error)
	// Append saves messages in order, ignoring client ids already saved, and
	// creates the conversation for owner if it doesn't exist yet. title is
	// applied only while the conversation has no messages and no custom
	// title. It returns how many messages were new.
	Append(ctx context.Context, id string, owner int64, msgs []NewMessage, title string) (int, error)
	Rename(ctx context.Context, id string, owner int64, title string) error
	Delete(ctx context.Context, id string, owner int64) error
	// LoadContext returns a conversation's owner and model context.
	LoadContext(ctx context.Context, id string) (owner int64, turns []Turn, found bool, err error)
	// SaveContext stores the model context, creating the conversation for
	// owner if needed; it never changes anyone else's conversation.
	SaveContext(ctx context.Context, id string, owner int64, turns []Turn) error
}

// HistoryService validates and runs history operations for one owner.
type HistoryService struct {
	repo HistoryRepository
}

func NewHistoryService(repo HistoryRepository) *HistoryService { return &HistoryService{repo: repo} }

// storageErr turns a storage failure into ErrHistoryUnavailable (logged
// without any content); not-found passes through.
func storageErr(op string, err error) error {
	if err == nil || errors.Is(err, ErrHistoryNotFound) {
		return err
	}
	slog.Error("chat history storage failed", "op", op, "err", err.Error())
	return ErrHistoryUnavailable
}

func checkOwner(owner int64) error {
	if owner <= 0 {
		return HistoryInputError{"sign in to use chat history"}
	}
	return nil
}

func checkConversationID(id string) error {
	if !conversationIDPattern.MatchString(id) {
		return ErrHistoryNotFound
	}
	return nil
}

// Create starts an empty conversation for owner with a new id.
func (s *HistoryService) Create(ctx context.Context, owner int64) (*ConversationSummary, error) {
	if err := checkOwner(owner); err != nil {
		return nil, err
	}
	c, err := s.repo.Create(ctx, newConversationID(), owner)
	return c, storageErr("create", err)
}

// List returns owner's conversations, newest first, optionally matching
// search in a title or message, and the cursor for the next page.
func (s *HistoryService) List(ctx context.Context, owner int64, search, cursor string, limit int) ([]ConversationSummary, string, error) {
	if err := checkOwner(owner); err != nil {
		return nil, "", err
	}
	search = strings.Join(strings.Fields(cleanText(search)), " ")
	if len([]rune(search)) > maxSearchRunes {
		search = string([]rune(search)[:maxSearchRunes])
	}
	var after *HistoryCursor
	if cursor != "" {
		c, err := decodeHistoryCursor(cursor)
		if err != nil {
			return nil, "", HistoryInputError{"invalid cursor"}
		}
		after = c
	}
	limit = clampLimit(limit, defaultListLimit, maxListLimit)
	list, err := s.repo.List(ctx, owner, search, after, limit+1)
	if err != nil {
		return nil, "", storageErr("list", err)
	}
	next := ""
	if len(list) > limit {
		list = list[:limit]
		last := list[len(list)-1]
		next = encodeHistoryCursor(HistoryCursor{UpdatedAt: last.UpdatedAt, ID: last.ID})
	}
	if list == nil {
		list = []ConversationSummary{}
	}
	return list, next, nil
}

// Messages returns a page of a conversation's messages in chronological
// order: the newest ones, or those before beforeID. hasMore reports older
// messages.
func (s *HistoryService) Messages(ctx context.Context, owner int64, id string, beforeID int64, limit int) (*ConversationSummary, []StoredMessage, bool, error) {
	if err := checkOwner(owner); err != nil {
		return nil, nil, false, err
	}
	if err := checkConversationID(id); err != nil {
		return nil, nil, false, err
	}
	if beforeID < 0 {
		return nil, nil, false, HistoryInputError{"invalid before"}
	}
	summary, err := s.repo.Summary(ctx, id, owner)
	if err != nil {
		return nil, nil, false, storageErr("summary", err)
	}
	limit = clampLimit(limit, defaultMessagesLimit, maxMessagesLimit)
	msgs, err := s.repo.Messages(ctx, id, owner, beforeID, limit+1)
	if err != nil {
		return nil, nil, false, storageErr("messages", err)
	}
	hasMore := len(msgs) > limit
	if hasMore {
		msgs = msgs[1:] // the repository returns them oldest first
	}
	if msgs == nil {
		msgs = []StoredMessage{}
	}
	return summary, msgs, hasMore, nil
}

// Append saves messages to owner's conversation (creating it on the first
// save) and returns the updated summary and how many were new.
func (s *HistoryService) Append(ctx context.Context, owner int64, id string, msgs []NewMessage) (*ConversationSummary, int, error) {
	if err := checkOwner(owner); err != nil {
		return nil, 0, err
	}
	if err := checkConversationID(id); err != nil {
		return nil, 0, HistoryInputError{"invalid conversation id"}
	}
	clean, err := cleanNewMessages(msgs)
	if err != nil {
		return nil, 0, err
	}
	title := defaultChatTitle
	for _, m := range clean {
		if m.Role == "user" {
			title = autoTitle(m.Content)
			break
		}
	}
	saved, err := s.repo.Append(ctx, id, owner, clean, title)
	if err != nil {
		return nil, 0, storageErr("append", err)
	}
	summary, err := s.repo.Summary(ctx, id, owner)
	return summary, saved, storageErr("summary", err)
}

// Rename sets a conversation's title.
func (s *HistoryService) Rename(ctx context.Context, owner int64, id, title string) (*ConversationSummary, error) {
	if err := checkOwner(owner); err != nil {
		return nil, err
	}
	if err := checkConversationID(id); err != nil {
		return nil, err
	}
	title = strings.Join(strings.Fields(cleanText(title)), " ")
	if title == "" {
		return nil, HistoryInputError{"title is required"}
	}
	if len([]rune(title)) > maxChatTitleRunes {
		return nil, HistoryInputError{"title must be at most " + strconv.Itoa(maxChatTitleRunes) + " characters"}
	}
	if err := s.repo.Rename(ctx, id, owner, redactSensitive(title)); err != nil {
		return nil, storageErr("rename", err)
	}
	summary, err := s.repo.Summary(ctx, id, owner)
	return summary, storageErr("summary", err)
}

// Delete removes a conversation and its messages.
func (s *HistoryService) Delete(ctx context.Context, owner int64, id string) error {
	if err := checkOwner(owner); err != nil {
		return err
	}
	if err := checkConversationID(id); err != nil {
		return err
	}
	return storageErr("delete", s.repo.Delete(ctx, id, owner))
}

// --- validation and cleaning ------------------------------------------------

var (
	clientMessageIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,40}$`)
	intentPattern          = regexp.MustCompile(`^[a-z_]{1,40}$`)
)

func clampLimit(n, def, max int) int {
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// cleanText drops control characters (keeping newlines and tabs).
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return -1
		}
		return r
	}, s)
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func cleanNewMessages(msgs []NewMessage) ([]NewMessage, error) {
	if len(msgs) == 0 {
		return nil, HistoryInputError{"messages are required"}
	}
	if len(msgs) > maxMessagesPerSave {
		return nil, HistoryInputError{"at most " + strconv.Itoa(maxMessagesPerSave) + " messages per save"}
	}
	seen := map[string]bool{}
	out := make([]NewMessage, 0, len(msgs))
	for _, m := range msgs {
		if !clientMessageIDPattern.MatchString(m.ClientID) {
			return nil, HistoryInputError{"invalid client_message_id"}
		}
		if m.Role != "user" && m.Role != "assistant" {
			return nil, HistoryInputError{"role must be user or assistant"}
		}
		content := strings.TrimSpace(cleanText(m.Content))
		if content == "" {
			return nil, HistoryInputError{"content is required"}
		}
		if seen[m.ClientID] {
			continue
		}
		seen[m.ClientID] = true
		out = append(out, NewMessage{
			ClientID: m.ClientID,
			Role:     m.Role,
			Content:  truncateRunes(redactSensitive(content), maxHistoryMessageRune),
			Metadata: cleanMeta(m.Metadata),
		})
	}
	return out, nil
}

// cleanMeta keeps only valid display hints.
func cleanMeta(m *MessageMeta) *MessageMeta {
	if m == nil {
		return nil
	}
	out := MessageMeta{Support: m.Support, Bookings: m.Bookings}
	if intentPattern.MatchString(m.Intent) {
		out.Intent = m.Intent
	}
	for _, h := range m.Hotels {
		name := truncateRunes(strings.Join(strings.Fields(cleanText(h.Name)), " "), maxChatTitleRunes)
		if h.ID > 0 && name != "" && len(out.Hotels) < maxMetaHotels {
			out.Hotels = append(out.Hotels, MetaHotel{ID: h.ID, Name: name})
		}
	}
	if out.Intent == "" && len(out.Hotels) == 0 && !out.Support && !out.Bookings {
		return nil
	}
	return &out
}

// --- titles and previews ----------------------------------------------------

// A request's lead-in, dropped from an automatic title ("Show me hotels in
// Agra" becomes "Hotels in Agra").
var titleLeadIn = regexp.MustCompile(`(?i)^(hi|hello|hey|namaste)?[\s,!.]*(please\s+)?(can|could|would)?\s*(you\s+)?(please\s+)?(show|find|search|get|give|tell|list)?\s*(me\s+)?(for\s+)?(some\s+|the\s+|a\s+|an\s+)?`)

// autoTitle is a short title from the first message of a conversation.
func autoTitle(first string) string {
	t := strings.Join(strings.Fields(first), " ")
	if stripped := strings.TrimSpace(titleLeadIn.ReplaceAllString(t, "")); len([]rune(stripped)) >= 3 {
		t = stripped
	}
	t = strings.TrimRight(t, " ?!.,")
	if r := []rune(t); len(r) > autoTitleRunes {
		cut := string(r[:autoTitleRunes])
		if i := strings.LastIndex(cut, " "); i > autoTitleRunes/2 {
			cut = cut[:i]
		}
		t = strings.TrimRight(cut, " ,.;:-") + "…"
	}
	if t == "" {
		return defaultChatTitle
	}
	return upperFirst(t)
}

// chatPreview is the start of a message, on one line, for the history list.
func chatPreview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxPreviewRunes {
		return string(r[:maxPreviewRunes-1]) + "…"
	}
	return s
}

// --- masking sensitive values -------------------------------------------------

var (
	cardNumberLike  = regexp.MustCompile(`\b\d(?:[ -]?\d){12,18}\b`)
	jwtLike         = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)
	bearerLike      = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`)
	statedPassword  = regexp.MustCompile(`(?i)\b(password|passwd|pwd|passcode)\s*(?:is|:|=)\s*\S+`)
	statedShortCode = regexp.MustCompile(`(?i)\b(otp|upi pin|pin|cvv|cvc)\s*(?:is|:|=)?\s*\d{3,8}\b`)
)

// redactSensitive masks payment card numbers (Luhn-valid 13-19 digits),
// tokens, and passwords or one-time codes the guest typed, so they are never
// stored. Booking ids and phone numbers are not card numbers and are kept.
func redactSensitive(s string) string {
	s = jwtLike.ReplaceAllString(s, "[token removed]")
	s = bearerLike.ReplaceAllString(s, "[token removed]")
	s = statedPassword.ReplaceAllString(s, "$1 [removed]")
	s = statedShortCode.ReplaceAllString(s, "$1 [removed]")
	return cardNumberLike.ReplaceAllStringFunc(s, func(m string) string {
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, m)
		if len(digits) >= 13 && len(digits) <= 19 && luhnValid(digits) {
			return "[card number removed]"
		}
		return m
	})
}

func luhnValid(digits string) bool {
	sum, double := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// --- cursors ----------------------------------------------------------------

func encodeHistoryCursor(c HistoryCursor) string {
	raw := strconv.FormatInt(c.UpdatedAt.UnixMicro(), 10) + "." + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeHistoryCursor(s string) (*HistoryCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	ts, id, ok := strings.Cut(string(raw), ".")
	if !ok || !conversationIDPattern.MatchString(id) {
		return nil, errors.New("bad cursor")
	}
	us, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || us <= 0 {
		return nil, errors.New("bad cursor")
	}
	return &HistoryCursor{UpdatedAt: time.UnixMicro(us).UTC(), ID: id}, nil
}
