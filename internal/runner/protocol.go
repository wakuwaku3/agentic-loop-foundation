// Package runner contains the provider-neutral Runner enrollment/session protocol.
package runner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

const (
	TokenTTL        = 10 * time.Minute
	SessionTTL      = 15 * time.Minute
	ChallengeTTL    = 2 * time.Minute
	ChallengeMethod = "POST"
	ChallengePath   = "/v1/runner/enrollment/complete"
)

var (
	ErrInvalidEnrollment = errors.New("invalid runner enrollment")
	ErrExpired           = errors.New("runner enrollment/session expired")
	ErrReplay            = errors.New("runner challenge already consumed")
	ErrUnauthenticated   = errors.New("runner session is not authenticated")
)

type Enrollment struct {
	RunnerID  string
	PublicKey []byte
	TokenHash string
	ExpiresAt time.Time
}
type Challenge struct {
	ID        string
	RunnerID  string
	PublicKey []byte
	Nonce     []byte
	Method    string
	Path      string
	IssuedAt  time.Time
	ExpiresAt time.Time
}
type Session struct {
	RunnerID  string
	TokenHash string
	Token     string `json:"token,omitempty"`
	ExpiresAt time.Time
}
type Store interface {
	SaveEnrollment(context.Context, Enrollment) error
	ConsumeEnrollment(context.Context, string, []byte, Challenge) error
	Challenge(context.Context, string) (Challenge, bool, error)
	ConsumeChallenge(context.Context, string, []byte, time.Time, Session) error
	SessionByToken(context.Context, string) (Session, bool, error)
}
type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store) (*Service, error) {
	if store == nil {
		return nil, errors.New("runner enrollment store is required")
	}
	return &Service{store: store, now: func() time.Time { return time.Now().UTC() }}, nil
}
func (s *Service) IssueEnrollment(ctx context.Context, runnerID string, ttl time.Duration) (string, error) {
	if runnerID == "" {
		return "", ErrInvalidEnrollment
	}
	if ttl <= 0 || ttl > TokenTTL {
		ttl = TokenTTL
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	if err := s.store.SaveEnrollment(ctx, Enrollment{RunnerID: runnerID, TokenHash: hashToken(token), ExpiresAt: s.now().Add(ttl)}); err != nil {
		return "", err
	}
	return token, nil
}
func (s *Service) Begin(ctx context.Context, token string, publicKey ed25519.PublicKey) (Challenge, error) {
	if token == "" || len(publicKey) != ed25519.PublicKeySize {
		return Challenge{}, ErrInvalidEnrollment
	}
	now := s.now()
	id, err := randomToken()
	if err != nil {
		return Challenge{}, err
	}
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		return Challenge{}, err
	}
	c := Challenge{ID: id, PublicKey: append([]byte(nil), publicKey...), Nonce: nonce, Method: ChallengeMethod, Path: ChallengePath, IssuedAt: now, ExpiresAt: now.Add(ChallengeTTL)}
	if err := s.store.ConsumeEnrollment(ctx, hashToken(token), publicKey, c); err != nil {
		return Challenge{}, err
	}
	stored, ok, err := s.store.Challenge(ctx, c.ID)
	if err != nil || !ok {
		return Challenge{}, ErrInvalidEnrollment
	}
	return stored, nil
}
func (s *Service) Complete(ctx context.Context, challengeID, signature string) (Session, error) {
	if challengeID == "" || signature == "" {
		return Session{}, ErrInvalidEnrollment
	}
	c, ok, err := s.store.Challenge(ctx, challengeID)
	if err != nil {
		return Session{}, err
	}
	if !ok {
		return Session{}, ErrReplay
	}
	if s.now().After(c.ExpiresAt) {
		return Session{}, ErrExpired
	}
	sig, err := decodeSignature(signature)
	if err != nil {
		return Session{}, ErrInvalidEnrollment
	}
	token, err := randomToken()
	if err != nil {
		return Session{}, err
	}
	session := Session{RunnerID: c.RunnerID, TokenHash: hashToken(token), ExpiresAt: s.now().Add(SessionTTL), Token: token}
	if err := s.store.ConsumeChallenge(ctx, challengeID, sig, s.now(), session); err != nil {
		return Session{}, err
	}
	return session, nil
}
func (s *Service) VerifySession(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", ErrUnauthenticated
	}
	v, ok, err := s.store.SessionByToken(ctx, hashToken(token))
	if err != nil {
		return "", err
	}
	if !ok || s.now().After(v.ExpiresAt) {
		return "", ErrExpired
	}
	return v.RunnerID, nil
}
func challengeMessage(c Challenge) []byte {
	return []byte(fmt.Sprintf("agentic-loop/runner-challenge/v2\nmethod=%s\npath=%s\nchallenge_id=%s\nrunner_id=%s\nissued_at=%s\nexpires_at=%s\nnonce=%s\n", c.Method, c.Path, c.ID, c.RunnerID, c.IssuedAt.UTC().Format(time.RFC3339Nano), c.ExpiresAt.UTC().Format(time.RFC3339Nano), base64.RawURLEncoding.EncodeToString(c.Nonce)))
}
func ChallengeMessage(c Challenge) []byte { return challengeMessage(c) }
func decodeSignature(value string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err == nil {
		return b, nil
	}
	return hex.DecodeString(value)
}
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
