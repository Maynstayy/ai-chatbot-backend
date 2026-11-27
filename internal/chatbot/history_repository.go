//go:build ignore

// Server-side chat history (from MaynStay feature/ai-chatbot), kept for a
// later version and excluded from the default build: V1 keeps history in
// the browser. Its owner is still the MaynStay signed-in user
// (middleware.GetSignupIDFromCtx); enabling it needs a guest identity first.

package chatbot

// MySQL storage for chat history (tables from migration 0047). Every query
// that takes an owner filters on signup_id, so one guest's rows are never
// read or changed through another guest's id.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

type historyRepository struct {
	db *sql.DB
}

// NewHistoryRepository returns the MySQL chat history store.
func NewHistoryRepository(db *sql.DB) HistoryRepository {
	return &historyRepository{db: db}
}

const summaryColumns = `conversation_id, title, last_preview, message_count, created_at, updated_at`

func scanSummary(row interface{ Scan(...any) error }) (*ConversationSummary, error) {
	var c ConversationSummary
	if err := row.Scan(&c.ID, &c.Title, &c.Preview, &c.MessageCount, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	return &c, nil
}

func (r *historyRepository) Create(ctx context.Context, id string, owner int64) (*ConversationSummary, error) {
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO chat_conversations (conversation_id, signup_id, title) VALUES (?, ?, ?)`,
		id, owner, defaultChatTitle); err != nil {
		return nil, err
	}
	return r.Summary(ctx, id, owner)
}

func (r *historyRepository) Summary(ctx context.Context, id string, owner int64) (*ConversationSummary, error) {
	c, err := scanSummary(r.db.QueryRowContext(ctx,
		`SELECT `+summaryColumns+` FROM chat_conversations WHERE conversation_id = ? AND signup_id = ?`, id, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrHistoryNotFound
	}
	return c, err
}

// likePattern escapes s for LIKE with '!' as the escape character, so it
// works whatever the server's backslash setting.
func likePattern(s string) string {
	s = strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(s)
	return "%" + s + "%"
}

func (r *historyRepository) List(ctx context.Context, owner int64, search string, after *HistoryCursor, limit int) ([]ConversationSummary, error) {
	q := `SELECT ` + summaryColumns + ` FROM chat_conversations c WHERE c.signup_id = ? AND c.message_count > 0`
	args := []any{owner}
	if after != nil {
		q += ` AND (c.updated_at < ? OR (c.updated_at = ? AND c.conversation_id < ?))`
		args = append(args, after.UpdatedAt, after.UpdatedAt, after.ID)
	}
	if search != "" {
		p := likePattern(search)
		q += ` AND (c.title LIKE ? ESCAPE '!' OR EXISTS (SELECT 1 FROM chat_messages m WHERE m.conversation_id = c.conversation_id AND m.content LIKE ? ESCAPE '!'))`
		args = append(args, p, p)
	}
	q += ` ORDER BY c.updated_at DESC, c.conversation_id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConversationSummary
	for rows.Next() {
		c, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (r *historyRepository) Messages(ctx context.Context, id string, owner int64, beforeID int64, limit int) ([]StoredMessage, error) {
	q := `SELECT m.message_id, m.client_message_id, m.role, m.content, m.metadata, m.created_at
		FROM chat_messages m JOIN chat_conversations c ON c.conversation_id = m.conversation_id
		WHERE m.conversation_id = ? AND c.signup_id = ?`
	args := []any{id, owner}
	if beforeID > 0 {
		q += ` AND m.message_id < ?`
		args = append(args, beforeID)
	}
	q += ` ORDER BY m.message_id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredMessage
	for rows.Next() {
		var m StoredMessage
		var meta []byte
		if err := rows.Scan(&m.ID, &m.ClientID, &m.Role, &m.Content, &meta, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.CreatedAt = m.CreatedAt.UTC()
		if len(meta) > 0 {
			var md MessageMeta
			if json.Unmarshal(meta, &md) == nil {
				m.Metadata = cleanMeta(&md)
			}
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Newest first from the query; the caller gets them oldest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (r *historyRepository) Append(ctx context.Context, id string, owner int64, msgs []NewMessage, title string) (saved int, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Create the conversation on its first save; an id that already belongs
	// to someone else is left alone and then not found for this owner.
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO chat_conversations (conversation_id, signup_id, title) VALUES (?, ?, ?)
		 ON DUPLICATE KEY UPDATE conversation_id = conversation_id`, id, owner, defaultChatTitle); err != nil {
		return 0, err
	}
	var count int
	var custom bool
	err = tx.QueryRowContext(ctx,
		`SELECT message_count, title_custom FROM chat_conversations WHERE conversation_id = ? AND signup_id = ? FOR UPDATE`,
		id, owner).Scan(&count, &custom)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrHistoryNotFound
		return 0, err
	}
	if err != nil {
		return 0, err
	}

	lastContent := ""
	for _, m := range msgs {
		var meta any
		if m.Metadata != nil {
			b, mErr := json.Marshal(m.Metadata)
			if mErr != nil {
				err = mErr
				return 0, err
			}
			meta = string(b)
		}
		var res sql.Result
		// A client id already saved for this conversation changes nothing
		// (0 rows affected), so a retried save never duplicates a message.
		res, err = tx.ExecContext(ctx,
			`INSERT INTO chat_messages (conversation_id, client_message_id, role, content, metadata)
			 VALUES (?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE message_id = message_id`,
			id, m.ClientID, m.Role, m.Content, meta)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			saved++
			lastContent = m.Content
		}
	}
	if saved > 0 {
		setTitle := count == 0 && !custom
		if _, err = tx.ExecContext(ctx,
			`UPDATE chat_conversations
			 SET message_count = message_count + ?, last_preview = ?, updated_at = CURRENT_TIMESTAMP(3),
			     title = CASE WHEN ? THEN ? ELSE title END
			 WHERE conversation_id = ? AND signup_id = ?`,
			saved, chatPreview(lastContent), setTitle, title, id, owner); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return saved, nil
}

func (r *historyRepository) Rename(ctx context.Context, id string, owner int64, title string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE chat_conversations SET title = ?, title_custom = 1 WHERE conversation_id = ? AND signup_id = ?`,
		title, id, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Unchanged rows also report 0: tell "same title" from "not yours".
		_, err := r.Summary(ctx, id, owner)
		return err
	}
	return nil
}

func (r *historyRepository) Delete(ctx context.Context, id string, owner int64) error {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM chat_conversations WHERE conversation_id = ? AND signup_id = ?`, id, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrHistoryNotFound
	}
	return nil
}

func (r *historyRepository) LoadContext(ctx context.Context, id string) (int64, []Turn, bool, error) {
	var owner int64
	var raw []byte
	err := r.db.QueryRowContext(ctx,
		`SELECT signup_id, ai_context FROM chat_conversations WHERE conversation_id = ?`, id).Scan(&owner, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, false, nil
	}
	if err != nil {
		return 0, nil, false, err
	}
	var turns []Turn
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &turns)
	}
	return owner, trimTurns(turns), true, nil
}

func (r *historyRepository) SaveContext(ctx context.Context, id string, owner int64, turns []Turn) error {
	b, err := json.Marshal(turns)
	if err != nil {
		return err
	}
	// Only the owner's own row is updated; an id owned by anyone else keeps
	// its context unchanged.
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO chat_conversations (conversation_id, signup_id, title, ai_context) VALUES (?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE ai_context = IF(signup_id = VALUES(signup_id), VALUES(ai_context), ai_context)`,
		id, owner, defaultChatTitle, string(b))
	return err
}
