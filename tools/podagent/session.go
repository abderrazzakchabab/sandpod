package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// passthroughEnv are host variables forwarded into the container when set.
// .sandcastle/.env (or --env-file) overrides them.
var passthroughEnv = []string{
	"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY",
	"OPENROUTER_API_KEY", "GEMINI_API_KEY", "GH_TOKEN", "GITHUB_TOKEN",
}

// Session is one agent container bound to a repo (or a worktree of it).
type Session struct {
	Agent     Agent
	Repo      string // main checkout on the host
	Workspace string // directory mounted at /home/agent/workspace
	Branch    string // set when Workspace is a worktree
	Base      string
	Container string
}

// gitGuardMounts mounts the repo's git hooks and config read-only over the
// read-write .git. The agent runs without permission prompts, and a hook or
// a config key like core.fsmonitor that it wrote there would run on the
// host the next time anyone ran git in the repo.
func gitGuardMounts(hostGitDir, containerGitDir string) ([]Mount, error) {
	hooks := filepath.Join(hostGitDir, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		return nil, err
	}
	return []Mount{
		{Host: hooks, Container: filepath.Join(containerGitDir, "hooks"), ReadOnly: true},
		{Host: filepath.Join(hostGitDir, "config"), Container: filepath.Join(containerGitDir, "config"), ReadOnly: true},
	}, nil
}

// hostGitSafety stops podagent's own host-side git calls from running hooks
// or an fsmonitor command taken from the repo.
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

func repoRoot(dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	// --git-common-dir handles being called from inside a worktree.
	common, err := gitOut(abs, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repository", abs)
	}
	return filepath.Dir(common), nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func containerName(repo string, a Agent, branch string) string {
	name := "podagent-" + slug(filepath.Base(repo)) + "-" + a.Name
	if branch != "" {
		name += "-" + slug(branch)
	}
	return name
}

func defaultBase(repo string) string {
	if _, err := gitOut(repo, "rev-parse", "--verify", "--quiet", "origin/main"); err == nil {
		return "origin/main"
	}
	return "HEAD"
}

// newSession resolves the repo and, when branch is set, creates or reuses a
// git worktree at .sandcastle/worktrees/<branch> (already gitignored).
func newSession(a Agent, repoFlag, branch, base string) (*Session, error) {
	repo, err := repoRoot(repoFlag)
	if err != nil {
		return nil, err
	}
	s := &Session{Agent: a, Repo: repo, Workspace: repo, Branch: branch, Base: base,
		Container: containerName(repo, a, branch)}
	if branch == "" {
		return s, nil
	}
	if s.Base == "" {
		s.Base = defaultBase(repo)
	}
	s.Workspace = filepath.Join(repo, ".sandcastle", "worktrees", slug(branch))
	return s, nil
}

func (s *Session) ensureWorktree() error {
	if s.Branch == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(s.Workspace, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.Workspace), 0o755); err != nil {
		return err
	}
	var args []string
	if _, err := gitOut(s.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+s.Branch); err == nil {
		args = []string{"worktree", "add", s.Workspace, s.Branch}
	} else {
		args = []string{"worktree", "add", "-b", s.Branch, s.Workspace, s.Base}
	}
	fmt.Fprintf(os.Stderr, "podagent: git %s\n", strings.Join(args, " "))
	_, err := gitOut(s.Repo, args...)
	return err
}

// readEnvFile parses KEY=VALUE lines; blank lines and # comments are skipped.
func readEnvFile(path string) (map[string]string, error) {
	out := map[string]string{}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if v != "" {
			out[k] = v
		}
	}
	return out, sc.Err()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type startOpts struct {
	EnvFile  string
	HostAuth bool
	Ports    []string
	Setup    string
	Rebuild  bool
}

