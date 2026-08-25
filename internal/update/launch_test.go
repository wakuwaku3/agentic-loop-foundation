package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// overwrite replaces the bytes of an installed, deliberately read-only file.
// It is how a test puts the launcher in the situation install-time
// verification cannot see: correct bytes at install, different bytes now.
func overwrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
}

// recordingStarter collects every ChildSpec the launcher asked for.
type recordingStarter struct {
	specs []ChildSpec
	probe int
}

func (r *recordingStarter) start(_ context.Context, spec ChildSpec) (ChildResult, error) {
	r.specs = append(r.specs, spec)
	return ChildResult{ExitCode: 0}, nil
}

func (r *recordingStarter) ok(context.Context) error {
	r.probe++
	return nil
}

// TestLaunchReVerifiesOnDiskBytesBeforeEveryExec is the journey behind the
// only claim this task may make about restarting: the launcher re-execs a
// re-verified binary after the child exits. That is NOT M8 completion
// condition 2, which names breaking a real Preview Control Plane and Runner
// and recovering from the Stable launcher; that exercise is preview-local
// and belongs to V2-035.
func TestLaunchReVerifiesOnDiskBytesBeforeEveryExec(t *testing.T) {
	f := newFixture(t)
	state := NewState(1)
	f.install(t, state, "1.0.0", []byte("runner-one"), at(0))
	f.promote(t, state, "1.0.0", at(1))

	spy := &recordingStarter{}
	launcher := Launcher{Root: f.root, CanonicalSchema: 1, Anchors: f.resolver(), Probe: spy.ok, Start: spy.start, Args: []string{"--role", "runner"}}

	outcome, err := launcher.Run(context.Background(), "stable", 3)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Version != "1.0.0" || outcome.Launches != 3 || outcome.Verifications != 3 || len(spy.specs) != 3 {
		t.Fatalf("outcome = %+v, specs = %d", outcome, len(spy.specs))
	}
	t.Logf("verdict: %d launches, %d re-verifications, one per exec", outcome.Launches, outcome.Verifications)
	for i, spec := range spy.specs {
		if !spec.NewProcessGroup {
			t.Fatalf("spec %d was not asked for its own process group", i)
		}
		if spec.Path != filepath.Join(VersionDir(f.root, "1.0.0"), "runner") || spec.Attempt != i+1 {
			t.Fatalf("spec %d = %+v", i, spec)
		}
	}

	// Now the situation install-time verification is blind to: the bytes on
	// disk change after a successful install and three successful launches.
	before := len(spy.specs)
	overwrite(t, filepath.Join(VersionDir(f.root, "1.0.0"), "runner"), []byte("runner-tampered"))
	if _, err := launcher.Launch(context.Background(), "stable"); !errors.Is(err, ErrLaunchRefused) {
		t.Fatalf("tampered binary launched: err = %v", err)
	} else {
		t.Logf("verdict tampered-binary: %v", err)
	}
	if len(spy.specs) != before {
		t.Fatal("a child was started from tampered bytes")
	}

	// A tampered manifest fails the signature, not merely the digest.
	f2 := newFixture(t)
	state2 := NewState(1)
	f2.install(t, state2, "1.0.0", []byte("runner-one"), at(0))
	f2.promote(t, state2, "1.0.0", at(1))
	spy2 := &recordingStarter{}
	launcher2 := Launcher{Root: f2.root, CanonicalSchema: 1, Anchors: f2.resolver(), Probe: spy2.ok, Start: spy2.start}
	if _, err := launcher2.Launch(context.Background(), "stable"); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(VersionDir(f2.root, "1.0.0"), "manifest.json")
	original, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	overwrite(t, manifestPath, append(append([]byte(nil), original[:len(original)-1]...), []byte(`,"roles":["runner"]}`)...))
	if _, err := launcher2.Launch(context.Background(), "stable"); !errors.Is(err, ErrLaunchRefused) {
		t.Fatalf("tampered manifest launched: err = %v", err)
	} else {
		t.Logf("verdict tampered-manifest: %v", err)
	}
	// A missing signature is a refusal, not a fall back to a digest-only check.
	overwrite(t, manifestPath, original)
	if err := os.Remove(filepath.Join(VersionDir(f2.root, "1.0.0"), "signature")); err != nil {
		t.Fatal(err)
	}
	if _, err := launcher2.Launch(context.Background(), "stable"); !errors.Is(err, ErrLaunchRefused) {
		t.Fatalf("missing signature launched: err = %v", err)
	} else {
		t.Logf("verdict missing-signature: %v", err)
	}
	if len(spy2.specs) != 1 {
		t.Fatalf("children started = %d, want only the one from untampered bytes", len(spy2.specs))
	}
}

