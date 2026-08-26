package domain

// Tests for the Repository aggregate (V2-064). Every assertion here is pure:
// no clock is read, no sleep is taken, no goroutine is started, and every
// staleness question is answered against an explicitly supplied instant.

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"
)

func mustLocator(t *testing.T, raw string) SourceLocator {
	t.Helper()
	locator, err := ParseSourceLocator(raw)
	if err != nil {
		t.Fatalf("ParseSourceLocator(%q): %v", raw, err)
	}
	return locator
}

// TestSourceLocatorEquivalenceTable is acceptance A4's table: every form a
// person or a tool actually writes for the same repository must normalise to
// one locator, so the duplicate constraint below cannot be defeated by
// spelling.
func TestSourceLocatorEquivalenceTable(t *testing.T) {
	want := SourceLocator{Forge: ForgeGitHub, Host: "github.com", Owner: "o", Name: "n"}
	equivalent := []string{
		"https://github.com/O/N",
		"https://github.com/O/N.git",
		"git@github.com:O/N.git",
		"https://GitHub.com/o/n",
		"http://github.com/O/N",
		"ssh://git@github.com/O/N.git",
		"git://github.com/O/N.git",
		"github.com/O/N",
		"O/N",
		"  https://github.com/O/N.git  ",
		"ssh://git@github.com:22/O/N.git",
	}
	for _, raw := range equivalent {
		got, err := ParseSourceLocator(raw)
		if err != nil {
			t.Fatalf("ParseSourceLocator(%q): %v", raw, err)
		}
		if got != want {
			t.Fatalf("ParseSourceLocator(%q) = %+v, want %+v", raw, got, want)
		}
		if got.Key() != "github/o/n" {
			t.Fatalf("ParseSourceLocator(%q).Key() = %q, want %q", raw, got.Key(), "github/o/n")
		}
	}
	// A credential-bearing URL form loses the credential in parsing: the
	// userinfo section is dropped before anything is stored, so no field of
	// the resulting locator can carry it.
	withUserinfo, err := ParseSourceLocator("https://someone:not-a-real-value@github.com/O/N.git")
	if err != nil {
		t.Fatalf("userinfo form: %v", err)
	}
	if withUserinfo != want {
		t.Fatalf("userinfo form = %+v, want %+v", withUserinfo, want)
	}
	for _, field := range []string{withUserinfo.Forge, withUserinfo.Host, withUserinfo.Owner, withUserinfo.Name, withUserinfo.DefaultBranch} {
		if strings.Contains(field, "@") || strings.Contains(field, "not-a-real-value") {
			t.Fatalf("locator field %q retained userinfo", field)
		}
	}
}

func TestSourceLocatorRejectsNonRepositoryCoordinates(t *testing.T) {
	for _, raw := range []string{
		"",
		"   ",
		"https://github.com/",
		"https://github.com/O",
		"https://github.com/O/N/extra",
		"owner/name/extra",
		"N",
		"https://github.com//N",
		"https://github.com/O//",
		"https://github.com/O/N with space",
	} {
		if got, err := ParseSourceLocator(raw); err == nil {
			t.Fatalf("ParseSourceLocator(%q) accepted and produced %+v", raw, got)
		} else if !errors.Is(err, ErrInvalidSourceLocator) {
			t.Fatalf("ParseSourceLocator(%q) error = %v, want ErrInvalidSourceLocator", raw, err)
		}
	}
}

