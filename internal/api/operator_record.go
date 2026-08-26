package api

// This file is the operator's half of V2-083's repair. V2-083 stopped the
// CALLER from receiving a dependency's own error text; it did not make that
// text exist anywhere, and measured that it existed nowhere -- no non-test
// file under internal/ or cmd/ imports "log" or "log/slog". Two branches in
// api.go therefore lose a failure completely: domainError's unclassified
// branch answers 500 with a constant and drops err on the floor, and
// ServeHTTP's recover branch did not even bind what it recovered.
//
// The whole content of this file is to give those two branches ONE
// destination, and the destination is chosen by AUDIENCE rather than by
// sensitivity: a single-line JSON record on an io.Writer the Handler holds --
// os.Stdout in the shipped binary, which Cloud Run hands to Cloud Logging and
// which preview-local puts in the owner's own terminal. The recorder never
// receives the http.ResponseWriter and never receives the *http.Request, so
// the response cannot change by one byte no matter what a call site passes.
//
// Nothing here writes to Firestore, and that is a measured refusal rather
// than a preference: a Firestore write consumes the daily write budget that
// domainError's FIRST branch answers 429 quota_exhausted for, so a caller who
// can provoke a 500 could drain the installation's write budget through the
// error path and stop every mutation -- observability would become a
// denial-of-service amplifier. The fault most worth recording is itself
// usually a Firestore fault, so a Firestore-backed record would also be lost
// exactly when it is needed.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/runner"
)

// The kind field is one of exactly these three values. There is no fourth,
// and a call site cannot invent one without editing this file.
const (
	// OperatorRecordUnclassifiedError is domainError's unclassified branch:
	// an error no sentinel identified, answered as 500 internal_error.
	OperatorRecordUnclassifiedError = "unclassified_error"
	// OperatorRecordRecoveredPanic is ServeHTTP's recover branch, which is
	// the same defect measured a second time: a panic is by definition an
	// error nobody classified and nobody may show a caller.
	OperatorRecordRecoveredPanic = "recovered_panic"
	// OperatorRecordSuppressionStarted is the rate cap's marker. It is the
	// only kind no call site can ask for: the recorder emits it itself.
	OperatorRecordSuppressionStarted = "suppression_started"
)

const (
	// operatorRecordFieldLimit caps every string the recorder copies out of a
	// request, in BYTES. 256 is the same cap internal/provider's safeMessage
	// applies to a provider failure message.
	operatorRecordFieldLimit = 256
	// operatorRecordWindow and operatorRecordsPerWindow are the rate cap. It
	// is a fixed window evaluated lazily against the INJECTED clock on each
	// attempt: no timer, no goroutine, no background sweeper. These are
	// constants and not configuration on purpose -- this feature adds no
	// environment variable and no knob.
	operatorRecordWindow     = time.Minute
	operatorRecordsPerWindow = 32
)

// OperatorObservation is everything a call site may supply: the kind plus the
// four values api.go is allowed to read. There is deliberately no field for a
// request body, a response body, a header, a query string or a stack trace,
// because a closed struct is the difference between agreeing not to record a
// body and there being no field a body could go in.
type OperatorObservation struct {
	Kind          string
	CorrelationID string
	Method        string
	Path          string
	Detail        string
}

// OperatorRecorder is the port api.Config holds. A nil value means record
// nothing, which is what keeps every pre-existing test's output unchanged.
type OperatorRecorder interface {
	RecordOperatorObservation(observation OperatorObservation)
}

// operatorRecordLine is the emitted line, as a closed struct with json tags
// and no map and no free-form field. window_seconds and records_per_window
// are pointers so they appear ONLY on the suppression marker; every other key
// is always present, so a reader can compare the key set exactly.
type operatorRecordLine struct {
	SchemaVersion    string `json:"schema_version"`
	Kind             string `json:"kind"`
	Severity         string `json:"severity"`
	ObservedAt       string `json:"observed_at"`
	CorrelationID    string `json:"correlation_id"`
	Method           string `json:"method"`
	Path             string `json:"path"`
	Status           int    `json:"status"`
	Error            string `json:"error"`
	Detail           string `json:"detail"`
	DetailTruncated  bool   `json:"detail_truncated"`
	SuppressedBefore int    `json:"suppressed_before"`
	WindowSeconds    *int   `json:"window_seconds,omitempty"`
	RecordsPerWindow *int   `json:"records_per_window,omitempty"`
}

// JSONOperatorRecorder writes one JSON object per line to a writer it is
// given once. All of the window state, the mutex and both size caps live
// inside this value, so a cap cannot be reset by a code path that rebuilds
// the Handler: cmd/control-plane constructs exactly one of these.
type JSONOperatorRecorder struct {
	mu       sync.Mutex
	out      io.Writer
	clock    application.Clock
	windowAt time.Time
	inWindow int
	marked   bool
	dropped  int
}

