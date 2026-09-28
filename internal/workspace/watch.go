package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/remoteoss/dexter/internal/parser"
)

// WatchCallbacks translates platform events into workspace reconciliation work.
type WatchCallbacks struct {
	PathChanged     func(string)
	FullReconcile   func()
	CoverageChanged func(bool)
}

type watchBackend interface {
	Close() error
	Degraded() bool
}

// Watcher owns one recursive change source for a workspace daemon.
type Watcher struct {
	backend watchBackend
	kind    string
}

// Watch starts the best recursive watcher available on this platform.
func Watch(root string, callbacks WatchCallbacks) (*Watcher, error) {
	backend, kind, err := startPlatformWatcher(root, callbacks)
	if err != nil {
		return nil, err
	}
	return &Watcher{backend: backend, kind: kind}, nil
}

func (w *Watcher) Close() error { return w.backend.Close() }

func (w *Watcher) Degraded() bool { return w.backend.Degraded() }

func (w *Watcher) Kind() string { return w.kind }

func skipWatchDir(name string) bool {
	switch name {
	case "_build", ".git", "node_modules", "deps", ".dexter":
		return true
	}
	return false
}

func ignoredWatchPath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if skipWatchDir(part) {
			return true
		}
	}
	return false
}

// inNestedWorktree reports whether dir is, or lies inside, a linked git worktree
// nested below root. The index leaves those trees out, so their directories need
// no watch and creating one needs no reconcile.
func inNestedWorktree(root, dir string) bool {
	return dir != root && (parser.IsLinkedWorktree(dir) || parser.InLinkedWorktree(root, dir))
}

// worktreeTops is the set of nested worktree tops that a watcher knows. With it,
// a watcher drops events from inside a worktree with map lookups only, and it
// can find a top whose .git file went away.
type worktreeTops struct {
	mu   sync.Mutex
	tops map[string]struct{}
}

func (s *worktreeTops) add(dir string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tops == nil {
		s.tops = make(map[string]struct{})
	}
	if _, ok := s.tops[dir]; ok {
		return false
	}
	s.tops[dir] = struct{}{}
	return true
}

func (s *worktreeTops) remove(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tops, dir)
}

func (s *worktreeTops) has(dir string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.tops[dir]
	return ok
}

func (s *worktreeTops) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tops := make([]string, 0, len(s.tops))
	for dir := range s.tops {
		tops = append(tops, dir)
	}
	return tops
}

// under reports whether path lies below a known top, not counting the top.
func (s *worktreeTops) under(root, path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tops) == 0 {
		return false
	}
	for dir := filepath.Dir(path); len(dir) > len(root); dir = filepath.Dir(dir) {
		if _, ok := s.tops[dir]; ok {
			return true
		}
	}
	return false
}

// nestedWorktreeTops lists the linked worktrees of root's repository that are
// checked out below root. It reads git's own records: each admin directory in
// <common dir>/worktrees/ has a gitdir file that names the worktree's .git
// file. Paths come back in the spelling of root; aliases are other spellings of
// root, such as its symlink-resolved path, that git may have recorded instead.
func nestedWorktreeTops(root string, aliases ...string) []string {
	gitDir, ok := parser.GitDir(root)
	if !ok {
		return nil
	}
	common := gitDir
	if b, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		common = resolveFrom(gitDir, strings.TrimSpace(string(b)))
	}
	admins, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if err != nil {
		return nil
	}
	var tops []string
	for _, admin := range admins {
		adminDir := filepath.Join(common, "worktrees", admin.Name())
		b, err := os.ReadFile(filepath.Join(adminDir, "gitdir"))
		if err != nil {
			continue
		}
		top := filepath.Dir(resolveFrom(adminDir, strings.TrimSpace(string(b))))
		for _, spelling := range append([]string{root}, aliases...) {
			rel, err := filepath.Rel(spelling, top)
			if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				tops = append(tops, filepath.Join(root, rel))
				break
			}
		}
	}
	return tops
}

func resolveFrom(dir, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return filepath.Clean(path)
}
