package main

// Git isolation. The agent runs without permission prompts, so it must never
// write to the host repo's git metadata: a hook, a config key such as
// core.fsmonitor, or a `commondir` file there would make git on the host run
// the agent's code. So:
//
//   - The host's .git is mounted read-only at /mnt/host-git, used only to
//     seed the container's own repo.
//   - /home/agent/workspace/.git is a podman volume holding that repo. It
//     hides the host .git, so the files are shared live but git metadata is
//     not.
//   - Commits come back to the host through `podagent sync`: a git bundle
//     (plain data) fetched with hooks and fsmonitor disabled.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const hostGitMount = "/mnt/host-git"

// hostGitSafety stops podagent's own host-side git calls from running hooks
// or an fsmonitor command.
var hostGitSafety = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}

func gitOut(dir string, args ...string) (string, error) {
	full := append(append([]string{"-C", dir}, hostGitSafety...), args...)
	cmd := exec.Command("git", full...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func gitVolume(container string) string { return container + "-git" }

// initRepoScript seeds the container's repo from the read-only host .git.
// $1 branch, $2 "shared" or "fresh", $3 origin URL (may be empty).
const initRepoScript = `set -eu
cd /home/agent/workspace
git init -q -b "$1"
git -c safe.directory='*' fetch -q --no-tags ` + hostGitMount + ` \
  "+$4:refs/podagent/start" \
  '+refs/heads/*:refs/remotes/host/*' \
  '+refs/remotes/origin/*:refs/remotes/origin/*'
git update-ref "refs/heads/$1" refs/podagent/start
git update-ref -d refs/podagent/start
if [ -n "$3" ]; then git remote add origin "$3"; fi
if [ "$2" = shared ]; then git reset -q; else git reset -q --hard; fi
`

// initContainerRepo creates the container's repo on branch s.GitBranch,
// starting at the host commit startRef. In shared mode the working files are
// already the host's, so only the index is reset; otherwise the branch is
// checked out into the empty workspace.
func (s *Session) initContainerRepo() error {
	startRef := "refs/heads/" + s.GitBranch
	if _, err := gitOut(s.Repo, "rev-parse", "--verify", "--quiet", startRef); err != nil {
		// New branch: start it from --base via a temporary host ref.
		sha, err := gitOut(s.Repo, "rev-parse", "--verify", s.Base+"^{commit}")
		if err != nil {
			return fmt.Errorf("base %q: %w", s.Base, err)
		}
		startRef = "refs/podagent/start/" + s.Container
		if _, err := gitOut(s.Repo, "update-ref", startRef, sha); err != nil {
			return err
		}
		defer gitOut(s.Repo, "update-ref", "-d", startRef)
	}
	mode := "fresh"
	if s.shared() {
		mode = "shared"
	}
	origin, _ := gitOut(s.Repo, "remote", "get-url", "origin")
	fmt.Fprintf(os.Stderr, "podagent: seeding container repo on branch %s\n", s.GitBranch)
	return containerExec(s.Container, execOpts{}, "bash", "-c", initRepoScript, "init",
		s.GitBranch, mode, origin, startRef)
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

type syncPlan int

const (
	syncNothing        syncPlan = iota
	syncUpdate                  // move the host branch ref
	syncUpdateAndReset          // move it and refresh the shared checkout's index
	syncKeepAside               // leave the commits at refs/podagent/out/<branch>
)

// planSync decides what to do with the container's branch tip. checkedOutAt
// lists host worktrees that have the branch checked out; sharedPath is the
// checkout mounted into the container ("" when the workspace isn't shared).
func planSync(old, tip string, fastForward bool, checkedOutAt []string, sharedPath string) syncPlan {
	switch {
	case old == tip:
		return syncNothing
	case old != "" && !fastForward:
		return syncKeepAside
	case len(checkedOutAt) == 0:
		return syncUpdate
	case sharedPath != "" && len(checkedOutAt) == 1 && checkedOutAt[0] == sharedPath:
		return syncUpdateAndReset
	default:
		return syncKeepAside
	}
}

// checkedOutAt returns host worktree paths that have branch checked out.
func checkedOutAt(repo, branch string) []string {
	out, err := gitOut(repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}
	var paths []string
	var cur string
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			cur = realPath(p)
		}
		if line == "branch refs/heads/"+branch {
			paths = append(paths, cur)
		}
	}
	return paths
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// sync brings the container branch's commits back to the host repo.
func (s *Session) sync() error {
	b := s.GitBranch
	tip, err := containerShellOutput(s.Container, containerWorkspace, `git rev-parse --verify "refs/heads/$0"`+" 2>/dev/null || true", b)
	if err != nil {
		return err
	}
	if !shaRe.MatchString(tip) {
		return fmt.Errorf("container branch %s not found (got %q)", b, tip)
	}
	aside := "refs/podagent/out/" + b
	if _, err := gitOut(s.Repo, "cat-file", "-e", tip+"^{commit}"); err != nil {
		if err := s.fetchBundle(b, aside); err != nil {
			return err
		}
	} else if _, err := gitOut(s.Repo, "update-ref", aside, tip); err != nil {
		return err
	}

	old, _ := gitOut(s.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+b)
	ff := old == "" || exec.Command("git", append(append([]string{"-C", s.Repo}, hostGitSafety...),
		"merge-base", "--is-ancestor", old, tip)...).Run() == nil
	shared := ""
	if s.shared() {
		shared = realPath(s.Repo)
	}
	switch planSync(old, tip, ff, checkedOutAt(s.Repo, b), shared) {
	case syncNothing:
		fmt.Fprintf(os.Stderr, "podagent: %s already up to date on the host\n", b)
	case syncUpdate:
		if _, err := gitOut(s.Repo, "update-ref", "refs/heads/"+b, tip); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "podagent: host branch %s -> %s\n", b, tip[:12])
	case syncUpdateAndReset:
		if _, err := gitOut(s.Repo, "update-ref", "refs/heads/"+b, tip); err != nil {
			return err
		}
		// The files already match the container; refresh the index only.
		if _, err := gitOut(s.Repo, "reset", "-q"); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "podagent: host branch %s -> %s (index refreshed)\n", b, tip[:12])
	case syncKeepAside:
		fmt.Fprintf(os.Stderr, "podagent: host %s has diverged or is checked out elsewhere; commits kept at %s\n", b, aside)
	}
	return nil
}

// fetchBundle copies the container's new commits out as a git bundle and
// fetches it into ref. A bundle is plain data: no hooks or config travel
// with it. It first tries a bundle of only the commits the host lacks, then
// falls back to the whole branch.
func (s *Session) fetchBundle(branch, ref string) error {
	const inside = "/tmp/podagent-out.bundle"
	dir, err := os.MkdirTemp("", "podagent-sync-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	local := filepath.Join(dir, "out.bundle")
	var lastErr error
	for _, extra := range []string{" --not --remotes=host --remotes=origin", ""} {
		script := `rm -f ` + inside + ` && git bundle create -q ` + inside + ` "refs/heads/$0"` + extra
		if _, lastErr = containerShellOutput(s.Container, containerWorkspace, script, branch); lastErr != nil {
			continue
		}
		if lastErr = podman("cp", s.Container+":"+inside, local); lastErr != nil {
			continue
		}
		if _, lastErr = gitOut(s.Repo, "fetch", "-q", "--no-tags", local, "+refs/heads/"+branch+":"+ref); lastErr == nil {
			return nil
		}
	}
	return fmt.Errorf("sync %s: %w", branch, lastErr)
}
