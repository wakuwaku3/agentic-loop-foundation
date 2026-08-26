package api_test

// V2-088 A5, A12 and A13: the recorder is proved ALONE, with no HTTP
// involved. Every assertion here is by equality on the emitted bytes, because
// equality is the only assertion strong enough to prove that the field set is
// CLOSED -- a weaker check passes while a new field quietly appears.

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/api"
	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
	"github.com/takushi/agentic-loop-foundation/v2/internal/runner"
)

// operatorRecordKeys is the declared key set of an ordinary record. The
// suppression marker carries these plus window_seconds and
// records_per_window, and nothing else ever does.
var operatorRecordKeys = []string{
	"correlation_id", "detail", "detail_truncated", "error", "kind", "method",
	"observed_at", "path", "schema_version", "severity", "status", "suppressed_before",
}

// operatorRecordInstant is the same fixed instant the package's existing
// framingClock fixtures start from, so every observed_at in this file is a
// constant and the emitted line can be compared byte for byte.
func operatorRecordInstant() time.Time { return time.Unix(1700000000, 0).UTC() }

// operatorSecretFixtures builds the secret-shaped fixtures by CONCATENATING a
// prefix with a suffix, so the pattern exists at run time and not in this
// file. A literal bearer token, PEM header or gh token written here would trip
// the repository's own secrets check even though it is a fixture.
func operatorSecretFixtures() []string {
	return []string{
		"Bearer " + "abcdefghijklmnopqrstuvwxyz012345",
		"-----BEGIN " + "RSA PRIVATE KEY-----",
		"gh" + "p_" + "0123456789abcdefghij0123456789",
	}
}

// operatorRecordLines splits a sink into the lines it received, refusing a
// sink that does not end in exactly one newline per line.
func operatorRecordLines(t *testing.T, sink *bytes.Buffer) []string {
	t.Helper()
	raw := sink.String()
	if raw == "" {
		return nil
	}
	if !strings.HasSuffix(raw, "\n") {
		t.Fatalf("the sink does not end in a newline, so a line is unterminated: %q", raw)
	}
	return strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
}