func TestNormalizeSourceLocatorRefusesPathSeparatorsAndUnknownForge(t *testing.T) {
	base := SourceLocator{Forge: ForgeGitHub, Host: "github.com", Owner: "o", Name: "n"}
	cases := map[string]SourceLocator{
		"empty owner":            {Forge: ForgeGitHub, Host: "github.com", Owner: "", Name: "n"},
		"empty name":             {Forge: ForgeGitHub, Host: "github.com", Owner: "o", Name: ""},
		"owner with slash":       {Forge: ForgeGitHub, Host: "github.com", Owner: "o/x", Name: "n"},
		"name with slash":        {Forge: ForgeGitHub, Host: "github.com", Owner: "o", Name: "n/x"},
		"owner with backslash":   {Forge: ForgeGitHub, Host: "github.com", Owner: `o\x`, Name: "n"},
		"name with backslash":    {Forge: ForgeGitHub, Host: "github.com", Owner: "o", Name: `n\x`},
		"host with slash":        {Forge: ForgeGitHub, Host: "github.com/x", Owner: "o", Name: "n"},
		"branch with backslash":  {Forge: ForgeGitHub, Host: "github.com", Owner: "o", Name: "n", DefaultBranch: `main\x`},
		"branch with whitespace": {Forge: ForgeGitHub, Host: "github.com", Owner: "o", Name: "n", DefaultBranch: "ma in"},
	}
	for name, locator := range cases {
		if _, err := NormalizeSourceLocator(locator); !errors.Is(err, ErrInvalidSourceLocator) {
			t.Fatalf("%s: error = %v, want ErrInvalidSourceLocator", name, err)
		}
	}
	if _, err := NormalizeSourceLocator(SourceLocator{Forge: "gitlab", Host: "gitlab.com", Owner: "o", Name: "n"}); !errors.Is(err, ErrUnknownForge) {
		t.Fatalf("unknown forge accepted")
	}
	// Defaults: an absent forge and host resolve, and ".git" is stripped
	// from the name only.
	got, err := NormalizeSourceLocator(SourceLocator{Owner: "O", Name: "N.GIT", DefaultBranch: "feature/x"})
	if err != nil {
		t.Fatal(err)
	}
	if got != (SourceLocator{Forge: ForgeGitHub, Host: DefaultForgeHost, Owner: "o", Name: "n", DefaultBranch: "feature/x"}) {
		t.Fatalf("defaults = %+v", got)
	}
	if !base.Recorded() || (SourceLocator{}).Recorded() {
		t.Fatal("Recorded does not distinguish a populated locator from a zero one")
	}
	// ".git" alone is a legitimate one-character-plus-suffix name and must
	// not be trimmed away into emptiness.
	if _, err := NormalizeSourceLocator(SourceLocator{Owner: "o", Name: ".git"}); err != nil {
		t.Fatalf("name %q rejected: %v", ".git", err)
	}
}

func TestRepositoryIDIsOpaqueAndRejectsEmpty(t *testing.T) {
	if _, err := NewRepositoryID("  "); !errors.Is(err, ErrEmptyID) {
		t.Fatalf("blank repository id accepted: %v", err)
	}
	id, err := NewRepositoryID("repository-1")
	if err != nil || id.String() != "repository-1" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}

func TestDecideRepositoryRegisterAndRetire(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC()
	pending := Repository{ID: "repository-1", Locator: mustLocator(t, "https://GitHub.com/O/N.git"), RequestedBy: RequestedBy{ActorType: ActorTypeOwner, Subject: "owner"}}

	registered, err := DecideRepository(pending, RepositoryCommand{Kind: RepositoryRegister, Actor: "owner", At: at, ExpectedVersion: 0})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if registered.Status != RepositoryRegistered || registered.Version != 1 {
		t.Fatalf("register produced %+v", registered)
	}
	if err := ValidateRepository(registered); err != nil {
		t.Fatalf("registered repository is invalid: %v", err)
	}

	retired, err := DecideRepository(registered, RepositoryCommand{Kind: RepositoryRetire, Actor: "owner", At: at, ExpectedVersion: 1})
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if retired.Status != RepositoryRetired || retired.Version != 2 {
		t.Fatalf("retire produced %+v", retired)
	}
	// The retired aggregate still exists and still validates: retire is a
	// transition, never a deletion.
	if err := ValidateRepository(retired); err != nil {
		t.Fatalf("retired repository is invalid: %v", err)
	}
	// Retiring twice is refused, and so is registering over a registered
	// Repository.
	if _, err := DecideRepository(retired, RepositoryCommand{Kind: RepositoryRetire, Actor: "owner", At: at, ExpectedVersion: 2}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("double retire error = %v, want ErrInvalidTransition", err)
	}
	if _, err := DecideRepository(registered, RepositoryCommand{Kind: RepositoryRegister, Actor: "owner", At: at, ExpectedVersion: 1}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("re-register error = %v, want ErrInvalidTransition", err)
	}
}

func TestDecideRepositoryRefusals(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC()
	pending := Repository{ID: "repository-1", Locator: mustLocator(t, "O/N")}

	if _, err := DecideRepository(pending, RepositoryCommand{Kind: RepositoryRegister, At: at}); err == nil {
		t.Fatal("missing actor accepted")
	}
	if _, err := DecideRepository(pending, RepositoryCommand{Kind: RepositoryRegister, Actor: "owner"}); err == nil {
		t.Fatal("zero timestamp accepted")
	}
	if _, err := DecideRepository(pending, RepositoryCommand{Kind: RepositoryRegister, Actor: "owner", At: at, ExpectedVersion: 7}); !errors.Is(err, ErrStaleVersion) {
		t.Fatal("stale expected version accepted")
	}
	if _, err := DecideRepository(pending, RepositoryCommand{Kind: "resurrect", Actor: "owner", At: at}); err == nil {
		t.Fatal("unknown command accepted")
	}
	if _, err := DecideRepository(Repository{Locator: pending.Locator}, RepositoryCommand{Kind: RepositoryRegister, Actor: "owner", At: at}); !errors.Is(err, ErrEmptyID) {
		t.Fatal("empty repository id accepted")
	}
	if _, err := DecideRepository(Repository{ID: "repository-1"}, RepositoryCommand{Kind: RepositoryRegister, Actor: "owner", At: at}); !errors.Is(err, ErrInvalidSourceLocator) {
		t.Fatal("empty locator accepted")
	}
}

