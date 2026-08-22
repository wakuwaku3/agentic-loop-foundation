package runner

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"time"
)

type MemoryStore struct {
	mu          sync.Mutex
	enrollments map[string]Enrollment
	challenges  map[string]Challenge
	sessions    map[string]Session
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{enrollments: map[string]Enrollment{}, challenges: map[string]Challenge{}, sessions: map[string]Session{}}
}
func (m *MemoryStore) SaveEnrollment(_ context.Context, v Enrollment) error {
	if v.RunnerID == "" || v.TokenHash == "" {
		return errors.New("invalid enrollment")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enrollments[v.TokenHash] = v
	return nil
}
func (m *MemoryStore) ConsumeEnrollment(_ context.Context, hash string, key []byte, c Challenge) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.enrollments[hash]
	if !ok {
		return ErrExpired
	}
	if len(key) != ed25519.PublicKeySize {
		return ErrInvalidEnrollment
	}
	if e.ExpiresAt.Before(c.IssuedAt) {
		return ErrExpired
	}
	delete(m.enrollments, hash)
	c.RunnerID = e.RunnerID
	c.PublicKey = append([]byte(nil), key...)
	m.challenges[c.ID] = cloneChallenge(c)
	return nil
}
func (m *MemoryStore) Challenge(_ context.Context, id string) (Challenge, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.challenges[id]
	return cloneChallenge(v), ok, nil
}
func (m *MemoryStore) ConsumeChallenge(_ context.Context, id string, sig []byte, now time.Time, session Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.challenges[id]
	if !ok {
		return ErrReplay
	}
	if now.After(c.ExpiresAt) {
		return ErrExpired
	}
	if !ed25519.Verify(ed25519.PublicKey(c.PublicKey), challengeMessage(c), sig) {
		return ErrInvalidEnrollment
	}
	delete(m.challenges, id)
	session.Token = ""
	m.sessions[session.TokenHash] = session
	return nil
}
func (m *MemoryStore) SessionByToken(_ context.Context, hash string) (Session, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.sessions[hash]
	return v, ok, nil
}
func cloneChallenge(v Challenge) Challenge {
	v.PublicKey = append([]byte(nil), v.PublicKey...)
	v.Nonce = append([]byte(nil), v.Nonce...)
	return v
}
