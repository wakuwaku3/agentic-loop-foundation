package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/api"
	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/reconciler"
	"github.com/takushi/agentic-loop-foundation/v2/internal/runner"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/firestore"
)

type productionClock struct{}

func (productionClock) Now() time.Time { return time.Now().UTC() }

type productionIDs struct{ n atomic.Uint64 }

func (g *productionIDs) Next(kind string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return kind + "-" + hex.EncodeToString(b[:]) + fmt.Sprintf("-%d", g.n.Add(1)), nil
}

func requiredEnv(name string) (string, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return "", fmt.Errorf("required environment variable %s is missing", name)
	}
	return v, nil
}
func ownerAllowlist(value string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for _, raw := range strings.Split(value, ",") {
		email := strings.ToLower(strings.TrimSpace(raw))
		if email == "" || strings.Count(email, "@") != 1 || strings.ContainsAny(email, " \t\r\n") {
			return nil, errors.New("OWNER_EMAILS contains an invalid email")
		}
		out[email] = struct{}{}
	}
	if len(out) == 0 {
		return nil, errors.New("OWNER_EMAILS must not be empty")
	}
	return out, nil
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println("0.1.0-dev")
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "control-plane:", err)
		os.Exit(1)
	}
}

func run() error {
	installation, err := requiredEnv("INSTALLATION_ID")
	if err != nil {
		return err
	}
	project, err := requiredEnv("GCP_PROJECT_ID")
	if err != nil {
		return err
	}
	emails, err := requiredEnv("OWNER_EMAILS")
	if err != nil {
		return err
	}
	owners, err := ownerAllowlist(emails)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := firestore.NewClient(ctx, project, installation)
	if err != nil {
		return err
	}
	defer store.Close()
	runnerEnrollment, err := runner.NewService(store)
	if err != nil {
		return err
	}
	originsValue, err := requiredEnv("OWNER_ORIGINS")
	if err != nil {
		return err
	}
	origins := splitNonEmpty(originsValue)
	if len(origins) == 0 {
		return errors.New("OWNER_ORIGINS must not be empty")
	}
	reconcileIdentity, err := requiredEnv("RECONCILE_IDENTITY")
	if err != nil {
		return err
	}
	service, err := application.NewServiceWithConfig(store, productionClock{}, &productionIDs{}, application.ServiceConfig{InstallationID: installation, LeaseTTL: time.Minute})
	if err != nil {
		return err
	}
	clock := productionClock{}
	leaseReconciler := &reconciler.Reconciler{Tx: store, Clock: clock}
	verificationReconciler := &reconciler.VerificationReconciler{Tx: store, Clock: clock, Deadline: time.Minute}
	h := api.New(api.Config{Authenticator: api.CombinedAuthenticator{Runner: runnerEnrollment, OwnerEmails: owners}, Service: service, RunnerEnrollment: runnerEnrollment, AllowedOrigins: origins, ReconcileIdentity: strings.ToLower(reconcileIdentity), InternalReconcile: func(callCtx context.Context) error {
		tickCtx, cancel := context.WithTimeout(callCtx, 5*time.Second)
		defer cancel()
		if _, _, err := leaseReconciler.Tick(tickCtx, ""); err != nil {
			return err
		}
		_, err := verificationReconciler.Tick(tickCtx)
		return err
	}})
	addr := ":8080"
	if value := strings.TrimSpace(os.Getenv("PORT")); value != "" {
		addr = ":" + value
	}
	server := &http.Server{Addr: addr, Handler: h}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func splitNonEmpty(value string) []string {
	var out []string
	for _, raw := range strings.Split(value, ",") {
		if item := strings.TrimSpace(raw); item != "" {
			out = append(out, item)
		}
	}
	return out
}