// ensureContainer builds the image if needed and starts the container,
// reusing an existing one. Returns true when it was newly created.
func (s *Session) ensureContainer(o startOpts) (bool, error) {
	if err := requirePodman(); err != nil {
		return false, err
	}
	if o.Rebuild || !imageExists(imageName(s.Agent)) {
		if err := buildImage(s.Agent, false); err != nil {
			return false, err
		}
	}
	if containerExists(s.Container) {
		if !containerRunning(s.Container) {
			if err := podman("start", s.Container); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	if err := s.ensureWorktree(); err != nil {
		return false, err
	}

	env := map[string]string{}
	for _, k := range passthroughEnv {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	envFile := o.EnvFile
	if envFile == "" {
		envFile = filepath.Join(s.Repo, ".sandcastle", ".env")
	}
	fileEnv, err := readEnvFile(envFile)
	if err != nil {
		return false, err
	}
	for k, v := range fileEnv {
		env[k] = v
	}
	// Commit as the host user.
	for _, pair := range [][3]string{
		{"user.name", "GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"},
		{"user.email", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"},
	} {
		if v, err := gitOut(s.Repo, "config", pair[0]); err == nil && v != "" {
			env[pair[1]], env[pair[2]] = v, v
		}
	}

	mounts := []Mount{{Host: s.Workspace, Container: containerWorkspace}}
	gitDir := filepath.Join(s.Repo, ".git")
	containerGitDir := filepath.Join(containerWorkspace, ".git")
	if s.Workspace != s.Repo {
		// A worktree's .git file points at <repo>/.git/worktrees/<name> by
		// absolute host path, so mount the main .git at that same path.
		mounts = append(mounts, Mount{Host: gitDir, Container: gitDir})
		containerGitDir = gitDir
	}
	guards, err := gitGuardMounts(gitDir, containerGitDir)
	if err != nil {
		return false, err
	}
	mounts = append(mounts, guards...)
	spec := ContainerSpec{
		Name: s.Container, Image: imageName(s.Agent), Env: env, Ports: o.Ports,
		Labels: map[string]string{
			"podagent.agent": s.Agent.Name, "podagent.repo": s.Repo,
			"podagent.branch": s.Branch,
		},
	}
	if o.HostAuth {
		home, _ := os.UserHomeDir()
		for _, rel := range s.Agent.HostAuth {
			p := filepath.Join(home, rel)
			if _, err := os.Stat(p); err == nil {
				mounts = append(mounts, Mount{Host: p, Container: filepath.Join(containerHome, rel), NoRelabel: true})
			}
		}
	}
	spec.Mounts = mounts

	fmt.Fprintf(os.Stderr, "podagent: starting container %s (%s mounted at %s)\n", s.Container, s.Workspace, containerWorkspace)
	if err := createContainer(spec); err != nil {
		return false, err
	}
	if o.Setup != "" {
		fmt.Fprintf(os.Stderr, "podagent: setup: %s\n", o.Setup)
		if err := containerExec(s.Container, execOpts{}, "bash", "-lc", o.Setup); err != nil {
			return true, fmt.Errorf("setup command failed: %w", err)
		}
	}
	return true, nil
}

// builtinArgs mirrors Sandcastle's SOURCE_BRANCH / TARGET_BRANCH.
func (s *Session) builtinArgs() map[string]string {
	out := map[string]string{}
	if b, err := gitOut(s.Workspace, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		out["SOURCE_BRANCH"] = b
		out["BRANCH"] = b
	}
	target := s.Base
	if target == "" {
		target = defaultBase(s.Repo)
	}
	out["TARGET_BRANCH"] = strings.TrimPrefix(target, "origin/")
	return out
}

func (s *Session) head() string {
	h, _ := gitOut(s.Workspace, "rev-parse", "HEAD")
	return h
}

// runPrompt renders a prompt file inside the container and runs the agent.
func (s *Session) runPrompt(promptName string, userArgs map[string]string, model string, interactive bool) error {
	path, err := resolvePrompt(s.Repo, promptName)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	args := s.builtinArgs()
	for k, v := range userArgs {
		args[k] = v
	}
	prompt, err := renderPrompt(string(raw), args, func(c string) (string, error) {
		return containerShellOutput(s.Container, containerWorkspace, c)
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "podagent: %s running prompt %s\n", s.Agent.Name, path)
	if interactive {
		return containerExec(s.Container, execOpts{TTY: true}, s.Agent.Interactive(model, prompt)...)
	}
	argv, stdin := s.Agent.Print(model, prompt)
	if stdin == "" {
		return containerExec(s.Container, execOpts{}, argv...)
	}
	return containerExec(s.Container, execOpts{Stdin: &stdin}, argv...)
}