func TestValidateRepositoryRefusals(t *testing.T) {
	good := Repository{ID: "repository-1", Locator: mustLocator(t, "O/N"), Status: RepositoryRegistered, Version: 1}
	if err := ValidateRepository(good); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRepository(Repository{Locator: good.Locator, Status: RepositoryRegistered}); !errors.Is(err, ErrEmptyID) {
		t.Fatal("empty id accepted")
	}
	if err := ValidateRepository(Repository{ID: "r", Locator: good.Locator, Status: "archived"}); err == nil {
		t.Fatal("unknown status accepted")
	}
	if err := ValidateRepository(Repository{ID: "r", Locator: good.Locator, Status: ""}); err == nil {
		t.Fatal("empty status accepted")
	}
	notNormalised := good
	notNormalised.Locator.Owner = "O"
	if err := ValidateRepository(notNormalised); !errors.Is(err, ErrInvalidSourceLocator) {
		t.Fatal("non-normalised locator accepted")
	}
	badActor := good
	badActor.RequestedBy = RequestedBy{ActorType: "robot", Subject: "x"}
	if err := ValidateRepository(badActor); err == nil {
		t.Fatal("unknown requested_by actor type accepted")
	}
	// A zero RequestedBy stays legitimate, exactly as on Requirement.
	zeroActor := good
	zeroActor.RequestedBy = RequestedBy{}
	if err := ValidateRepository(zeroActor); err != nil {
		t.Fatalf("zero requested_by rejected: %v", err)
	}
}

func TestRepositoryObservationStalenessUsesSuppliedInstant(t *testing.T) {
	observedAt := time.Unix(1700000000, 0).UTC()
	observation := RepositoryObservation{RepositoryID: "repository-1", Reachable: true, ObservedAt: observedAt}
	if !observation.Recorded() || (RepositoryObservation{}).Recorded() {
		t.Fatal("Recorded does not distinguish a real observation from a zero one")
	}
	if observation.StaleAt(observedAt.Add(time.Minute), 15*time.Minute) {
		t.Fatal("fresh observation reported stale")
	}
	if !observation.StaleAt(observedAt.Add(15*time.Minute), 15*time.Minute) {
		t.Fatal("observation exactly at the bound reported fresh")
	}
	if !observation.StaleAt(observedAt.Add(time.Hour), 15*time.Minute) {
		t.Fatal("old observation reported fresh")
	}
	if observation.StaleAt(observedAt.Add(time.Hour), 0) {
		t.Fatal("a zero staleness bound must disable the rule rather than expire everything")
	}
	if !(RepositoryObservation{}).StaleAt(observedAt, time.Minute) {
		t.Fatal("an absent observation must always be stale")
	}
}

func TestShouldObserveRepository(t *testing.T) {
	locator := mustLocator(t, "O/N")
	other := mustLocator(t, "O/other")
	observedAt := time.Unix(1700000000, 0).UTC()
	fresh := RepositoryObservation{RepositoryID: "repository-1", Locator: locator, Reachable: true, ObservedAt: observedAt}

	if !ShouldObserveRepository(RepositoryObservation{}, false, locator, observedAt, DefaultObservationStaleAfter) {
		t.Fatal("absent observation must be submitted")
	}
	if ShouldObserveRepository(fresh, true, locator, observedAt.Add(time.Minute), DefaultObservationStaleAfter) {
		t.Fatal("fresh observation must not be resubmitted")
	}
	if !ShouldObserveRepository(fresh, true, locator, observedAt.Add(time.Hour), DefaultObservationStaleAfter) {
		t.Fatal("stale observation must be resubmitted")
	}
	if !ShouldObserveRepository(fresh, true, other, observedAt.Add(time.Minute), DefaultObservationStaleAfter) {
		t.Fatal("a changed locator must be resubmitted")
	}
}

