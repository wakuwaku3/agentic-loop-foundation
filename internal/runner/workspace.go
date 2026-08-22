package runner

import (
	"errors"
	"os"
	"path/filepath"
)

type Workspace struct{ root string }

func NewWorkspace(root string) (*Workspace, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, errors.New("workspace root must be absolute")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(realRoot)
	if err != nil || !info.IsDir() {
		return nil, errors.New("workspace root is not a directory")
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("workspace root permissions are broader than 0700")
	}
	return &Workspace{root: realRoot}, nil
}
func (w *Workspace) Path(executionID string) (string, error) {
	if executionID == "" || filepath.Base(executionID) != executionID || executionID == "." || executionID == ".." {
		return "", errors.New("invalid execution workspace id")
	}
	p := filepath.Join(w.root, executionID)
	rel, err := filepath.Rel(w.root, p)
	if err != nil || rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return "", errors.New("workspace path escapes root")
	}
	if info, err := os.Lstat(p); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("execution workspace may not be a symlink")
		}
		if !info.IsDir() {
			return "", errors.New("execution workspace is not a directory")
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return p, nil
}
func (w *Workspace) Create(executionID string) (string, error) {
	p, err := w.Path(executionID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(p, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(p)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("workspace directory failed isolation checks")
	}
	return p, nil
}
