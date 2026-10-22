package providerkeys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-walkerx/trequila/internal/ai"
	"github.com/sky-walkerx/trequila/internal/crypto"
	"github.com/sky-walkerx/trequila/internal/netguard"
)

var ErrNoKey = errors.New("providerkeys: no configured key found")

type ProviderKey struct {
	Provider     string `json:"provider"`
	BaseURL      string `json:"base_url,omitempty"`
	DefaultModel string `json:"default_model"`
	IsDefault    bool   `json:"is_default"`
	MaskedKey    string `json:"masked_key"`
}

type Store struct {
	db     *sql.DB
	cipher *crypto.Cipher
}

func NewStore(db *sql.DB, cipher *crypto.Cipher) *Store {
	return &Store{db: db, cipher: cipher}
}

// MaskKey returns a display-safe form of an API key: only the last four
// characters survive. It is provider-agnostic (no assumed "sk-" prefix, since
// Gemini keys start with "AIza", etc.).
func MaskKey(key string) string {
	if len(key) <= 4 {
		return "***"
	}
	return "..." + key[len(key)-4:]
}

func (s *Store) Resolve(ctx context.Context, userID, overrideProvider string) (ai.ProviderConfig, error) {
	query := `
		SELECT provider, encrypted_key, base_url, default_model
		FROM provider_keys
		WHERE user_id = $1 AND ($2 = '' AND is_default = true OR provider = $2)
		LIMIT 1
	`
	var provider, encKey, defaultModel string
	var baseURL *string

	err := s.db.QueryRowContext(ctx, query, userID, overrideProvider).Scan(&provider, &encKey, &baseURL, &defaultModel)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ai.ProviderConfig{}, ErrNoKey
		}
		return ai.ProviderConfig{}, err
	}

	decryptedKey, err := s.cipher.Decrypt(encKey)
	if err != nil {
		return ai.ProviderConfig{}, err
	}

	cfg := ai.ProviderConfig{
		APIKey: decryptedKey,
		Model:  defaultModel,
	}

	switch provider {
	case "gemini":
		cfg.Kind = ai.ProviderGemini
	case "anthropic":
		cfg.Kind = ai.ProviderAnthropic
	case "openai_compatible":
		cfg.Kind = ai.ProviderOpenAICompatible
		if baseURL != nil {
			cfg.BaseURL = *baseURL
		}
	}

	return cfg, nil
}

func (s *Store) List(ctx context.Context, userID string) ([]ProviderKey, error) {
	query := `
		SELECT provider, encrypted_key, base_url, default_model, is_default
		FROM provider_keys
		WHERE user_id = $1
		ORDER BY created_at ASC
	`
	rows, err := s.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []ProviderKey
	for rows.Next() {
		var pk ProviderKey
		var encKey string
		var baseURL *string
		if err := rows.Scan(&pk.Provider, &encKey, &baseURL, &pk.DefaultModel, &pk.IsDefault); err != nil {
			return nil, err
		}
		if baseURL != nil {
			pk.BaseURL = *baseURL
		}

		decryptedKey, err := s.cipher.Decrypt(encKey)
		if err == nil {
			pk.MaskedKey = MaskKey(decryptedKey)
		} else {
			pk.MaskedKey = "***error***"
		}
		keys = append(keys, pk)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

// ErrInvalidBaseURL means a provider base URL was rejected: not https, or pointing at a
// private or internal address. The wrapped message is safe to show to the user.
var ErrInvalidBaseURL = errors.New("invalid base_url")

// Upsert stores (or replaces) a user's key for a provider. A base URL is validated
// first, because the server will make requests to it on the user's behalf.
func (s *Store) Upsert(ctx context.Context, userID, provider, apiKey, baseURL, defaultModel string, makeDefault bool) error {
	if baseURL != "" {
		if _, err := netguard.ValidateBaseURL(baseURL, netguard.AllowPrivateFromEnv()); err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidBaseURL, err.Error())
		}
	}
	encKey, err := s.cipher.Encrypt(apiKey)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// If making default, unset others
	if makeDefault {
		_, err = tx.ExecContext(ctx, "UPDATE provider_keys SET is_default = false WHERE user_id = $1", userID)
		if err != nil {
			return err
		}
	} else {
		// If this is the only key, force it to be default
		var count int
		err = tx.QueryRowContext(ctx, "SELECT count(*) FROM provider_keys WHERE user_id = $1", userID).Scan(&count)
		if err != nil {
			return err
		}
		if count == 0 {
			makeDefault = true
		}
	}

	// On conflict we only ever promote to default (is_default OR), never demote:
	// re-saving a non-default key must not strip the user's existing default.
	// Demotion happens explicitly above, when another key is made default.
	query := `
		INSERT INTO provider_keys (user_id, provider, encrypted_key, base_url, default_model, is_default)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id, provider) DO UPDATE SET
			encrypted_key = EXCLUDED.encrypted_key,
			base_url = EXCLUDED.base_url,
			default_model = EXCLUDED.default_model,
			is_default = provider_keys.is_default OR EXCLUDED.is_default,
			updated_at = CURRENT_TIMESTAMP
	`
	var bURL *string
	if baseURL != "" {
		bURL = &baseURL
	}

	_, err = tx.ExecContext(ctx, query, userID, provider, encKey, bURL, defaultModel, makeDefault)
	if err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) Delete(ctx context.Context, userID, provider string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var wasDefault bool
	err = tx.QueryRowContext(ctx, "DELETE FROM provider_keys WHERE user_id = $1 AND provider = $2 RETURNING is_default", userID, provider).Scan(&wasDefault)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // Already deleted
		}
		return err
	}

	if wasDefault {
		// Make another key default if exists
		_, err = tx.ExecContext(ctx, `
			UPDATE provider_keys 
			SET is_default = true 
			WHERE user_id = $1 
			AND provider = (SELECT provider FROM provider_keys WHERE user_id = $1 LIMIT 1)
		`, userID)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}
