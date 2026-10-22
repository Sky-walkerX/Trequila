package chatbot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/sky-walkerx/trequila/internal/ai"
)

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

type Conversation struct {
	ID        string
	UserID    string
	TripID    string
	Title     string
	Messages  []ai.ChatTurn
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (s *Store) GetConversation(ctx context.Context, id, userID string) (*Conversation, error) {
	var c Conversation
	var msgsJSON []byte
	var tripID sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, trip_id, title, messages, created_at, updated_at
		FROM conversations WHERE id = $1 AND user_id = $2`, id, userID).
		Scan(&c.ID, &c.UserID, &tripID, &c.Title, &msgsJSON, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // not found
		}
		return nil, err
	}

	if tripID.Valid {
		c.TripID = tripID.String
	}

	if len(msgsJSON) > 0 {
		if err := json.Unmarshal(msgsJSON, &c.Messages); err != nil {
			return nil, err
		}
	}

	return &c, nil
}

func (s *Store) SaveConversation(ctx context.Context, c *Conversation) error {
	if c.ID == "" {
		c.ID = uuid.NewString()
		c.CreatedAt = time.Now()
	}
	c.UpdatedAt = time.Now()

	msgsJSON, _ := json.Marshal(c.Messages)

	var tripID interface{}
	if c.TripID != "" {
		tripID = c.TripID
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO conversations (id, user_id, trip_id, title, messages, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET
			title = EXCLUDED.title,
			messages = EXCLUDED.messages,
			trip_id = EXCLUDED.trip_id,
			updated_at = EXCLUDED.updated_at
	`, c.ID, c.UserID, tripID, c.Title, msgsJSON, c.CreatedAt, c.UpdatedAt)

	return err
}