// NewJSONOperatorRecorder refuses a nil writer and a nil clock. Refusing the
// clock is what makes "the recorder never reads the wall clock" a property of
// the type rather than a habit: there is no fallback to time.Now to fall back
// to.
func NewJSONOperatorRecorder(w io.Writer, clock application.Clock) (*JSONOperatorRecorder, error) {
	if w == nil {
		return nil, errors.New("operator recorder needs a writer")
	}
	if clock == nil {
		return nil, errors.New("operator recorder needs a clock")
	}
	return &JSONOperatorRecorder{out: w, clock: clock}, nil
}

// RecordOperatorObservation emits at most one line. The worst case inside one
// window is 33 lines: operatorRecordsPerWindow records, then exactly one
// suppression marker. Every further attempt in the window writes nothing and
// increments a counter, and the next line actually emitted carries that count
// in suppressed_before and resets it, so the fact that lines were dropped is
// visible twice -- immediately, and then exactly.
func (r *JSONOperatorRecorder) RecordOperatorObservation(observation OperatorObservation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock.Now().UTC()
	if r.windowAt.IsZero() || now.Sub(r.windowAt) >= operatorRecordWindow {
		r.windowAt = now
		r.inWindow = 0
		r.marked = false
	}
	if r.inWindow >= operatorRecordsPerWindow {
		if r.marked {
			r.dropped++
			return
		}
		r.marked = true
		window := int(operatorRecordWindow / time.Second)
		perWindow := operatorRecordsPerWindow
		// The marker keeps the tripping observation's identity, which is what
		// makes it joinable, and carries no detail at all.
		line := r.line(OperatorRecordSuppressionStarted, observation, now)
		line.Detail = ""
		line.DetailTruncated = false
		line.WindowSeconds = &window
		line.RecordsPerWindow = &perWindow
		r.write(line)
		return
	}
	r.inWindow++
	line := r.line(observation.Kind, observation, now)
	line.SuppressedBefore = r.dropped
	r.dropped = 0
	r.write(line)
}

// line builds the record. Both branches this recorder serves answer 500
// internal_error, so status and error are constants here: they say what the
// CALLER was told, which is what makes a record joinable to a complaint.
func (r *JSONOperatorRecorder) line(kind string, observation OperatorObservation, now time.Time) operatorRecordLine {
	detail, detailTruncated := boundedOperatorField(observation.Detail)
	path, pathTruncated := boundedOperatorField(observation.Path)
	// The correlation id and the method are bounded the same way even though
	// no acceptance item asks for it: api.ServeHTTP accepts a
	// caller-supplied X-Correlation-ID header of unbounded length, so without
	// this a caller could grow a record without limit, which is exactly what
	// "bounded" forbids. Their truncation is not reported in
	// detail_truncated, which stays what it says it is -- the fact about
	// detail and path.
	correlationID, _ := boundedOperatorField(observation.CorrelationID)
	method, _ := boundedOperatorField(observation.Method)
	return operatorRecordLine{
		SchemaVersion:   "v1",
		Kind:            kind,
		Severity:        "ERROR",
		ObservedAt:      now.Format(time.RFC3339Nano),
		CorrelationID:   correlationID,
		Method:          method,
		Path:            path,
		Status:          http.StatusInternalServerError,
		Error:           "internal_error",
		Detail:          detail,
		DetailTruncated: detailTruncated || pathTruncated,
	}
}

func (r *JSONOperatorRecorder) write(line operatorRecordLine) {
	encoded, err := json.Marshal(line)
	if err != nil {
		return
	}
	// One Write per line, under the mutex, so two concurrent requests cannot
	// interleave halves of a record. A failed write is dropped rather than
	// propagated: a record that cannot be written must not change what the
	// caller is answered.
	_, _ = r.out.Write(append(encoded, '\n'))
}

// boundedOperatorField is the ONLY redaction mechanism in this package.
// runner.RedactLog is the same deny-list internal/runner/log.go's BoundedLog
// already applies to every diagnostic write in this repository, reached over
// the already-declared api -> runner dependency edge. No secret regular
// expression is copied into internal/api, because a second copy of a pattern
// is a copy that can drift.
func boundedOperatorField(value string) (string, bool) {
	redacted := runner.RedactLog(value)
	if len(redacted) <= operatorRecordFieldLimit {
		return redacted, false
	}
	return redacted[:operatorRecordFieldLimit], true
}

// operatorPanicText turns a recovered value into the record's detail. It is
// the only place a panic's value is read, and the value never reaches the
// response.
func operatorPanicText(recovered any) string {
	switch v := recovered.(type) {
	case error:
		return v.Error()
	case string:
		return v
	default:
		return fmt.Sprintf("%v", recovered)
	}
}

// recordOperatorObservation is the single seam api.go uses. A nil recorder is
// the default and records nothing.
func (h *Handler) recordOperatorObservation(kind, correlationID, method, path, detail string) {
	if h.config.OperatorRecorder == nil {
		return
	}
	h.config.OperatorRecorder.RecordOperatorObservation(OperatorObservation{
		Kind:          kind,
		CorrelationID: correlationID,
		Method:        method,
		Path:          path,
		Detail:        detail,
	})
}