func TestRepositoryExecutabilityIsMeasuredOrExplicitlyUnobserved(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	locator := mustLocator(t, "O/N")
	registered := Repository{ID: "repository-1", Locator: locator, Status: RepositoryRegistered, Version: 1}
	allow := EffectiveControlResult{Mode: ControlAllow}

	// No observation: the honest answer is "unobserved" with a reason, never
	// a plausible-looking executable/blocked verdict.
	got := RepositoryExecutabilityFrom(registered, allow, RepositoryObservation{}, false, now, DefaultObservationStaleAfter)
	if got.State != RepositoryUnobserved || got.Executable || got.Reason == "" {
		t.Fatalf("unobserved case = %+v", got)
	}

	full := RepositoryObservation{RepositoryID: "repository-1", Locator: locator, Reachable: true, CanPush: true, DefaultBranch: "main", ObservedAt: now}
	got = RepositoryExecutabilityFrom(registered, allow, full, true, now, DefaultObservationStaleAfter)
	if got.State != RepositoryExecutable || !got.Executable || got.Reason == "" || got.Stale {
		t.Fatalf("executable case = %+v", got)
	}

	unreachable := full
	unreachable.Reachable = false
	unreachable.Reason = "not found"
	got = RepositoryExecutabilityFrom(registered, allow, unreachable, true, now, DefaultObservationStaleAfter)
	if got.State != RepositoryBlocked || got.Executable || !strings.Contains(got.Reason, "not found") {
		t.Fatalf("unreachable case = %+v", got)
	}

	noPush := full
	noPush.CanPush = false
	if got = RepositoryExecutabilityFrom(registered, allow, noPush, true, now, DefaultObservationStaleAfter); got.State != RepositoryBlocked || got.Executable {
		t.Fatalf("no-push case = %+v", got)
	}

	noBranch := full
	noBranch.DefaultBranch = ""
	if got = RepositoryExecutabilityFrom(registered, allow, noBranch, true, now, DefaultObservationStaleAfter); got.State != RepositoryBlocked || got.Executable {
		t.Fatalf("no-branch case = %+v", got)
	}

	// A stale but otherwise complete observation is still reported, with the
	// staleness flagged rather than silently ignored.
	if got = RepositoryExecutabilityFrom(registered, allow, full, true, now.Add(time.Hour), DefaultObservationStaleAfter); !got.Stale || got.State != RepositoryExecutable {
		t.Fatalf("stale case = %+v", got)
	}

	// A Control Intent that denies work wins over any Observation, and is
	// itself a measured input.
	if got = RepositoryExecutabilityFrom(registered, EffectiveControlResult{Mode: ControlPauseClaim, Found: true, Revision: 3}, full, true, now, DefaultObservationStaleAfter); got.State != RepositoryBlocked || got.Executable {
		t.Fatalf("control-denied case = %+v", got)
	}

	retired := registered
	retired.Status = RepositoryRetired
	if got = RepositoryExecutabilityFrom(retired, allow, full, true, now, DefaultObservationStaleAfter); got.State != RepositoryStateRetired || got.Executable {
		t.Fatalf("retired case = %+v", got)
	}
}

// TestRepositoryCarriesNoAssigneeOwnerOrRunnerField asserts A7 structurally
// on the aggregate's own field set, read from the AST rather than by review:
// a Repository is Installation-owned, so it must carry no assignee, no owner
// identity as an authorization subject, no ACL, no permission and no runner
// identifier. RequestedBy is attribution only, which is why it is named as
// the sole identity-shaped field.
func TestRepositoryCarriesNoAssigneeOwnerOrRunnerField(t *testing.T) {
	fields := repositoryStructFields(t, "Repository")
	want := map[string]bool{"ID": true, "Locator": true, "Status": true, "Version": true, "RequestedBy": true}
	if len(fields) != len(want) {
		t.Fatalf("Repository fields = %v, want exactly %v", fields, want)
	}
	for _, name := range fields {
		if !want[name] {
			t.Fatalf("Repository carries unexpected field %q", name)
		}
	}
	forbidden := []string{"assignee", "acl", "permission", "runnerid", "ownerid", "member", "role", "token", "secret", "credential", "password", "apikey"}
	for _, name := range fields {
		lowered := strings.ToLower(strings.ReplaceAll(name, "_", ""))
		for _, bad := range forbidden {
			if strings.Contains(lowered, bad) {
				t.Fatalf("Repository carries a forbidden field %q (matched %q)", name, bad)
			}
		}
	}
	// Positive control: the same matcher must flag a field this aggregate
	// must never grow, so the scan above cannot pass vacuously.
	for _, planted := range []string{"AssigneeID", "RunnerID", "ACL", "PermissionSet", "AccessToken"} {
		lowered := strings.ToLower(strings.ReplaceAll(planted, "_", ""))
		matched := false
		for _, bad := range forbidden {
			if strings.Contains(lowered, bad) {
				matched = true
			}
		}
		if !matched {
			t.Fatalf("positive control %q was not matched by the forbidden-field list", planted)
		}
	}
	// The observation is a separate record and must equally carry no runner
	// identifier: an Observation is Installation-scoped evidence about a
	// Repository, not a claim about which machine made it.
	for _, name := range repositoryStructFields(t, "RepositoryObservation") {
		if strings.Contains(strings.ToLower(name), "runner") {
			t.Fatalf("RepositoryObservation carries a runner identifier field %q", name)
		}
	}
}