func operatorRecordKeySet(t *testing.T, line string) []string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(line), &doc); err != nil {
		t.Fatalf("the emitted line is not one JSON object: %v (%q)", err, line)
	}
	keys := make([]string, 0, len(doc))
	for key := range doc {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// TestTheOperatorRecordIsAClosedBoundedLine is A5 (a)-(d) and (f), A12's
// cross-check against internal/provider's independently authored deny-list,
// and A13's source-level determinism assertion.
func TestTheOperatorRecordIsAClosedBoundedLine(t *testing.T) {
	// --- (f) the constructor refuses a nil writer and a nil clock ---------
	if _, err := api.NewJSONOperatorRecorder(nil, &framingClock{at: operatorRecordInstant()}); err == nil {
		t.Fatal("NewJSONOperatorRecorder accepted a nil writer")
	}
	var probe bytes.Buffer
	if _, err := api.NewJSONOperatorRecorder(&probe, nil); err == nil {
		t.Fatal("NewJSONOperatorRecorder accepted a nil clock")
	}
	if probe.Len() != 0 {
		t.Fatalf("a refused constructor wrote %d bytes", probe.Len())
	}

	// corpus collects every line this test emits, for the A12 cross-check.
	var corpus []string

	// --- (a) one observation, one line, equal byte for byte --------------
	var sink bytes.Buffer
	clk := &framingClock{at: operatorRecordInstant()}
	rec, err := api.NewJSONOperatorRecorder(&sink, clk)
	if err != nil {
		t.Fatal(err)
	}
	rec.RecordOperatorObservation(api.OperatorObservation{
		Kind:          api.OperatorRecordUnclassifiedError,
		CorrelationID: "cid-1",
		Method:        "POST",
		Path:          "/v1/requirements",
		Detail:        "rpc error: code = InvalidArgument desc = a fault the request did not cause",
	})
	const wantLine = `{"schema_version":"v1","kind":"unclassified_error","severity":"ERROR",` +
		`"observed_at":"2023-11-14T22:13:20Z","correlation_id":"cid-1","method":"POST",` +
		`"path":"/v1/requirements","status":500,"error":"internal_error",` +
		`"detail":"rpc error: code = InvalidArgument desc = a fault the request did not cause",` +
		`"detail_truncated":false,"suppressed_before":0}` + "\n"
	if sink.String() != wantLine {
		t.Fatalf("the emitted line is not byte-identical to the expected record\n got %q\nwant %q", sink.String(), wantLine)
	}
	lines := operatorRecordLines(t, &sink)
	if len(lines) != 1 {
		t.Fatalf("one observation produced %d lines, want exactly 1", len(lines))
	}
	corpus = append(corpus, lines...)

	// --- (b) the key set is EXACTLY the declared set ---------------------
	if got := operatorRecordKeySet(t, lines[0]); !reflect.DeepEqual(got, operatorRecordKeys) {
		t.Fatalf("record key set = %v, want exactly %v", got, operatorRecordKeys)
	}

	// A recovered panic uses the same closed shape, so a second kind cannot
	// smuggle a field in.
	sink.Reset()
	clk.nextDay()
	rec.RecordOperatorObservation(api.OperatorObservation{
		Kind:          api.OperatorRecordRecoveredPanic,
		CorrelationID: "cid-2",
		Method:        "GET",
		Path:          "/v1/queue/summary",
		Detail:        "a panic nobody classified",
	})
	panicLines := operatorRecordLines(t, &sink)
	if len(panicLines) != 1 {
		t.Fatalf("a recovered-panic observation produced %d lines, want 1", len(panicLines))
	}
	if got := operatorRecordKeySet(t, panicLines[0]); !reflect.DeepEqual(got, operatorRecordKeys) {
		t.Fatalf("recovered_panic key set = %v, want exactly %v", got, operatorRecordKeys)
	}
	var panicDoc map[string]any
	if err := json.Unmarshal([]byte(panicLines[0]), &panicDoc); err != nil {
		t.Fatal(err)
	}
	if panicDoc["kind"] != api.OperatorRecordRecoveredPanic {
		t.Fatalf("kind = %v, want %q", panicDoc["kind"], api.OperatorRecordRecoveredPanic)
	}
	corpus = append(corpus, panicLines...)

	// --- (c) redaction, in the detail AND in the path --------------------
	secrets := operatorSecretFixtures()
	for _, where := range []string{"detail", "path"} {
		for _, secret := range secrets {
			sink.Reset()
			clk.nextDay()
			observation := api.OperatorObservation{
				Kind:          api.OperatorRecordUnclassifiedError,
				CorrelationID: "cid-secret",
				Method:        "POST",
				Path:          "/v1/requirements",
				Detail:        "a fault carrying credential-shaped text",
			}
			if where == "detail" {
				observation.Detail = "the dependency said " + secret + " and stopped"
			} else {
				observation.Path = "/v1/requirements/" + secret
			}
			rec.RecordOperatorObservation(observation)
			got := operatorRecordLines(t, &sink)
			if len(got) != 1 {
				t.Fatalf("%s secret observation produced %d lines, want 1", where, len(got))
			}
			line := got[0]
			if strings.Contains(line, secret) {
				t.Fatalf("the emitted record carries the secret-shaped value verbatim in the %s: %s", where, line)
			}
			if !strings.Contains(line, "[REDACTED]") {
				t.Fatalf("the emitted record does not carry [REDACTED] where the secret was (%s): %s", where, line)
			}
			// Idempotence: the shared deny-list finds nothing left to redact.
			if again := runner.RedactLog(line); again != line {
				t.Fatalf("runner.RedactLog is not idempotent on the emitted line\n got %q\nwant %q", again, line)
			}
			if got := operatorRecordKeySet(t, line); !reflect.DeepEqual(got, operatorRecordKeys) {
				t.Fatalf("redacted record key set = %v, want exactly %v", got, operatorRecordKeys)
			}
			corpus = append(corpus, line)
		}
	}

	// --- (d) truncation at 256 bytes, reported and not hidden ------------
	for _, tc := range []struct{ name, field string }{{"detail", "detail"}, {"path", "path"}} {
		sink.Reset()
		clk.nextDay()
		observation := api.OperatorObservation{
			Kind:          api.OperatorRecordUnclassifiedError,
			CorrelationID: "cid-long",
			Method:        "POST",
			Path:          "/v1/requirements",
			Detail:        "short",
		}
		if tc.field == "detail" {
			observation.Detail = strings.Repeat("d", 300)
		} else {
			observation.Path = "/" + strings.Repeat("p", 299)
		}
		rec.RecordOperatorObservation(observation)
		got := operatorRecordLines(t, &sink)
		if len(got) != 1 {
			t.Fatalf("%s truncation observation produced %d lines, want 1", tc.name, len(got))
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(got[0]), &doc); err != nil {
			t.Fatal(err)
		}
		value, _ := doc[tc.field].(string)
		if len(value) != 256 {
			t.Fatalf("%s length = %d, want exactly 256 bytes", tc.field, len(value))
		}
		if doc["detail_truncated"] != true {
			t.Fatalf("detail_truncated = %v after a %s longer than 256 bytes", doc["detail_truncated"], tc.field)
		}
		if got := operatorRecordKeySet(t, got[0]); !reflect.DeepEqual(got, operatorRecordKeys) {
			t.Fatalf("truncated record key set = %v, want exactly %v", got, operatorRecordKeys)
		}
		corpus = append(corpus, got[0])
	}

	// --- A12: the two independently authored deny-lists are MEASURED to
	// agree, instead of one being trusted. internal/provider is imported
	// from the test only: api -> provider is declared in
	// verification_dependencies and a source import would demand an edit to
	// the prohibited ci/components.json.
	if len(corpus) == 0 {
		t.Fatal("the A12 cross-check corpus is empty")
	}
	for _, line := range corpus {
		if provider.SecretPatternMatches([]byte(line)) {
			t.Fatalf("internal/provider's independently authored deny-list still matches an emitted record: %s", line)
		}
	}
	// Positive control: the same predicate DOES match an unredacted fixture,
	// so the loop above is measuring something.
	if !provider.SecretPatternMatches([]byte(secrets[0])) {
		t.Fatal("provider.SecretPatternMatches does not match the bearer fixture, so the cross-check proves nothing")
	}
	t.Logf("A12 cross-check: %d emitted lines, none matched by provider.SecretPatternMatches", len(corpus))

	// --- A13: determinism as a SOURCE property ---------------------------
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "operator_record.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	forbiddenTimeSelectors := map[string]bool{
		"Now": true, "Timer": true, "Ticker": true, "NewTimer": true, "NewTicker": true,
		"After": true, "AfterFunc": true, "Sleep": true, "Tick": true, "Since": true, "Until": true,
	}
	selectors := 0
	ast.Inspect(file, func(n ast.Node) bool {
		if _, ok := n.(*ast.GoStmt); ok {
			t.Error("internal/api/operator_record.go contains a go statement; the recorder must hold no goroutine")
			return true
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		selectors++
		if ident.Name == "time" && forbiddenTimeSelectors[sel.Sel.Name] {
			t.Errorf("internal/api/operator_record.go calls time.%s; the recorder reads time only from the injected clock", sel.Sel.Name)
		}
		if ident.Name == "rand" {
			t.Errorf("internal/api/operator_record.go uses rand.%s; the recorder has no source of randomness", sel.Sel.Name)
		}
		return true
	})
	if selectors == 0 {
		t.Fatal("the AST scan found zero selector expressions, so it is not scanning the file")
	}
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		if path == "math/rand" || path == "math/rand/v2" || path == "crypto/rand" {
			t.Errorf("internal/api/operator_record.go imports %q", path)
		}
	}
	t.Logf("A13 source scan: %d selector expressions and %d imports inspected", selectors, len(file.Imports))
}

