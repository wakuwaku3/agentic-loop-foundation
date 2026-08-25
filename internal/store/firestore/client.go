package firestore

import (
	"context"
	"errors"

	cloudfirestore "cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ClientStore remains an API-compatible name for the production adapter.
type ClientStore = Store

func NewClient(ctx context.Context, projectID, installation string) (*Store, error) {
	if projectID == "" || installation == "" {
		return nil, errors.New("project and installation are required")
	}
	c, err := cloudfirestore.NewClient(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return NewStore(c, installation)
}

// NewEmulatorClient is the preview-local counterpart of NewClient: it wires
// the same cloudfirestore.Client construction (which honours
// FIRESTORE_EMULATOR_HOST automatically) but calls NewEmulatorStore instead
// of NewStore, so the resulting Store does not refuse an emulator host.
// cmd/control-plane only calls this behind an explicit local-only opt-in
// environment variable; it must never be reachable from the production
// startup path.
func NewEmulatorClient(ctx context.Context, projectID, installation string) (*Store, error) {
	if projectID == "" || installation == "" {
		return nil, errors.New("project and installation are required")
	}
	c, err := cloudfirestore.NewClient(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return NewEmulatorStore(c, installation)
}
func (s *Store) Close() error { return s.client.Close() }
func (s *Store) RunTransaction(ctx context.Context, fn func(context.Context, *cloudfirestore.Transaction) error) error {
	return s.client.RunTransaction(ctx, fn)
}
func (s *Store) BootstrapInstallation(ctx context.Context) error {
	path, err := CollectionPath(s.installation, "_meta")
	if err != nil {
		return err
	}
	return s.client.RunTransaction(ctx, func(ctx context.Context, tx *cloudfirestore.Transaction) error {
		ref := s.client.Doc(path + "/installation")
		if _, err := tx.Get(ref); err == nil {
			return nil
		} else if status.Code(err) != codes.NotFound {
			return err
		}
		return tx.Create(ref, map[string]any{"record_schema": RecordSchema, "kind": "installation", "installation_id": s.installation})
	})
}
