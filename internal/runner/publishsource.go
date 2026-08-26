package runner

// The publication source (V2-072). It answers one question: what exactly did
// the verified commit change, and what are the bytes of each changed path?
//
// Three properties are structural rather than promised:
//
//  1. What gets published is derived from the VERIFIED COMMIT in the working
//     copy, never from the caller's ChangeSet (dp-v2-072 d4). A ChangeSet is
//     what a caller intended; the committed tree is what VerifyIntegrity
//     actually measured, and publishing the former would mean the reviewable
//     artefact was never the verified one.
//  2. It adds NO git subcommand. diff, ls-files, cat-file and rev-parse are
//     already in the fourteen-entry allowlist in sourcecontrol.go, so the
//     strongest property V2-071 established -- that no push argv is
//     constructible, because the allowlist itself refuses it -- survives this
//     file untouched. internal/runner/git.go and sourcecontrol.go are
//     byte-identical: these methods hang off the same GitSourceControl type
//     from here.
//  3. Every child runs confined exactly as V2-071's do, through the same
//     private run helper, and each carries -C with the absolute working copy
//     root. No process output leaves this file except as the bounded values
//     below.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The two file modes a blob upload can represent. A symlink (120000) and a
// submodule gitlink (160000) are not blobs and are refused by name, before any
// forge call could exist.
const (
	publicationModeFile       = "100644"
	publicationModeExecutable = "100755"
	publicationModeSymlink    = "120000"
	publicationModeGitlink    = "160000"
)

var (
	// ErrPublicationChangeSetEmpty refuses a publication whose changed-path
	// count is zero. An empty change is not a reviewable branch, and creating
	// objects for it would leave irreversible residue for nothing.
	ErrPublicationChangeSetEmpty = errors.New("publishsource: the verified commit changes no path, so there is nothing reviewable to publish")
	// ErrPublicationModeUnrepresentable refuses a staged mode that is not
	// 100644 or 100755. A symlink at 120000 and a submodule gitlink at 160000
	// are the two cases this covers: neither can be expressed as a blob
	// upload, so publishing the change would produce a tree that quietly
	// differs from the verified one.
	ErrPublicationModeUnrepresentable = errors.New("publishsource: the staged mode cannot be represented as a blob upload")
	// ErrPublicationDeletionUnrepresentable refuses a change that would
	// require deleting a path. The change port carries whole content and has
	// no delete form, so a deletion cannot arise from it; refusing it
	// explicitly is what stops a silent divergence.
	ErrPublicationDeletionUnrepresentable = errors.New("publishsource: a publication that would delete a path is not representable")
	// ErrPublicationContentUnreadable refuses content whose measured size and
	// read length disagree, or which exceeds the adapter's hard output cap.
	// Its message carries none of the child's bytes.
	ErrPublicationContentUnreadable = errors.New("publishsource: the blob content could not be read whole within the bounded output")
)

// PublicationFile is one changed path of a publication, in the exact shape a
// tree entry needs. Content is base64 so a binary file cannot corrupt a
// request body, and Object is the blob object name the LOCAL repository
// reports -- which is what the forge's returned blob name is later required to
// equal.
type PublicationFile struct {
	Path    string
	Mode    string
	Object  string
	Content string
}

// PublicationPayload is everything derived from one verified commit. BaseTree
// is derived locally rather than read back from the forge: the base commit is
// the same object on both sides, because the working copy was cloned from it,
// so its tree is a content address that needs no external read (dp-v2-072 d5).
type PublicationPayload struct {
	BaseCommit string
	BaseTree   string
	HeadCommit string
	HeadTree   string
	Files      []PublicationFile
}

// PublicationSource is the narrow second port this task adds. It is one method
// wide on purpose: the only caller is the forge publisher, and every stage of
// the derivation is worthless on its own.
//
// Any fake implementation of this interface must live in a _test.go file, for
// the same reason SourceControl states: internal/runner is the only package
// permitted to start a child process.
type PublicationSource interface {
	PublicationPayload(ctx context.Context, working WorkingCopy, base, head string) (PublicationPayload, error)
}

// stagedEntry is the parse of one `ls-files --stage` line. The line's shape is
// "<mode> <object> <stage>\t<path>".
type stagedEntry struct {
	mode   string
	object string
	path   string
}

func parseStagedEntry(line string) (stagedEntry, bool) {
	meta, path, found := strings.Cut(line, "\t")
	if !found || path == "" {
		return stagedEntry{}, false
	}
	fields := strings.Fields(meta)
	if len(fields) != 3 {
		return stagedEntry{}, false
	}
	return stagedEntry{mode: fields[0], object: fields[1], path: path}, true
}

