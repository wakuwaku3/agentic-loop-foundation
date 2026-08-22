package runner

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

func TestEnrollmentChallengeSignatureAndReplay(t *testing.T) {
	pub, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	s, err := NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.IssueEnrollment(context.Background(), "runner-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Begin(context.Background(), token, pub)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(private, ChallengeMessage(c))
	session, err := s.Complete(context.Background(), c.ID, base64.RawURLEncoding.EncodeToString(sig))
	if err != nil {
		t.Fatal(err)
	}
	if session.Token == "" || session.TokenHash == session.Token {
		t.Fatal("session returned a secret hash or no token")
	}
	stored, ok, err := store.SessionByToken(context.Background(), session.TokenHash)
	if err != nil || !ok || stored.Token != "" {
		t.Fatalf("session secret was persisted: ok=%v token_present=%v err=%v", ok, stored.Token != "", err)
	}
	if got, err := s.VerifySession(context.Background(), session.Token); err != nil || got != "runner-1" {
		t.Fatalf("verify: %s %v", got, err)
	}
	if _, err := s.Complete(context.Background(), c.ID, base64.RawURLEncoding.EncodeToString(sig)); !errors.Is(err, ErrReplay) {
		t.Fatalf("expected replay rejection, got %v", err)
	}
}

func TestEnrollmentRejectsTamperedChallenge(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := NewService(NewMemoryStore())
	token, _ := s.IssueEnrollment(context.Background(), "runner-2", time.Minute)
	c, _ := s.Begin(context.Background(), token, pub)
	other := append([]byte(nil), c.Nonce...)
	other[0] ^= 1
	c.Nonce = other
	bad := ed25519.Sign(ed25519.PrivateKey(make([]byte, ed25519.PrivateKeySize)), ChallengeMessage(c))
	if _, err := s.Complete(context.Background(), c.ID, base64.RawURLEncoding.EncodeToString(bad)); err == nil {
		t.Fatal("tampered challenge accepted")
	}
}

func TestEnrollmentTokenIsConsumedOnFirstBegin(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	s, _ := NewService(NewMemoryStore())
	token, _ := s.IssueEnrollment(context.Background(), "runner-once", time.Minute)
	if _, err := s.Begin(context.Background(), token, pub); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(context.Background(), token, pub); !errors.Is(err, ErrExpired) {
		t.Fatalf("second begin was accepted: %v", err)
	}
}