// repositoryStructFields reads the named struct type's field names out of
// repository.go's AST. It fails outright when the type is not found, so a
// rename cannot make the assertions above pass vacuously.
func repositoryStructFields(t *testing.T, typeName string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "repository.go", nil, 0)
	if err != nil {
		t.Fatalf("parse repository.go: %v", err)
	}
	var fields []string
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name == nil || spec.Name.Name != typeName {
			return true
		}
		structType, ok := spec.Type.(*ast.StructType)
		if !ok || structType.Fields == nil {
			return true
		}
		found = true
		for _, field := range structType.Fields.List {
			for _, name := range field.Names {
				fields = append(fields, name.Name)
			}
		}
		return false
	})
	if !found {
		t.Fatalf("struct type %q not found in repository.go", typeName)
	}
	if len(fields) == 0 {
		t.Fatalf("struct type %q has no fields; the AST walk is not finding them", typeName)
	}
	return fields
}

// --- V2-071: the Requirement-to-Repository link ----------------------------

func TestRequirementRepositoryLinkRefusesAnIncompleteAssociation(t *testing.T) {
	at := time.Date(2026, 8, 25, 6, 0, 0, 0, time.UTC)
	valid := RequirementRepositoryLink{RequirementID: "req-1", RepositoryID: "repo-1", AssignedAt: at, RequestedBy: RequestedBy{ActorType: ActorTypeOwner, Subject: "owner-1"}}
	if err := ValidateRequirementRepositoryLink(valid); err != nil {
		t.Fatalf("positive control: a complete link must validate, got %v", err)
	}
	if !valid.Recorded() {
		t.Fatal("positive control: a complete link must report itself recorded")
	}
	// Every refusal below is a separate assertion, so a single over-broad
	// check cannot stand in for four distinct invariants.
	cases := []struct {
		name  string
		value RequirementRepositoryLink
	}{
		{"empty requirement id", RequirementRepositoryLink{RepositoryID: "repo-1", AssignedAt: at}},
		{"blank requirement id", RequirementRepositoryLink{RequirementID: "   ", RepositoryID: "repo-1", AssignedAt: at}},
		{"empty repository id", RequirementRepositoryLink{RequirementID: "req-1", AssignedAt: at}},
		{"blank repository id", RequirementRepositoryLink{RequirementID: "req-1", RepositoryID: "\t", AssignedAt: at}},
		{"zero assigned_at", RequirementRepositoryLink{RequirementID: "req-1", RepositoryID: "repo-1"}},
		{"unknown actor type", RequirementRepositoryLink{RequirementID: "req-1", RepositoryID: "repo-1", AssignedAt: at, RequestedBy: RequestedBy{ActorType: ActorType("robot")}}},
	}
	for _, tc := range cases {
		if err := ValidateRequirementRepositoryLink(tc.value); err == nil {
			t.Fatalf("%s: expected a refusal, got none", tc.name)
		}
	}
	if err := ValidateRequirementRepositoryLink(RequirementRepositoryLink{RequirementID: "req-1", RepositoryID: "repo-1", AssignedAt: at}); err != nil {
		t.Fatalf("a link with no requested_by is still valid (attribution is optional), got %v", err)
	}
	if (RequirementRepositoryLink{}).Recorded() {
		t.Fatal("a zero link must not report itself recorded")
	}
	if !errors.Is(ValidateRequirementRepositoryLink(RequirementRepositoryLink{RepositoryID: "repo-1", AssignedAt: at}), ErrEmptyID) {
		t.Fatal("an empty requirement id must be refused as ErrEmptyID, the package's own opaque-id refusal")
	}
}

