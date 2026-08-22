package application

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

var ErrIdempotencyConflict = errors.New("idempotency key was reused with a different request")

func requestFingerprint(operation string, request any) (string, error) {
	b, err := json.Marshal(struct {
		Operation string `json:"operation"`
		Request   any    `json:"request"`
	}{operation, request})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func responseJSON(value any) ([]byte, error) { return json.Marshal(value) }

func restoreResponse(record IdempotentResponse, out any) error {
	if len(record.ResponseJSON) == 0 {
		return ErrDuplicateRequest
	}
	return json.Unmarshal(record.ResponseJSON, out)
}