// TestTheOperatorRecordIsBoundedInRate is A5 (e): the 33-line worst case per
// window, the marker, and the exact dropped count on the next line emitted.
func TestTheOperatorRecordIsBoundedInRate(t *testing.T) {
	var sink bytes.Buffer
	clk := &framingClock{at: operatorRecordInstant()}
	rec, err := api.NewJSONOperatorRecorder(&sink, clk)
	if err != nil {
		t.Fatal(err)
	}
	const attempts = 200
	for i := 0; i < attempts; i++ {
		rec.RecordOperatorObservation(api.OperatorObservation{
			Kind:          api.OperatorRecordUnclassifiedError,
			CorrelationID: "cid-flood",
			Method:        "GET",
			Path:          "/v1/requirements",
			Detail:        "the same fault, over and over",
		})
	}
	lines := operatorRecordLines(t, &sink)
	if len(lines) != 33 {
		t.Fatalf("%d observations inside one window produced %d lines, want exactly 33 (32 records plus one marker)", attempts, len(lines))
	}
	for i, line := range lines[:32] {
		var doc map[string]any
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			t.Fatal(err)
		}
		if doc["kind"] != api.OperatorRecordUnclassifiedError {
			t.Fatalf("line %d kind = %v, want %q", i, doc["kind"], api.OperatorRecordUnclassifiedError)
		}
		if doc["suppressed_before"] != float64(0) {
			t.Fatalf("line %d suppressed_before = %v, want 0", i, doc["suppressed_before"])
		}
		if got := operatorRecordKeySet(t, line); !reflect.DeepEqual(got, operatorRecordKeys) {
			t.Fatalf("line %d key set = %v, want exactly %v", i, got, operatorRecordKeys)
		}
	}
	var marker map[string]any
	if err := json.Unmarshal([]byte(lines[32]), &marker); err != nil {
		t.Fatal(err)
	}
	if marker["kind"] != api.OperatorRecordSuppressionStarted {
		t.Fatalf("the 33rd line kind = %v, want %q", marker["kind"], api.OperatorRecordSuppressionStarted)
	}
	if marker["window_seconds"] != float64(60) {
		t.Fatalf("marker window_seconds = %v, want 60", marker["window_seconds"])
	}
	if marker["records_per_window"] != float64(32) {
		t.Fatalf("marker records_per_window = %v, want 32", marker["records_per_window"])
	}
	if marker["detail"] != "" {
		t.Fatalf("the marker carries a detail: %v", marker["detail"])
	}
	wantMarkerKeys := append(append([]string{}, operatorRecordKeys...), "records_per_window", "window_seconds")
	sort.Strings(wantMarkerKeys)
	if got := operatorRecordKeySet(t, lines[32]); !reflect.DeepEqual(got, wantMarkerKeys) {
		t.Fatalf("marker key set = %v, want exactly %v", got, wantMarkerKeys)
	}

	// The next line ACTUALLY EMITTED carries the exact number dropped, and
	// then the counter resets. 200 attempts minus 33 lines is 167 drops.
	sink.Reset()
	clk.nextDay()
	rec.RecordOperatorObservation(api.OperatorObservation{
		Kind:          api.OperatorRecordUnclassifiedError,
		CorrelationID: "cid-after",
		Method:        "GET",
		Path:          "/v1/requirements",
		Detail:        "the first record of the next window",
	})
	after := operatorRecordLines(t, &sink)
	if len(after) != 1 {
		t.Fatalf("the first observation of the next window produced %d lines, want 1", len(after))
	}
	var resumed map[string]any
	if err := json.Unmarshal([]byte(after[0]), &resumed); err != nil {
		t.Fatal(err)
	}
	if resumed["suppressed_before"] != float64(attempts-33) {
		t.Fatalf("suppressed_before = %v, want exactly %d", resumed["suppressed_before"], attempts-33)
	}
	if got := operatorRecordKeySet(t, after[0]); !reflect.DeepEqual(got, operatorRecordKeys) {
		t.Fatalf("resumed record key set = %v, want exactly %v", got, operatorRecordKeys)
	}

	sink.Reset()
	rec.RecordOperatorObservation(api.OperatorObservation{
		Kind:          api.OperatorRecordUnclassifiedError,
		CorrelationID: "cid-after-2",
		Method:        "GET",
		Path:          "/v1/requirements",
		Detail:        "the second record of the next window",
	})
	second := operatorRecordLines(t, &sink)
	if len(second) != 1 {
		t.Fatalf("the second observation of the next window produced %d lines, want 1", len(second))
	}
	var reset map[string]any
	if err := json.Unmarshal([]byte(second[0]), &reset); err != nil {
		t.Fatal(err)
	}
	if reset["suppressed_before"] != float64(0) {
		t.Fatalf("suppressed_before = %v after the count was reported, want 0", reset["suppressed_before"])
	}
	t.Logf("bounding: %d attempts produced 33 lines, then suppressed_before=%d and a reset to 0", attempts, attempts-33)
}