// TestRequirementRepositoryLinkAddsNoFieldToAnyExistingAggregate is the
// structural half of the same claim: the association exists without any of
// the M1 aggregates gaining a field, which is what makes model.go's
// byte-identity possible at all.
func TestRequirementRepositoryLinkAddsNoFieldToAnyExistingAggregate(t *testing.T) {
	for _, spec := range []struct{ file, typeName string }{
		{"model.go", "Requirement"}, {"model.go", "Increment"}, {"model.go", "Execution"},
		{"model.go", "Lease"}, {"control.go", "ControlIntent"}, {"control.go", "ControlTarget"},
	} {
		for _, field := range structFieldsInFile(t, spec.file, spec.typeName) {
			if strings.Contains(strings.ToLower(field), "repositorylink") {
				t.Fatalf("%s %s carries a link field %q; the association must stay a side record", spec.file, spec.typeName, field)
			}
		}
	}
	// Repository itself must not have grown a Requirement list either.
	for _, field := range structFieldsInFile(t, "repository.go", "Repository") {
		if strings.Contains(strings.ToLower(field), "requirement") {
			t.Fatalf("Repository carries %q; the association is Requirement-keyed, not a list on the aggregate", field)
		}
	}
	fields := structFieldsInFile(t, "repository.go", "RequirementRepositoryLink")
	want := []string{"RequirementID", "RepositoryID", "AssignedAt", "RequestedBy"}
	if len(fields) != len(want) {
		t.Fatalf("RequirementRepositoryLink fields = %v, want exactly %v", fields, want)
	}
	for i := range want {
		if fields[i] != want[i] {
			t.Fatalf("RequirementRepositoryLink fields = %v, want exactly %v", fields, want)
		}
	}
}

// structFieldsInFile is repositoryStructFields generalised over the file to
// parse, so the assertion above can read model.go's aggregates without
// modifying model.go or duplicating the walk a third time.
func structFieldsInFile(t *testing.T, fileName, typeName string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, fileName, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", fileName, err)
	}
	var fields []string
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name == nil || spec.Name.Name != typeName {
			return true
		}
		structType, ok := spec.Type.(*ast.StructType)
		if !ok || structType.Fields == nil {
			return true
		}
		found = true
		for _, field := range structType.Fields.List {
			for _, name := range field.Names {
				fields = append(fields, name.Name)
			}
		}
		return false
	})
	if !found {
		t.Fatalf("struct type %q not found in %s", typeName, fileName)
	}
	if len(fields) == 0 {
		t.Fatalf("struct type %q in %s has no fields; the AST walk is not finding them", typeName, fileName)
	}
	return fields
}

// --- V2-072: the publication intent and the publication Observation --------

func TestPublicationRefNameIsADeterministicFunctionOfTwoIdentifiers(t *testing.T) {
	// A19: one pure function is the only producer of a ref name, so the same
	// operation retried targets one ref and a second Execution gets its own.
	first, err := PublicationRefName("inc-1", "exe-1")
	if err != nil {
		t.Fatalf("PublicationRefName: %v", err)
	}
	again, err := PublicationRefName("inc-1", "exe-1")
	if err != nil {
		t.Fatalf("PublicationRefName (second call): %v", err)
	}
	if first != again {
		t.Fatalf("the ref name is not a function of its inputs: %q then %q", first, again)
	}
	if first != PublicationRefPrefix+"inc-1/exe-1" {
		t.Fatalf("ref name = %q", first)
	}
	second, err := PublicationRefName("inc-1", "exe-2")
	if err != nil {
		t.Fatalf("PublicationRefName (second execution): %v", err)
	}
	if second == first {
		t.Fatalf("a second Execution of the same Increment reused the first Execution's ref %q", first)
	}
	increment, execution, err := ParsePublicationRef(second)
	if err != nil {
		t.Fatalf("ParsePublicationRef(%q): %v", second, err)
	}
	if increment != "inc-1" || execution != "exe-2" {
		t.Fatalf("ParsePublicationRef(%q) = %q, %q", second, increment, execution)
	}
}

func TestPublicationRefRefusesAnythingOutsideTheReservedPrefix(t *testing.T) {
	for _, ref := range []string{
		"",
		"refs/heads/main",
		"refs/heads/v2",
		"refs/heads/agentic-loop",
		"refs/heads/agentic-loop/",
		"refs/heads/agentic-loop/inc-1",
		"refs/heads/agentic-loop/inc-1/exe-1/extra",
		"refs/heads/agentic-loop/../inc/exe",
		"refs/tags/agentic-loop/inc-1/exe-1",
		"agentic-loop/inc-1/exe-1",
	} {
		if _, _, err := ParsePublicationRef(ref); !errors.Is(err, ErrInvalidPublicationRef) {
			t.Fatalf("ParsePublicationRef(%q) = %v, want ErrInvalidPublicationRef", ref, err)
		}
	}
	for _, pair := range [][2]string{
		{"", "exe"}, {"inc", ""}, {"inc/extra", "exe"}, {"inc", "exe/extra"},
		{"-inc", "exe"}, {"inc", "-exe"}, {"..", "exe"}, {"inc", "."},
		{"inc name", "exe"}, {"inc", "exe\ttab"}, {"in..c", "exe"},
	} {
		if _, err := PublicationRefName(IncrementID(pair[0]), ExecutionID(pair[1])); !errors.Is(err, ErrInvalidPublicationRef) {
			t.Fatalf("PublicationRefName(%q, %q) = %v, want ErrInvalidPublicationRef", pair[0], pair[1], err)
		}
	}
}

