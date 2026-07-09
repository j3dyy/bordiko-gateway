package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUserNotFound is returned by UserStore.Get when no such user exists.
var ErrUserNotFound = errors.New("user not found")

// User is a Bordiko account. The ID is stable and provider-qualified
// (e.g. "google:1234", "github:5678", "dev:alice") and is what appears as a
// match player slot and on the leaderboard.
type User struct {
	ID          string    `json:"id"`
	Provider    string    `json:"provider"`
	ProviderID  string    `json:"-"`
	DisplayName string    `json:"displayName"`
	AvatarURL   string    `json:"avatarUrl"`
	Email       string    `json:"-"`
	CreatedAt   time.Time `json:"createdAt"`
}

// UserStore persists accounts. A Postgres implementation is used when
// DATABASE_URL is set; otherwise an in-memory store keeps accounts for the
// lifetime of the process (fine for dev / dev-login).
type UserStore interface {
	Upsert(ctx context.Context, u *User) error
	Get(ctx context.Context, id string) (*User, error)
	GetMany(ctx context.Context, ids []string) (map[string]*User, error)
	Close() error
}

/* ------------------------------ in-memory --------------------------------- */

type MemoryUserStore struct {
	mu    sync.RWMutex
	users map[string]*User
}

func NewMemoryUserStore() *MemoryUserStore {
	return &MemoryUserStore{users: map[string]*User{}}
}

func (s *MemoryUserStore) Upsert(_ context.Context, u *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.users[u.ID]; ok {
		// Preserve original creation time; refresh mutable profile fields.
		u.CreatedAt = existing.CreatedAt
	} else if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	cp := *u
	s.users[u.ID] = &cp
	return nil
}

func (s *MemoryUserStore) Get(_ context.Context, id string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[id]
	if !ok {
		return nil, ErrUserNotFound
	}
	cp := *u
	return &cp, nil
}

func (s *MemoryUserStore) GetMany(_ context.Context, ids []string) (map[string]*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*User, len(ids))
	for _, id := range ids {
		if u, ok := s.users[id]; ok {
			cp := *u
			out[id] = &cp
		}
	}
	return out, nil
}

func (s *MemoryUserStore) Close() error { return nil }

/* ------------------------------- postgres --------------------------------- */

type PostgresUserStore struct {
	ctx  context.Context
	pool *pgxpool.Pool
}

const usersSchemaSQL = `
CREATE TABLE IF NOT EXISTS users (
    id           text PRIMARY KEY,
    provider     text NOT NULL,
    provider_id  text NOT NULL,
    display_name text NOT NULL,
    avatar_url   text NOT NULL DEFAULT '',
    email        text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);`

func NewPostgresUserStore(ctx context.Context, url string) (*PostgresUserStore, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if _, err := pool.Exec(ctx, usersSchemaSQL); err != nil {
		pool.Close()
		return nil, err
	}
	return &PostgresUserStore{ctx: ctx, pool: pool}, nil
}

func (s *PostgresUserStore) Upsert(ctx context.Context, u *User) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO users (id, provider, provider_id, display_name, avatar_url, email)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (id) DO UPDATE
		   SET display_name = EXCLUDED.display_name,
		       avatar_url   = EXCLUDED.avatar_url,
		       email        = EXCLUDED.email`,
		u.ID, u.Provider, u.ProviderID, u.DisplayName, u.AvatarURL, u.Email)
	return err
}

func (s *PostgresUserStore) Get(ctx context.Context, id string) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, provider, provider_id, display_name, avatar_url, email, created_at
		 FROM users WHERE id = $1`, id).
		Scan(&u.ID, &u.Provider, &u.ProviderID, &u.DisplayName, &u.AvatarURL, &u.Email, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *PostgresUserStore) GetMany(ctx context.Context, ids []string) (map[string]*User, error) {
	out := make(map[string]*User, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, provider, provider_id, display_name, avatar_url, email, created_at
		 FROM users WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Provider, &u.ProviderID, &u.DisplayName, &u.AvatarURL, &u.Email, &u.CreatedAt); err != nil {
			continue
		}
		cp := u
		out[u.ID] = &cp
	}
	return out, nil
}

func (s *PostgresUserStore) Close() error {
	s.pool.Close()
	return nil
}
