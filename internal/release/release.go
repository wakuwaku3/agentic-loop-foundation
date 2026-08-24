// Package release contains the local, provider-neutral M5 release core.
// Persistence is deliberately behind a small port; no application UnitOfWork
// records or mutable domain candidate is exposed by the store.
package release

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

var ErrImmutableConflict = errors.New("release candidate is immutable")

type Bundle struct {
	Repository string
	Candidate  domain.ReleaseCandidate
	CreatedAt  time.Time
}

func NewBundle(repository string, candidate domain.ReleaseCandidate, at time.Time) (Bundle, error) {
	if repository == "" || at.IsZero() {
		return Bundle{}, errors.New("repository and creation time are required")
	}
	if err := domain.ValidateRelease(candidate); err != nil {
		return Bundle{}, err
	}
	return Bundle{Repository: repository, Candidate: candidate.Clone(), CreatedAt: at.UTC()}, nil
}
func (b Bundle) Snapshot() Bundle { b.Candidate = b.Candidate.Clone(); return b }

type Store interface {
	Put(Bundle) error
	Get(repository, candidateID string) (Bundle, bool, error)
}

type MemoryStore struct {
	mu      sync.RWMutex
	bundles map[string]Bundle
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{bundles: map[string]Bundle{}} }
func (s *MemoryStore) Put(bundle Bundle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := bundle.Repository + "\x00" + bundle.Candidate.ID.String()
	if old, ok := s.bundles[key]; ok {
		if !reflect.DeepEqual(old.Snapshot(), bundle.Snapshot()) {
			return ErrImmutableConflict
		}
		return nil
	}
	s.bundles[key] = bundle.Snapshot()
	return nil
}
func (s *MemoryStore) Get(repository, candidateID string) (Bundle, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.bundles[repository+"\x00"+candidateID]
	return b.Snapshot(), ok, nil
}

type Route struct {
	Repository, PreviewDigest, StableDigest, PreviousStableDigest string
	Generation                                                    uint64
}
type Router struct {
	mu     sync.RWMutex
	routes map[string]Route
}

func NewRouter() *Router { return &Router{routes: map[string]Route{}} }
func (r *Router) SetPreview(repository, digest string) error {
	if repository == "" || digest == "" {
		return errors.New("preview route requires repository and digest")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	route := r.routes[repository]
	route.Repository, route.PreviewDigest, route.Generation = repository, digest, route.Generation+1
	r.routes[repository] = route
	return nil
}
func (r *Router) Promote(repository, digest string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	route := r.routes[repository]
	if route.PreviewDigest != digest {
		return errors.New("only the preview route can be promoted")
	}
	route.PreviousStableDigest, route.StableDigest, route.Generation = route.StableDigest, digest, route.Generation+1
	r.routes[repository] = route
	return nil
}
func (r *Router) Rollback(repository string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	route := r.routes[repository]
	if route.PreviousStableDigest == "" {
		return errors.New("no previous stable route")
	}
	route.StableDigest, route.PreviousStableDigest, route.Generation = route.PreviousStableDigest, route.StableDigest, route.Generation+1
	r.routes[repository] = route
	return nil
}
func (r *Router) Get(repository string) (Route, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	route, ok := r.routes[repository]
	return route, ok
}

type CompiledContract struct {
	Digest       string
	Version      string
	Capabilities []string
	DocsDigest   string
}

func CompileContract(data []byte, docs []byte) (CompiledContract, error) {
	var raw struct {
		SchemaVersion string `json:"schema_version"`
		ID            string `json:"id"`
		Kind          string `json:"kind"`
		CreatedAt     string `json:"created_at"`
		CorrelationID string `json:"correlation_id"`
		Release       string `json:"release"`
		Capabilities  []struct {
			ID          string   `json:"id"`
			Name        string   `json:"name"`
			Status      string   `json:"status"`
			EvidenceIDs []string `json:"evidence_ids"`
		} `json:"capabilities"`
		Verification []string `json:"verification"`
		Rollback     struct {
			Procedure string `json:"procedure"`
			Target    string `json:"target"`
		} `json:"rollback"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil || raw.SchemaVersion == "" || raw.Release == "" || len(raw.Capabilities) == 0 || len(docs) == 0 {
		return CompiledContract{}, errors.New("release contract is incomplete")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return CompiledContract{}, errors.New("release contract must contain exactly one JSON value")
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(raw.Capabilities))
	for _, c := range raw.Capabilities {
		if c.ID == "" || seen[c.ID] {
			return CompiledContract{}, errors.New("release contract capability ids must be unique and non-empty")
		}
		seen[c.ID] = true
		ids = append(ids, c.ID)
		if c.Status == "stable" && len(c.EvidenceIDs) == 0 {
			return CompiledContract{}, errors.New("release contract capability claims stable status without evidence")
		}
	}
	h := sha256.Sum256(data)
	d := sha256.Sum256(docs)
	return CompiledContract{Digest: hex.EncodeToString(h[:]), Version: raw.Release, Capabilities: ids, DocsDigest: hex.EncodeToString(d[:])}, nil
}

type GateResult struct {
	Candidate domain.ReleaseCandidate
	Proof     domain.StableReleaseProof
}

func PromotionGate(bundle Bundle, control domain.EffectiveControlResult, permit domain.PermitDecision) (GateResult, error) {
	candidate := bundle.Candidate.Clone()
	if err := candidate.CanPromote(); err != nil {
		return GateResult{}, err
	}
	if !permit.Allowed() || permit.Kind() != domain.PermitPromotion || !control.Found || control.Mode != domain.ControlAllow || permit.Revision() != candidate.ExpectedControlRevision || permit.FencingToken() != candidate.FencingToken {
		return GateResult{}, domain.ErrControlDenied
	}
	// Use the domain's proof-producing transition; no routing side effect occurs here.
	promoting, _, err := domain.PromoteReleaseWithPermit(candidate, control, permit)
	if err != nil {
		return GateResult{}, err
	}
	stable, proof, err := domain.CompletePromotionWithProof(promoting, control, permit)
	if err != nil {
		return GateResult{}, fmt.Errorf("promotion completion: %w", err)
	}
	return GateResult{Candidate: stable, Proof: proof}, nil
}