// PublicationPayload derives the payload from the verified commit using only
// already-allowlisted subcommands: diff --name-only for the changed paths,
// ls-files --stage for each path's mode and blob object name, cat-file -s and
// cat-file blob for its bytes, and rev-parse for the two tree object names.
func (g GitSourceControl) PublicationPayload(ctx context.Context, working WorkingCopy, base, head string) (PublicationPayload, error) {
	if !working.Recorded() {
		return PublicationPayload{}, fmt.Errorf("%w: no working copy", ErrWorkingCopyRequestInvalid)
	}
	workspace, root, err := g.workingCopyRoot(working.ExecutionID)
	if err != nil {
		return PublicationPayload{}, err
	}
	if root != working.Root || workspace != working.Workspace {
		return PublicationPayload{}, fmt.Errorf("%w: the working copy root is not this Execution's own", ErrWorkingCopyPathEscapes)
	}
	if len(base) != 40 && len(base) != 64 {
		return PublicationPayload{}, fmt.Errorf("%w: base object name", ErrGitOutputUnreadable)
	}
	if len(head) != 40 && len(head) != 64 {
		return PublicationPayload{}, fmt.Errorf("%w: head object name", ErrGitOutputUnreadable)
	}

	out := PublicationPayload{BaseCommit: base, HeadCommit: head}
	if out.BaseTree, err = g.revParse(ctx, workspace, root, base+"^{tree}"); err != nil {
		return PublicationPayload{}, err
	}
	if out.HeadTree, err = g.revParse(ctx, workspace, root, head+"^{tree}"); err != nil {
		return PublicationPayload{}, err
	}

	argv, err := g.buildGitArgv([]string{"-C", root}, "diff", "--name-only", base, head)
	if err != nil {
		return PublicationPayload{}, err
	}
	raw, err := g.run(ctx, workspace, argv)
	if err != nil {
		return PublicationPayload{}, err
	}
	paths := nonEmptyLines(raw)
	if len(paths) == 0 {
		return PublicationPayload{}, ErrPublicationChangeSetEmpty
	}

	out.Files = make([]PublicationFile, 0, len(paths))
	for _, path := range paths {
		if _, e := changePath(root, path); e != nil {
			return PublicationPayload{}, e
		}
		if argv, err = g.buildGitArgv([]string{"-C", root}, "ls-files", "--stage", "--", path); err != nil {
			return PublicationPayload{}, err
		}
		if raw, err = g.run(ctx, workspace, argv); err != nil {
			return PublicationPayload{}, err
		}
		lines := nonEmptyLines(raw)
		if len(lines) == 0 {
			// The path changed between base and head but is no longer in the
			// index: the change is a deletion, which no blob upload can
			// express.
			return PublicationPayload{}, fmt.Errorf("%w: one path is absent from the index", ErrPublicationDeletionUnrepresentable)
		}
		if len(lines) != 1 {
			return PublicationPayload{}, fmt.Errorf("%w: one path reported %d index entries", ErrGitOutputUnreadable, len(lines))
		}
		entry, ok := parseStagedEntry(lines[0])
		if !ok || entry.path != path {
			return PublicationPayload{}, fmt.Errorf("%w: index entry", ErrGitOutputUnreadable)
		}
		if entry.mode != publicationModeFile && entry.mode != publicationModeExecutable {
			return PublicationPayload{}, fmt.Errorf("%w: mode %s (a symlink is %s and a submodule gitlink is %s)", ErrPublicationModeUnrepresentable, entry.mode, publicationModeSymlink, publicationModeGitlink)
		}
		if len(entry.object) != 40 && len(entry.object) != 64 {
			return PublicationPayload{}, fmt.Errorf("%w: blob object name", ErrGitOutputUnreadable)
		}
		content, e := g.readBlob(ctx, workspace, root, entry.object)
		if e != nil {
			return PublicationPayload{}, e
		}
		out.Files = append(out.Files, PublicationFile{Path: path, Mode: entry.mode, Object: entry.object, Content: content})
	}
	return out, nil
}

// readBlob reads one blob's bytes and returns them base64-encoded. The size is
// measured first with cat-file -s so a read the bounded output cap truncated is
// detected rather than silently published: a truncated blob would upload as a
// different object and the blob equality would then fail on the forge, after
// an irreversible object had already been created.
func (g GitSourceControl) readBlob(ctx context.Context, workspace, root, object string) (string, error) {
	argv, err := g.buildGitArgv([]string{"-C", root}, "cat-file", "-s", object)
	if err != nil {
		return "", err
	}
	raw, err := g.run(ctx, workspace, argv)
	if err != nil {
		return "", err
	}
	size, err := strconv.Atoi(firstLine(raw))
	if err != nil || size < 0 {
		return "", fmt.Errorf("%w: blob size", ErrGitOutputUnreadable)
	}
	if size > maxGitOutputBytes {
		return "", fmt.Errorf("%w: %d bytes exceeds the bounded output", ErrPublicationContentUnreadable, size)
	}
	if argv, err = g.buildGitArgv([]string{"-C", root}, "cat-file", "blob", object); err != nil {
		return "", err
	}
	if raw, err = g.run(ctx, workspace, argv); err != nil {
		return "", err
	}
	if len(raw) != size {
		return "", fmt.Errorf("%w: read %d of %d bytes", ErrPublicationContentUnreadable, len(raw), size)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}
