package main

import (
	"bufio"
	"fmt"
	"os"
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

// Session is one agent container bound to a repo.
type Session struct {
	Agent     Agent
	Repo      string // main checkout on the host
	GitDir    string // the host repo's .git (common dir)
	Workspace string // host directory mounted at /home/agent/workspace
	Branch    string // --branch; empty means the host checkout itself is shared
	GitBranch string // branch the container's repo works on
	Base      string
	Container string
}

// shared reports whether the host checkout itself is the workspace.
func (s *Session) shared() bool { return s.Branch == "" }

func repoRoot(dir string) (root, gitDir string, err error) {
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	// --git-common-dir handles being called from inside a worktree.
	common, err := gitOut(abs, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", "", fmt.Errorf("%s is not inside a git repository", abs)
	}
	return filepath.Dir(common), common, nil
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

// newSession resolves the repo. Without a branch the host checkout is the
// workspace; with one, a separate directory .sandcastle/worktrees/<branch>.
func newSession(a Agent, repoFlag, branch, base string) (*Session, error) {
	repo, gitDir, err := repoRoot(repoFlag)
	if err != nil {
		return nil, err
	}
	s := &Session{Agent: a, Repo: repo, GitDir: gitDir, Workspace: repo, Branch: branch,
		GitBranch: branch, Base: base, Container: containerName(repo, a, branch)}
	if s.Base == "" {
		s.Base = defaultBase(repo)
	}
	if branch != "" {
		s.Workspace = filepath.Join(repo, ".sandcastle", "worktrees", slug(branch))
		return s, nil
	}
	// Empty on a detached HEAD; only creating a container needs it.
	s.GitBranch, _ = gitOut(repo, "symbolic-ref", "--short", "-q", "HEAD")
	return s, nil
}

// ensureWorkspaceDir creates the --branch directory. It is a plain folder:
// the container's repo checks the branch out into it.
func (s *Session) ensureWorkspaceDir() error {
	if s.shared() {
		return nil
	}
	if st, err := os.Lstat(filepath.Join(s.Workspace, ".git")); err == nil && !st.IsDir() {
		return fmt.Errorf("%s is a git worktree from an older podagent; remove it with `git worktree remove %s` first", s.Workspace, s.Workspace)
	}
	parent := filepath.Dir(s.Workspace)
	if err := os.MkdirAll(s.Workspace, 0o755); err != nil {
		return err
	}
	ignore := filepath.Join(parent, ".gitignore")
	if _, err := os.Stat(ignore); os.IsNotExist(err) {
		return os.WriteFile(ignore, []byte("*\n"), 0o644)
	}
	return nil
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
		return false, s.attach()
	}
	if s.GitBranch == "" {
		return false, fmt.Errorf("%s has a detached HEAD; check out a branch or pass --branch", s.Repo)
	}
	if err := s.ensureWorkspaceDir(); err != nil {
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

	// The workspace files are shared; git metadata is not (see gitsync.go).
	mounts := []Mount{
		{Host: s.Workspace, Container: containerWorkspace},
		{Host: s.GitDir, Container: hostGitMount, ReadOnly: true},
	}
	spec := ContainerSpec{
		Name: s.Container, Image: imageName(s.Agent), Env: env, Ports: o.Ports,
		Labels: map[string]string{
			"podagent.agent": s.Agent.Name, "podagent.repo": s.Repo,
			"podagent.branch": s.Branch, gitBranchLabel: s.GitBranch,
		},
		Volumes: []string{gitVolume(s.Container) + ":" + containerWorkspace + "/.git:U"},
	}
	spec.Mounts = mounts

	fmt.Fprintf(os.Stderr, "podagent: starting container %s (%s mounted at %s)\n", s.Container, s.Workspace, containerWorkspace)
	if err := createContainer(spec); err != nil {
		return false, err
	}
	if err := s.initContainerRepo(); err != nil {
		// Don't leave a half-seeded container around to be reused.
		_ = podman("rm", "-f", "--time", "0", s.Container)
		_ = podman("volume", "rm", "-f", gitVolume(s.Container))
		return false, fmt.Errorf("seed container repo: %w", err)
	}
	if o.HostAuth {
		if err := s.copyHostAuth(); err != nil {
			return true, err
		}
	}
	if o.Setup != "" {
		fmt.Fprintf(os.Stderr, "podagent: setup: %s\n", o.Setup)
		if err := containerExec(s.Container, execOpts{}, "bash", "-lc", o.Setup); err != nil {
			return true, fmt.Errorf("setup command failed: %w", err)
		}
	}
	return true, nil
}

const gitBranchLabel = "podagent.gitbranch"

// attach starts an existing container and reads the branch its repo was
// created on, which may differ from the host's current branch by now.
func (s *Session) attach() error {
	if !containerRunning(s.Container) {
		if err := podman("start", s.Container); err != nil {
			return err
		}
	}
	b, err := podmanOutput("inspect", "-f", `{{index .Config.Labels "`+gitBranchLabel+`"}}`, s.Container)
	if err != nil {
		return err
	}
	if b == "" || b == "<no value>" {
		return fmt.Errorf("container %s was made by an older podagent; `podman rm -f %s` and run again", s.Container, s.Container)
	}
	s.GitBranch = b
	return nil
}

// builtinArgs mirrors Sandcastle's SOURCE_BRANCH / TARGET_BRANCH.
func (s *Session) builtinArgs() map[string]string {
	return map[string]string{
		"SOURCE_BRANCH": s.GitBranch,
		"BRANCH":        s.GitBranch,
		"TARGET_BRANCH": strings.TrimPrefix(s.Base, "origin/"),
	}
}

// head is the container repo's HEAD commit.
func (s *Session) head() string {
	h, _ := containerShellOutput(s.Container, containerWorkspace, "git rev-parse HEAD")
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

// copyHostAuth copies the agent's credential files into the container. They
// are copies, not mounts, so nothing the agent does reaches the host's
// files. A token the agent refreshes inside stays inside.
func (s *Session) copyHostAuth() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	for _, rel := range s.Agent.HostAuth {
		src := filepath.Join(home, rel)
		if st, err := os.Stat(src); err != nil || !st.Mode().IsRegular() {
			continue
		}
		dst := filepath.Join(containerHome, rel)
		if err := containerExec(s.Container, execOpts{}, "mkdir", "-p", filepath.Dir(dst)); err != nil {
			return err
		}
		if err := podman("cp", src, s.Container+":"+dst); err != nil {
			return fmt.Errorf("copy %s into container: %w", src, err)
		}
		owner := fmt.Sprintf("%d:%d", containerUID, containerGID)
		if err := podman("exec", "-u", "0", s.Container, "chown", owner, dst); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "podagent: copied ~/%s into the container\n", rel)
	}
	return nil
}