// TestLaunchRefusesRatherThanDegrading walks the launcher's other
// preconditions. Each one refuses in the shape V2-046 established for the
// Runner side: a reported failure with no child started, never a degraded
// run.
func TestLaunchRefusesRatherThanDegrading(t *testing.T) {
	f := newFixture(t)
	state := NewState(1)
	f.install(t, state, "1.0.0", []byte("runner-one"), at(0))

	probeFailed := errors.New("rootless user+mount namespace confinement is unavailable")
	cases := []struct {
		name     string
		route    bool
		launcher func(*recordingStarter) Launcher
		channel  string
	}{
		{
			name:  "an-unrouted-channel",
			route: false,
			launcher: func(s *recordingStarter) Launcher {
				return Launcher{Root: f.root, CanonicalSchema: 1, Anchors: f.resolver(), Probe: s.ok, Start: s.start}
			},
			channel: "stable",
		},
		{
			name:  "an-unknown-channel",
			route: true,
			launcher: func(s *recordingStarter) Launcher {
				return Launcher{Root: f.root, CanonicalSchema: 1, Anchors: f.resolver(), Probe: s.ok, Start: s.start}
			},
			channel: "nightly",
		},
		{
			name:  "a-failed-confinement-probe",
			route: true,
			launcher: func(s *recordingStarter) Launcher {
				return Launcher{Root: f.root, CanonicalSchema: 1, Anchors: f.resolver(), Probe: func(context.Context) error { return probeFailed }, Start: s.start}
			},
			channel: "stable",
		},
		{
			name:  "no-confinement-probe-at-all",
			route: true,
			launcher: func(s *recordingStarter) Launcher {
				return Launcher{Root: f.root, CanonicalSchema: 1, Anchors: f.resolver(), Start: s.start}
			},
			channel: "stable",
		},
		{
			name:  "no-process-starter",
			route: true,
			launcher: func(s *recordingStarter) Launcher {
				return Launcher{Root: f.root, CanonicalSchema: 1, Anchors: f.resolver(), Probe: s.ok}
			},
			channel: "stable",
		},
		{
			name:  "a-canonical-schema-outside-the-routed-version-interval",
			route: true,
			launcher: func(s *recordingStarter) Launcher {
				return Launcher{Root: f.root, CanonicalSchema: 5, Anchors: f.resolver(), Probe: s.ok, Start: s.start}
			},
			channel: "stable",
		},
		{
			name:  "a-non-positive-canonical-schema",
			route: true,
			launcher: func(s *recordingStarter) Launcher {
				return Launcher{Root: f.root, CanonicalSchema: 0, Anchors: f.resolver(), Probe: s.ok, Start: s.start}
			},
			channel: "stable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			link := filepath.Join(f.root, "stable")
			_ = os.Remove(link)
			if tc.route {
				if err := os.Symlink(filepath.Join("versions", "1.0.0"), link); err != nil {
					t.Fatal(err)
				}
			}
			spy := &recordingStarter{}
			_, err := tc.launcher(spy).Launch(context.Background(), tc.channel)
			if err == nil {
				t.Fatal("accepted")
			}
			if len(spy.specs) != 0 {
				t.Fatal("a child was started by a refused launch")
			}
			t.Logf("verdict %s: %v", tc.name, err)
		})
	}

	t.Run("a-channel-pointing-outside-versions", func(t *testing.T) {
		link := filepath.Join(f.root, "preview")
		_ = os.Remove(link)
		if err := os.Symlink(filepath.Join("..", "elsewhere", "1.0.0"), link); err != nil {
			t.Fatal(err)
		}
		spy := &recordingStarter{}
		launcher := Launcher{Root: f.root, CanonicalSchema: 1, Anchors: f.resolver(), Probe: spy.ok, Start: spy.start}
		if _, err := launcher.Launch(context.Background(), "preview"); err == nil {
			t.Fatal("accepted")
		} else {
			t.Logf("verdict: %v", err)
		}
		if len(spy.specs) != 0 {
			t.Fatal("a child was started by a refused launch")
		}
	})
}
