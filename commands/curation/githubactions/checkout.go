package githubactions

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GithubSHAEnvVar names the commit the running job was triggered for.
const GithubSHAEnvVar = "GITHUB_SHA"

// WorkspaceCommit returns the commit checked out in dir, read from its .git files so no git binary
// is needed: HEAD, the loose ref it names, or packed-refs. A .git file ("gitdir: <path>") is
// followed.
func WorkspaceCommit(dir string) (string, error) {
	gitDir := filepath.Join(dir, ".git")
	info, err := os.Stat(gitDir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		content, err := os.ReadFile(gitDir)
		if err != nil {
			return "", err
		}
		target, ok := strings.CutPrefix(strings.TrimSpace(string(content)), "gitdir:")
		if !ok {
			return "", fmt.Errorf("%q is neither a git directory nor a gitdir file", gitDir)
		}
		gitDir = strings.TrimSpace(target)
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(dir, gitDir)
		}
	}
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", err
	}
	ref, symbolic := strings.CutPrefix(strings.TrimSpace(string(head)), "ref:")
	if !symbolic {
		return commitID(ref)
	}
	ref = strings.TrimSpace(ref)
	if loose, err := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(ref))); err == nil {
		return commitID(string(loose))
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	packed, err := os.ReadFile(filepath.Join(gitDir, "packed-refs"))
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", ref, err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(packed))
	for scanner.Scan() {
		if id, name, ok := strings.Cut(scanner.Text(), " "); ok && name == ref {
			return commitID(id)
		}
	}
	return "", fmt.Errorf("%s is neither a loose nor a packed ref in %q", ref, gitDir)
}

func commitID(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !isFullObjectID(s) {
		return "", fmt.Errorf("%q is not a commit ID", s)
	}
	return s, nil
}
