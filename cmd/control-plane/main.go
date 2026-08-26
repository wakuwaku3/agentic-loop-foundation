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

// parseOwnerBearerTokens is the preview-local owner session/token boundary
// (roadmap M2). Each entry is "token=email"; every email must already be in
// the OWNER_EMAILS allowlist, so local mode swaps only the transport
// assertion (bearer token instead of an IAP header), never the governed
// identity set. This must never be reachable in a real deployment; see the
// APP_ENV=production guard in run().
func parseOwnerBearerTokens(value string, owners map[string]struct{}) (map[string]string, error) {
	out := map[string]string{}
	for _, raw := range strings.Split(value, ",") {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			return nil, errors.New("AGENTIC_LOOP_LOCAL_OWNER_TOKENS entries must be token=email")
		}
		token := strings.TrimSpace(parts[0])
		email := strings.ToLower(strings.TrimSpace(parts[1]))
		if token == "" || email == "" {
			return nil, errors.New("AGENTIC_LOOP_LOCAL_OWNER_TOKENS entries must not be empty")
		}
		if _, ok := owners[email]; !ok {
			return nil, fmt.Errorf("AGENTIC_LOOP_LOCAL_OWNER_TOKENS email %s is not in OWNER_EMAILS", email)
		}
		if _, dup := out[token]; dup {
			return nil, errors.New("AGENTIC_LOOP_LOCAL_OWNER_TOKENS has a duplicate token")
		}
		out[token] = email
	}
	if len(out) == 0 {
		return nil, errors.New("AGENTIC_LOOP_LOCAL_OWNER_TOKENS must not be empty")
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
	appEnv := strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV")))
	localOwnerTokensValue := strings.TrimSpace(os.Getenv("AGENTIC_LOOP_LOCAL_OWNER_TOKENS"))
	allowEmulator := strings.TrimSpace(os.Getenv("AGENTIC_LOOP_ALLOW_FIRESTORE_EMULATOR")) == "1"
	if appEnv == "production" && (localOwnerTokensValue != "" || allowEmulator) {
		return errors.New("AGENTIC_LOOP_LOCAL_OWNER_TOKENS and AGENTIC_LOOP_ALLOW_FIRESTORE_EMULATOR must not be set when APP_ENV=production")
	}
	var ownerTokens map[string]string
	if localOwnerTokensValue != "" {
		if ownerTokens, err = parseOwnerBearerTokens(localOwnerTokensValue, owners); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// preview-local (roadmap M2) is the only grade allowed to point this
	// binary at the Firestore emulator, and only when explicitly opted in:
	// the production constructor (firestore.NewClient/NewStore) already
	// refuses an emulator host on its own, so this is defense in depth, not
	// the sole guard.
	var store *firestore.Store
	if emulatorHost := strings.TrimSpace(os.Getenv("FIRESTORE_EMULATOR_HOST")); emulatorHost != "" {
		if !allowEmulator {
			return errors.New("FIRESTORE_EMULATOR_HOST is set but AGENTIC_LOOP_ALLOW_FIRESTORE_EMULATOR=1 was not; refusing to start against an emulator")
		}
		store, err = firestore.NewEmulatorClient(ctx, project, installation)
	} else {
		store, err = firestore.NewClient(ctx, project, installation)
	}
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
	// V2-088: where an error the caller must not be shown goes instead of
	// nowhere. os.Stdout is the destination in BOTH deploy grades -- Cloud Run
	// hands a container's stdout to Cloud Logging, and preview-local runs this
	// binary in the owner's own terminal. Nothing is written to Firestore,
	// because a caller-provokable write would drain the daily write budget
	// that api's own 429 branch guards. The --version branch above returns
	// before this function is reached, so `make smoke` still sees exactly the
	// version on stdout.
	operatorRecorder, err := api.NewJSONOperatorRecorder(os.Stdout, clock)
	if err != nil {
		return err
	}
	leaseReconciler := &reconciler.Reconciler{Tx: store, Clock: clock}
	verificationReconciler := &reconciler.VerificationReconciler{Tx: store, Clock: clock, Deadline: time.Minute}
	var authenticator api.Authenticator = api.CombinedAuthenticator{Runner: runnerEnrollment, OwnerEmails: owners, SchedulerIdentity: strings.ToLower(reconcileIdentity)}
	if ownerTokens != nil {
		authenticator = api.LocalOwnerBearerAuthenticator{Runner: runnerEnrollment, OwnerTokens: ownerTokens}
	}
	h := api.New(api.Config{Authenticator: authenticator, Service: service, RunnerEnrollment: runnerEnrollment, AllowedOrigins: origins, ReconcileIdentity: strings.ToLower(reconcileIdentity), OperatorRecorder: operatorRecorder, InternalReconcile: func(callCtx context.Context) error {
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