func TestPublicationStateVocabularyIsClosedAndCannotSayCompleted(t *testing.T) {
	// A14 and A21(iii): the vocabulary itself is the guard. A reader cannot
	// conclude "the Requirement is done" from a record whose state set has no
	// value that means it.
	states := PublicationStates()
	if len(states) != 5 {
		t.Fatalf("the closed state set has %d values: %v", len(states), states)
	}
	seen := map[PublicationState]bool{}
	for _, state := range states {
		if seen[state] {
			t.Fatalf("state %q appears twice in the closed set", state)
		}
		seen[state] = true
		if !ValidPublicationState(state) {
			t.Fatalf("state %q is in the closed set but is not valid", state)
		}
		for _, forbidden := range []string{"complete", "resolv", "accept", "done", "finish", "success"} {
			if strings.Contains(strings.ToLower(string(state)), forbidden) {
				t.Fatalf("state %q contains %q: no publication state may mean the Requirement is finished", state, forbidden)
			}
		}
	}
	if !seen[PublicationUnobserved] {
		t.Fatal("unobserved is not a first-class state; nothing measured must be reportable as such")
	}
	if !seen[PublicationPublishedAndObserved] {
		t.Fatal("there is no terminal success state")
	}
	for _, unknown := range []PublicationState{"", "completed", "done", "accepted", "resolved", "published"} {
		if ValidPublicationState(unknown) {
			t.Fatalf("ValidPublicationState(%q) = true, want false", unknown)
		}
	}
}

func mustPublicationIntent(t *testing.T) PublicationIntent {
	t.Helper()
	ref, err := PublicationRefName("inc-1", "exe-1")
	if err != nil {
		t.Fatalf("PublicationRefName: %v", err)
	}
	return PublicationIntent{
		RepositoryID: "repo-1",
		Locator:      mustLocator(t, "https://github.com/Owner/Name.git"),
		Ref:          ref,
		BaseBranch:   "v2",
		BaseCommit:   strings.Repeat("a", 40),
		HeadCommit:   strings.Repeat("b", 40),
		HeadTree:     strings.Repeat("c", 40),
		ChangedPaths: 2,
	}
}

