package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// NamespaceConfinement runs a child inside a rootless (unprivileged) Linux
// user namespace plus mount namespace so that a write outside Workspace
// fails at the kernel/VFS layer -- not because the child chooses to obey a
// convention, but because the write() syscall itself returns EROFS.
//
// It needs no capability grant and no setuid helper: "unshare --user
// --map-root-user --mount" maps the caller's own uid to root inside a
// brand-new user namespace, which is sufficient (since Linux 3.8, see
// user_namespaces(7)) to also create a mount namespace and perform bind
// mounts and remounts within it, entirely inside that namespace's own view.
//
// The mechanism, once the namespace exists:
//
//  1. The top-level directory under "/" that contains Workspace (e.g. "/tmp"
//     if Workspace is "/tmp/x/y/workspace") is bind-mounted onto itself and
//     then remounted read-only. A bind mount is required first because an
//     unprivileged mount namespace cannot remount a mount it does not own
//     (mounting the exact same path onto itself, first, makes the mount
//     namespace the owner of a *new* mount object at that path); the two
//     steps cannot be collapsed into one "mount --bind -o ro" because the
//     kernel's bind-mount syscall ignores every flag except MS_BIND on its
//     first call.
//  2. Workspace itself is then bind-mounted onto itself and explicitly
//     remounted read-write, in that order, strictly *after* step 1. Doing
//     it after (not before) matters for two independent reasons: a mount
//     created at a path before an ancestor directory is bind-mounted gets
//     shadowed -- invisible -- once that ancestor becomes its own mount
//     object, and a bind mount created while its source is already
//     underneath a read-only mount inherits that read-only flag, so the
//     explicit "remount,bind,rw" is required even though Workspace was
//     never itself made read-only.
//
// Because step 1's remount is a property of one mount as a whole, every
// path under the sealed ancestor becomes read-only in one operation --
// no need to walk the tree -- except for Workspace's own, separately
// pinned, mount.
//
// What this does NOT prove or provide (see docs/operations/runner-local.md
// for the full list): the namespace's own root ("/") cannot itself be
// bind-mounted by an unprivileged caller (the kernel's do_loopback rejects
// binding a mount namespace's root dentry onto any target -- EINVAL,
// verified empirically, not merely inferred), so directories outside the
// sealed top-level ancestor are left exactly as the host's ordinary DAC
// permissions already left them (on any standard deployment where the
// runner's own uid is not the real root, everything above that top-level
// ancestor was already unwritable to it, so this is not an exploitable
// gap); nothing here confines network access, PIDs, or interference with
// other processes owned by the same uid outside the namespace.
type NamespaceConfinement struct {
	// Workspace is the absolute, already-existing directory that must stay
	// writable inside the namespace. It, and only it, keeps read-write.
	Workspace string
}

// ErrNamespaceUnsupported reports that this kernel/environment cannot
// create or use an unprivileged user+mount namespace. Callers (in
// particular ProcessSupervisor.Run) must treat it as a hard, reported
// failure: there is no silent fallback to running the child unconfined.
var ErrNamespaceUnsupported = errors.New("runner: rootless user+mount namespace confinement is unavailable on this kernel/environment")

// resolveTool finds an absolute path for an external command this package
// depends on. It prefers the caller's own PATH (so a devbox/nix-provided
// util-linux build is used when present) and falls back to the
// conventional system locations, so confinement still works when PATH has
// been stripped (e.g. "devbox run --pure") but the distribution's own
// util-linux is installed at its usual place.
func resolveTool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, dir := range []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return name
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// topLevelAncestor returns the first path component under "/" that
// contains absPath -- e.g. "/tmp" for "/tmp/a/b" -- or absPath itself if
// absPath is already a direct child of "/".
func topLevelAncestor(absPath string) string {
	clean := filepath.Clean(absPath)
	rel := strings.TrimPrefix(clean, "/")
	if rel == "" {
		return "/"
	}
	parts := strings.SplitN(rel, "/", 2)
	return "/" + parts[0]
}

// runUnshared runs script (a POSIX shell script body) inside a fresh
// rootless user+mount namespace and returns a wrapped ErrNamespaceUnsupported
// (with the command's own stderr folded in) on failure.
func runUnshared(ctx context.Context, script string, extraArgv ...string) *exec.Cmd {
	argv := []string{resolveTool("unshare"), "--user", "--map-root-user", "--mount", "--", resolveTool("sh"), "-c", script, "confine"}
	argv = append(argv, extraArgv...)
	return exec.CommandContext(ctx, argv[0], argv[1:]...)
}

// Probe reports whether unprivileged user+mount namespace confinement
// actually works in this environment. It is a *functional* check, not a
// version/capability guess: it creates a throwaway directory, and inside a
// fresh namespace performs exactly the operations Wrap depends on -- a
// self bind mount, a bind+ro remount, and (as an independent capability
// check matching the operations this environment was measured to support)
// a tmpfs mount -- before the namespace is torn down. Probe touches
// nothing under Workspace and never runs the caller's argv.
func (NamespaceConfinement) Probe(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "runner-confine-probe-*")
	if err != nil {
		return fmt.Errorf("%w: creating probe directory: %v", ErrNamespaceUnsupported, err)
	}
	defer os.RemoveAll(dir)
	bindDir := filepath.Join(dir, "bind")
	tmpfsDir := filepath.Join(dir, "tmpfs")
	if err := os.MkdirAll(bindDir, 0700); err != nil {
		return fmt.Errorf("%w: %v", ErrNamespaceUnsupported, err)
	}
	if err := os.MkdirAll(tmpfsDir, 0700); err != nil {
		return fmt.Errorf("%w: %v", ErrNamespaceUnsupported, err)
	}
	script := fmt.Sprintf(`set -e
mount --bind %[1]s %[1]s
mount -o remount,bind,ro %[1]s
mount -t tmpfs tmpfs %[2]s
`, shQuote(bindDir), shQuote(tmpfsDir))
	cmd := runUnshared(ctx, script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%w: %s", ErrNamespaceUnsupported, msg)
	}
	return nil
}

// wrap rewrites argv into a command line that runs it inside a rootless
// user+mount namespace with the confinement mounts from the type doc
// comment applied first. It performs no I/O and does not itself check
// whether namespaces are usable -- callers must call Probe first.
func (c NamespaceConfinement) wrap(argv []string) ([]string, error) {
	ws := filepath.Clean(c.Workspace)
	if ws == "" || !filepath.IsAbs(ws) {
		return nil, errors.New("runner: confinement workspace must be an absolute path")
	}
	var script strings.Builder
	script.WriteString("set -e\n")
	if ancestor := topLevelAncestor(ws); ancestor != ws {
		fmt.Fprintf(&script, "mount --bind %[1]s %[1]s\n", shQuote(ancestor))
		fmt.Fprintf(&script, "mount -o remount,bind,ro %[1]s\n", shQuote(ancestor))
	}
	fmt.Fprintf(&script, "mount --bind %[1]s %[1]s\n", shQuote(ws))
	fmt.Fprintf(&script, "mount -o remount,bind,rw %[1]s\n", shQuote(ws))
	script.WriteString(`exec "$@"` + "\n")
	wrapped := []string{resolveTool("unshare"), "--user", "--map-root-user", "--mount", "--", resolveTool("sh"), "-c", script.String(), "confine"}
	return append(wrapped, argv...), nil
}
