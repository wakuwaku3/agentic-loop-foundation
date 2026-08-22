package firestore

import (
	"context"
	"crypto/ed25519"
	"time"

	cloudfirestore "cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/takushi/agentic-loop-foundation/v2/internal/quota"
	"github.com/takushi/agentic-loop-foundation/v2/internal/runner"
)

// Runner records are installation-scoped and all consume operations are one
// Firestore transaction; no full collection scan is used for authentication.
func (s *Store) runnerRef(collection, id string) (*cloudfirestore.DocumentRef, error) {
	return s.path(collection, id)
}
func (s *Store) SaveEnrollment(ctx context.Context, v runner.Enrollment) error {
	if err := s.reserveRunnerQuota(ctx); err != nil {
		return err
	}
	ref, err := s.runnerRef("runner-enrollments", v.TokenHash)
	if err != nil {
		return err
	}
	d, err := encodeDocument("runner-enrollment", v)
	if err != nil {
		return err
	}
	_, err = ref.Create(ctx, d)
	return err
}
func (s *Store) ConsumeEnrollment(ctx context.Context, hash string, key []byte, c runner.Challenge) error {
	if err := s.reserveRunnerQuota(ctx); err != nil {
		return err
	}
	ref, err := s.runnerRef("runner-enrollments", hash)
	if err != nil {
		return err
	}
	ch, err := s.runnerRef("runner-challenges", c.ID)
	if err != nil {
		return err
	}
	return s.client.RunTransaction(ctx, func(ctx context.Context, tx *cloudfirestore.Transaction) error {
		snap, err := tx.Get(ref)
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return runner.ErrExpired
			}
			return err
		}
		var e runner.Enrollment
		if err = decodeDocument(snap, "runner-enrollment", &e); err != nil {
			return err
		}
		if e.ExpiresAt.Before(c.IssuedAt) {
			return runner.ErrExpired
		}
		c.RunnerID = e.RunnerID
		c.PublicKey = append([]byte(nil), key...)
		d, err := encodeDocument("runner-challenge", c)
		if err != nil {
			return err
		}
		if err = tx.Delete(ref); err != nil {
			return err
		}
		return tx.Create(ch, d)
	})
}
func (s *Store) Challenge(ctx context.Context, id string) (runner.Challenge, bool, error) {
	ref, err := s.runnerRef("runner-challenges", id)
	if err != nil {
		return runner.Challenge{}, false, err
	}
	snap, err := ref.Get(ctx)
	if status.Code(err) == codes.NotFound {
		return runner.Challenge{}, false, nil
	}
	if err != nil {
		return runner.Challenge{}, false, err
	}
	var c runner.Challenge
	if err = decodeDocument(snap, "runner-challenge", &c); err != nil {
		return runner.Challenge{}, false, err
	}
	return c, true, nil
}
func (s *Store) ConsumeChallenge(ctx context.Context, id string, sig []byte, now time.Time, session runner.Session) error {
	if err := s.reserveRunnerQuota(ctx); err != nil {
		return err
	}
	ch, err := s.runnerRef("runner-challenges", id)
	if err != nil {
		return err
	}
	sr, err := s.runnerRef("runner-sessions", session.TokenHash)
	if err != nil {
		return err
	}
	return s.client.RunTransaction(ctx, func(ctx context.Context, tx *cloudfirestore.Transaction) error {
		snap, err := tx.Get(ch)
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return runner.ErrReplay
			}
			return err
		}
		var c runner.Challenge
		if err = decodeDocument(snap, "runner-challenge", &c); err != nil {
			return err
		}
		if now.After(c.ExpiresAt) {
			return runner.ErrExpired
		}
		if !ed25519.Verify(ed25519.PublicKey(c.PublicKey), runner.ChallengeMessage(c), sig) {
			return runner.ErrInvalidEnrollment
		}
		session.Token = ""
		d, err := encodeDocument("runner-session", session)
		if err != nil {
			return err
		}
		if err = tx.Delete(ch); err != nil {
			return err
		}
		return tx.Create(sr, d)
	})
}
func (s *Store) SessionByToken(ctx context.Context, hash string) (runner.Session, bool, error) {
	ref, err := s.runnerRef("runner-sessions", hash)
	if err != nil {
		return runner.Session{}, false, err
	}
	snap, err := ref.Get(ctx)
	if status.Code(err) == codes.NotFound {
		return runner.Session{}, false, nil
	}
	if err != nil {
		return runner.Session{}, false, err
	}
	var v runner.Session
	if err = decodeDocument(snap, "runner-session", &v); err != nil {
		return runner.Session{}, false, err
	}
	return v, true, nil
}

var _ runner.Store = (*Store)(nil)

func (s *Store) reserveRunnerQuota(ctx context.Context) error {
	at := time.Now().UTC()
	ref, err := s.path("quota", quota.Day(at))
	if err != nil {
		return err
	}
	return s.client.RunTransaction(ctx, func(ctx context.Context, tx *cloudfirestore.Transaction) error {
		snap, err := tx.Get(ref)
		var record quotaRecord
		if err == nil {
			if err := decodeDocument(snap, "quota", &record); err != nil {
				return err
			}
		} else if status.Code(err) != codes.NotFound {
			return err
		}
		counter := quota.Counter{Day: record.Day, Total: record.Total, Shards: record.Shards}
		if err := counter.Reserve(at, "runner", quota.MutationUsage, quota.DefaultBudget); err != nil {
			return err
		}
		record = quotaRecord{Day: counter.Day, Total: counter.Total, Shards: counter.Shards}
		d, err := encodeDocument("quota", record)
		if err != nil {
			return err
		}
		return tx.Set(ref, d)
	})
}