func TestValidatePublicationIntentRefusals(t *testing.T) {
	base := mustPublicationIntent(t)
	if err := ValidatePublicationIntent(base); err != nil {
		t.Fatalf("the well-formed intent was refused: %v", err)
	}
	if !base.Recorded() {
		t.Fatal("a well-formed intent reports itself unrecorded")
	}
	if (PublicationIntent{}).Recorded() {
		t.Fatal("a zero intent reports itself recorded")
	}
	mutate := map[string]func(*PublicationIntent){
		"no repository":       func(i *PublicationIntent) { i.RepositoryID = "" },
		"unnormalised":        func(i *PublicationIntent) { i.Locator.Owner = "Owner" },
		"ref outside prefix":  func(i *PublicationIntent) { i.Ref = "refs/heads/main" },
		"no base branch":      func(i *PublicationIntent) { i.BaseBranch = " " },
		"base not an object":  func(i *PublicationIntent) { i.BaseCommit = "not-an-object" },
		"head not an object":  func(i *PublicationIntent) { i.HeadCommit = "" },
		"tree not an object":  func(i *PublicationIntent) { i.HeadTree = strings.Repeat("z", 40) },
		"nothing to publish":  func(i *PublicationIntent) { i.HeadCommit = i.BaseCommit },
		"no changed path":     func(i *PublicationIntent) { i.ChangedPaths = 0 },
		"negative path count": func(i *PublicationIntent) { i.ChangedPaths = -1 },
	}
	for name, apply := range mutate {
		candidate := base
		apply(&candidate)
		if err := ValidatePublicationIntent(candidate); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestPublicationObservationIsWriteOnceShapedAndRefusesUnmeasuredSuccess(t *testing.T) {
	tree := strings.Repeat("c", 40)
	at := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	ref, err := PublicationRefName("inc-1", "exe-1")
	if err != nil {
		t.Fatalf("PublicationRefName: %v", err)
	}
	good := PublicationObservation{
		OperationID:     "op-1",
		RepositoryID:    "repo-1",
		Ref:             ref,
		PublishedCommit: strings.Repeat("d", 40),
		PublishedTree:   tree,
		LocalCommit:     strings.Repeat("b", 40),
		LocalTree:       tree,
		TreesAgree:      true,
		State:           PublicationPublishedAndObserved,
		Reason:          "the ref was created and all four content-addressed equalities held",
		ObservedAt:      at,
	}
	if err := ValidatePublicationObservation(good); err != nil {
		t.Fatalf("the well-formed Observation was refused: %v", err)
	}
	if !good.Recorded() {
		t.Fatal("a well-formed Observation reports itself unrecorded")
	}
	increment, err := good.IncrementID()
	if err != nil || increment != "inc-1" {
		t.Fatalf("IncrementID() = %q, %v", increment, err)
	}
	// The published and the local commit object name are recorded and are
	// deliberately allowed to differ: the forge constructs the commit object.
	if good.PublishedCommit == good.LocalCommit {
		t.Fatal("this fixture is meant to exercise a differing commit object name")
	}
	mutate := map[string]func(*PublicationObservation){
		"no operation":       func(o *PublicationObservation) { o.OperationID = "" },
		"no repository":      func(o *PublicationObservation) { o.RepositoryID = "" },
		"ref outside prefix": func(o *PublicationObservation) { o.Ref = "refs/heads/main" },
		"unknown state":      func(o *PublicationObservation) { o.State = "completed" },
		"empty state":        func(o *PublicationObservation) { o.State = "" },
		"no reason":          func(o *PublicationObservation) { o.Reason = " " },
		"no instant":         func(o *PublicationObservation) { o.ObservedAt = time.Time{} },
		"bad object name":    func(o *PublicationObservation) { o.PublishedTree = "xyz" },
		"success with no published object": func(o *PublicationObservation) {
			o.PublishedCommit = ""
		},
		"success with disagreeing trees": func(o *PublicationObservation) {
			o.PublishedTree = strings.Repeat("e", 40)
		},
		"success with the flag cleared": func(o *PublicationObservation) { o.TreesAgree = false },
	}
	for name, apply := range mutate {
		candidate := good
		apply(&candidate)
		if err := ValidatePublicationObservation(candidate); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	// An unobserved Observation is a first-class record and must carry no
	// published object name at all.
	unobserved := PublicationObservation{OperationID: "op-2", RepositoryID: "repo-1", Ref: ref, State: PublicationUnobserved, Reason: "no publication has been attempted for this operation yet", ObservedAt: at}
	if err := ValidatePublicationObservation(unobserved); err != nil {
		t.Fatalf("the unobserved Observation was refused: %v", err)
	}
	unobserved.PublishedTree = tree
	if err := ValidatePublicationObservation(unobserved); err == nil {
		t.Fatal("an unobserved Observation carrying a published tree was accepted")
	}
}

func TestPublicationRecordsAddNoFieldToAnyExistingAggregate(t *testing.T) {
	// A13: the two new records are additive. No existing aggregate grows a
	// publication field, and neither new record stores a URL.
	for _, spec := range []struct{ file, typeName string }{
		{"model.go", "Requirement"}, {"model.go", "Increment"}, {"model.go", "Execution"},
		{"model.go", "Lease"}, {"control.go", "ControlIntent"}, {"repository.go", "Repository"},
	} {
		for _, field := range structFieldsInFile(t, spec.file, spec.typeName) {
			lowered := strings.ToLower(field)
			if strings.Contains(lowered, "publication") || strings.Contains(lowered, "publish") {
				t.Fatalf("%s %s carries %q; a publication is its own keyed record", spec.file, spec.typeName, field)
			}
		}
	}
	for _, typeName := range []string{"PublicationIntent", "PublicationObservation"} {
		fields := structFieldsInFile(t, "repository.go", typeName)
		if len(fields) == 0 {
			t.Fatalf("%s has no fields", typeName)
		}
		for _, field := range fields {
			if strings.Contains(strings.ToLower(field), "url") {
				t.Fatalf("%s carries %q; a URL is never identity and can carry credential material", typeName, field)
			}
		}
	}
	intent := structFieldsInFile(t, "repository.go", "PublicationIntent")
	wantIntent := []string{"RepositoryID", "Locator", "Ref", "BaseBranch", "BaseCommit", "HeadCommit", "HeadTree", "ChangedPaths"}
	if strings.Join(intent, ",") != strings.Join(wantIntent, ",") {
		t.Fatalf("PublicationIntent fields = %v, want exactly %v", intent, wantIntent)
	}
	observation := structFieldsInFile(t, "repository.go", "PublicationObservation")
	wantObservation := []string{"OperationID", "RepositoryID", "Ref", "PublishedCommit", "PublishedTree", "LocalCommit", "LocalTree", "TreesAgree", "State", "Reason", "ObservedAt"}
	if strings.Join(observation, ",") != strings.Join(wantObservation, ",") {
		t.Fatalf("PublicationObservation fields = %v, want exactly %v", observation, wantObservation)
	}
}
