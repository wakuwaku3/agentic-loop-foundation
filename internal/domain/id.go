// Package domain contains side-effect-free business types and invariants.
package domain

import (
	"errors"
	"fmt"
	"strings"
)

var ErrEmptyID = errors.New("domain id must not be empty")

// ID is an opaque, stable identifier. Its value is never interpreted by the
// domain, keeping storage and transport formats replaceable.
type ID string

func NewID(value string) (ID, error) {
	if strings.TrimSpace(value) == "" {
		return "", ErrEmptyID
	}
	return ID(value), nil
}

func (id ID) String() string { return string(id) }

// FencingToken monotonically orders leases. A stale worker must never be
// allowed to commit after a newer token has been issued.
type FencingToken uint64

func (t FencingToken) Next() (FencingToken, error) {
	if t == ^FencingToken(0) {
		return 0, fmt.Errorf("fencing token overflow")
	}
	return t + 1, nil
}

// Allows accepts only the currently issued token; both stale and unknown
// future tokens are rejected until the canonical state issues them.
func (t FencingToken) Allows(candidate FencingToken) bool { return candidate == t }
