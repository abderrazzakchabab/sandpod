// Command podagent runs a coding agent (claude, codex, opencode, pi) in a
// long-lived Podman container with this repo, its prompts and its software
// factory entry point. Attach a shell or VS Code to the same container.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

var verbose bool

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

const usage = `podagent — run coding agents in Podman with this repo's prompts

Usage:
  podagent run <agent> [flags]        start (or reuse) the container and run the agent
  podagent factory <agent> [flags]    software-factory task: implement, then review
  podagent shell <agent> [flags]      open bash in the agent's container
  podagent code <agent> [flags]       open VS Code attached to the container
  podagent build <agent> [--no-cache] build the agent image
  podagent ls                         list podagent containers
  podagent rm <agent> [flags]         remove the agent's container
  podagent prompts [--repo DIR]       list prompts found in the repo

Agents: ` + "claude, codex, opencode, pi" + `

Run "podagent <command> -h" for flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	cmd, rest := os.Args[1], os.Args[2:]
	switch cmd {
	case "run":
		err = cmdRun(rest)
	case "factory":
		err = cmdFactory(rest)
	case "shell", "sh":
		err = cmdShell(rest)
	case "code", "vscode":
		err = cmdCode(rest)
	case "build":
		err = cmdBuild(rest)
	case "ls", "list":
		err = cmdList()
	case "rm", "remove":
		err = cmdRemove(rest)
	case "prompts":
		err = cmdPrompts(rest)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "podagent:", err)
		os.Exit(1)
	}
}

// splitAgent pulls the leading agent name off args so flags can follow it.
func splitAgent(fs *flag.FlagSet, args []string) (Agent, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
			fs.Usage()
			os.Exit(0)
		}
		return Agent{}, fmt.Errorf("%s: missing agent name (%s)", fs.Name(), strings.Join(agentNames(), ", "))
	}
	a, err := lookupAgent(args[0])
	if err != nil {
		return Agent{}, err
	}
	if err := fs.Parse(args[1:]); err != nil {
		return Agent{}, err
	}
	if fs.NArg() > 0 {
		return Agent{}, fmt.Errorf("%s: unexpected arguments: %s", fs.Name(), strings.Join(fs.Args(), " "))
	}
	return a, nil
}

// sessionFlags are shared by every command that targets a container.
type sessionFlags struct {
	repo, branch, base string
}

func (f *sessionFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.repo, "repo", "", "repository to mount (default: git repo of the current dir)")
	fs.StringVar(&f.branch, "branch", "", "work in a git worktree on this branch (.sandcastle/worktrees/<branch>)")
	fs.StringVar(&f.base, "base", "", "base ref for a new --branch (default: origin/main, else HEAD)")
	fs.BoolVar(&verbose, "v", false, "print podman commands")
}

func (f *sessionFlags) session(a Agent) (*Session, error) {
	return newSession(a, f.repo, f.branch, f.base)
}

type startFlags struct {
	envFile, setup string
	hostAuth       bool
	rebuild        bool
	ports          multi
}

func (f *startFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.envFile, "env-file", "", "env file passed to the container (default: <repo>/.sandcastle/.env)")
	fs.StringVar(&f.setup, "setup", "", "shell command run once when the container is created (e.g. \"npm install\")")
	fs.BoolVar(&f.hostAuth, "host-auth", false, "copy the agent's credential file from your home into the container (e.g. ~/.codex/auth.json)")
	fs.BoolVar(&f.rebuild, "rebuild", false, "rebuild the agent image first")
	fs.Var(&f.ports, "p", "publish a port, podman syntax (repeatable), e.g. -p 3000:3000")
}

func (f *startFlags) opts() startOpts {
	return startOpts{EnvFile: f.envFile, Setup: f.setup, HostAuth: f.hostAuth, Rebuild: f.rebuild, Ports: f.ports}
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var sf sessionFlags
	var st startFlags
	var promptArgs multi
	sf.register(fs)
	st.register(fs)
	prompt := fs.String("prompt", "", "prompt name or file (list them with: podagent prompts); runs headless unless -i")
	model := fs.String("model", "", "model passed to the agent (default: the agent's own default)")
	interactive := fs.Bool("i", false, "run the agent's interactive TUI (default when no --prompt)")
	detach := fs.Bool("d", false, "only start the container; don't run the agent")
	fs.Var(&promptArgs, "arg", "prompt argument KEY=VALUE for {{KEY}} (repeatable)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: podagent run <agent> [flags]")
		fs.PrintDefaults()
	}
	a, err := splitAgent(fs, args)
	if err != nil {
		return err
	}
	pargs, err := parseArgs(promptArgs)
	if err != nil {
		return err
	}
	s, err := sf.session(a)
	if err != nil {
		return err
	}
	if _, err := s.ensureContainer(st.opts()); err != nil {
		return err
	}
	defer printAttachHint(s)
	switch {
	case *detach:
		return nil
	case *prompt != "":
		return s.runPrompt(*prompt, pargs, *model, *interactive)
	default:
		return containerExec(s.Container, execOpts{TTY: true}, a.Interactive(*model, "")...)
	}
}

// cmdFactory is the Podman version of .factory/implement-task.ts: it reads
// the same FACTORY_* variables the software-factory daemon sets, so the
// daemon can spawn `podagent factory claude` as its implementer.
func cmdFactory(args []string) error {
	fs := flag.NewFlagSet("factory", flag.ExitOnError)
	var sf sessionFlags
	var st startFlags
	sf.register(fs)
	st.register(fs)
	issue := fs.String("issue", os.Getenv("FACTORY_TASK_ISSUE_NUMBER"), "GitHub issue number [FACTORY_TASK_ISSUE_NUMBER]")
	title := fs.String("title", os.Getenv("FACTORY_TASK_DESCRIPTION"), "task description [FACTORY_TASK_DESCRIPTION]")
	model := fs.String("model", "", "model passed to the agent")
	implement := fs.String("implement-prompt", "implement", "implementer prompt")
	review := fs.String("review-prompt", "review", "reviewer prompt (empty to skip)")
	keep := fs.Bool("keep", true, "keep the container afterwards for shell/code")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: podagent factory <agent> --issue N --title T --branch B [flags]")
		fs.PrintDefaults()
	}
	a, err := splitAgent(fs, args)
	if err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	for name, ptr := range map[string]*string{
		"branch": &sf.branch, "base": &sf.base, "repo": &sf.repo,
	} {
		if !set[name] {
			*ptr = os.Getenv(map[string]string{
				"branch": "FACTORY_BRANCH", "base": "FACTORY_BASE", "repo": "FACTORY_REPO_PATH",
			}[name])
		}
	}
	if !set["setup"] {
		// Same onSandboxReady hook as .factory/implement-task.ts.
		st.setup = "npm install && npm run build"
	}
	if *issue == "" || *title == "" || sf.branch == "" {
		return errors.New("factory: --issue, --title and --branch are required (or FACTORY_* env vars)")
	}
	s, err := sf.session(a)
	if err != nil {
		return err
	}
	if _, err := s.ensureContainer(st.opts()); err != nil {
		return err
	}
	if !*keep {
		defer podman("rm", "-f", "--time", "0", s.Container)
	} else {
		defer printAttachHint(s)
	}
	pargs := map[string]string{"ISSUE_NUMBER": *issue, "ISSUE_TITLE": *title, "BRANCH": sf.branch}

	before := s.head()
	if err := s.runPrompt(*implement, pargs, *model, false); err != nil {
		return fmt.Errorf("implementer: %w", err)
	}
	if *review == "" {
		return nil
	}
	if s.head() == before {
		fmt.Fprintln(os.Stderr, "podagent: implementer made no commits; skipping review")
		return nil
	}
	if err := s.runPrompt(*review, pargs, *model, false); err != nil {
		return fmt.Errorf("reviewer: %w", err)
	}
	return nil
}

func targetSession(name string, args []string) (*Session, error) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	var sf sessionFlags
	sf.register(fs)
	a, err := splitAgent(fs, args)
	if err != nil {
		return nil, err
	}
	s, err := sf.session(a)
	if err != nil {
		return nil, err
	}
	if err := requirePodman(); err != nil {
		return nil, err
	}
	if !containerExists(s.Container) {
		return nil, fmt.Errorf("no container %s; start it with `podagent run %s -d`", s.Container, a.Name)
	}
	if !containerRunning(s.Container) {
		if err := podman("start", s.Container); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func cmdShell(args []string) error {
	s, err := targetSession("shell", args)
	if err != nil {
		return err
	}
	return containerExec(s.Container, execOpts{TTY: true}, "bash", "-l")
}

func cmdCode(args []string) error {
	s, err := targetSession("code", args)
	if err != nil {
		return err
	}
	return openVSCode(s.Container, containerWorkspace)
}

func cmdBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	noCache := fs.Bool("no-cache", false, "build without cache (picks up new agent CLI releases)")
	fs.BoolVar(&verbose, "v", false, "print podman commands")
	a, err := splitAgent(fs, args)
	if err != nil {
		return err
	}
	if err := requirePodman(); err != nil {
		return err
	}
	return buildImage(a, *noCache)
}

func cmdList() error {
	if err := requirePodman(); err != nil {
		return err
	}
	return podman("ps", "-a", "--filter", "label=podagent=1",
		"--format", "table {{.Names}}\t{{.Status}}\t{{.Label \"podagent.agent\"}}\t{{.Label \"podagent.branch\"}}")
}

func cmdRemove(args []string) error {
	fs := flag.NewFlagSet("rm", flag.ExitOnError)
	var sf sessionFlags
	sf.register(fs)
	a, err := splitAgent(fs, args)
	if err != nil {
		return err
	}
	s, err := sf.session(a)
	if err != nil {
		return err
	}
	if err := requirePodman(); err != nil {
		return err
	}
	if err := podman("rm", "-f", "--time", "0", s.Container); err != nil {
		return err
	}
	if s.Branch != "" {
		fmt.Fprintf(os.Stderr, "podagent: worktree kept at %s (git worktree remove it when done)\n", s.Workspace)
	}
	return nil
}

func cmdPrompts(args []string) error {
	fs := flag.NewFlagSet("prompts", flag.ExitOnError)
	repoFlag := fs.String("repo", "", "repository (default: git repo of the current dir)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repo, err := repoRoot(*repoFlag)
	if err != nil {
		return err
	}
	for _, p := range listPrompts(repo) {
		fmt.Println(p)
	}
	return nil
}

func printAttachHint(s *Session) {
	extra := ""
	if s.Branch != "" {
		extra = " --branch " + s.Branch
	}
	fmt.Fprintf(os.Stderr, "\npodagent: container %s is still running.\n  shell:   podagent shell %s%s\n  vscode:  podagent code %s%s\n  remove:  podagent rm %s%s\n",
		s.Container, s.Agent.Name, extra, s.Agent.Name, extra, s.Agent.Name, extra)
}
