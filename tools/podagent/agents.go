package main

import (
	"fmt"
	"sort"
	"strings"
)

// Agent describes how to install and launch one coding agent CLI inside the
// container. The install lines and flags mirror the agent providers in
// src/AgentProvider.ts and the Dockerfile templates in src/InitService.ts.
type Agent struct {
	Name string
	// Install is a Containerfile fragment. RootInstall runs before
	// `USER agent`; otherwise it runs as the agent user.
	Install     string
	RootInstall bool
	// HostAuth lists paths relative to $HOME that hold the agent's login
	// state; --host-auth bind-mounts them into the container's home.
	HostAuth []string
	// Print builds the non-interactive command. When it returns a non-empty
	// stdin, the prompt is piped in instead of passed as an argument.
	Print func(model, prompt string) (args []string, stdin string)
	// Interactive builds the TUI command, optionally seeded with a prompt.
	Interactive func(model, prompt string) []string
}

func withModel(args []string, model string) []string {
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}

var agents = map[string]Agent{
	"claude": {
		Name:     "claude",
		Install:  "RUN curl -fsSL https://claude.ai/install.sh | bash",
		HostAuth: []string{".claude", ".claude.json"},
		Print: func(model, prompt string) ([]string, string) {
			return withModel([]string{"claude", "--print", "--dangerously-skip-permissions"}, model), prompt
		},
		Interactive: func(model, prompt string) []string {
			args := withModel([]string{"claude", "--dangerously-skip-permissions"}, model)
			if prompt != "" {
				args = append(args, prompt)
			}
			return args
		},
	},
	"codex": {
		Name:        "codex",
		Install:     "RUN npm install -g @openai/codex",
		RootInstall: true,
		HostAuth:    []string{".codex"},
		Print: func(model, prompt string) ([]string, string) {
			// `-` makes `codex exec` read the prompt from stdin.
			return append(withModel([]string{"codex", "exec", "--dangerously-bypass-approvals-and-sandbox"}, model), "-"), prompt
		},
		Interactive: func(model, prompt string) []string {
			args := withModel([]string{"codex", "--dangerously-bypass-approvals-and-sandbox"}, model)
			if prompt != "" {
				args = append(args, prompt)
			}
			return args
		},
	},
	"opencode": {
		Name:        "opencode",
		Install:     "RUN npm install -g opencode-ai@latest",
		RootInstall: true,
		HostAuth:    []string{".config/opencode", ".local/share/opencode"},
		Print: func(model, prompt string) ([]string, string) {
			return append(withModel([]string{"opencode", "run", "--dangerously-skip-permissions"}, model), prompt), ""
		},
		Interactive: func(model, prompt string) []string {
			args := withModel([]string{"opencode"}, model)
			// The TUI's seed flag is --prompt; it pre-fills but does not submit.
			if prompt != "" {
				args = append(args, "--prompt", prompt)
			}
			return args
		},
	},
	"pi": {
		Name:        "pi",
		Install:     "RUN npm install -g @earendil-works/pi-coding-agent",
		RootInstall: true,
		HostAuth:    []string{".pi"},
		Print: func(model, prompt string) ([]string, string) {
			return withModel([]string{"pi", "-p"}, model), prompt
		},
		Interactive: func(model, prompt string) []string {
			args := withModel([]string{"pi"}, model)
			if prompt != "" {
				args = append(args, prompt)
			}
			return args
		},
	},
}

func agentNames() []string {
	names := make([]string, 0, len(agents))
	for n := range agents {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func lookupAgent(name string) (Agent, error) {
	a, ok := agents[strings.ToLower(name)]
	if !ok {
		return Agent{}, fmt.Errorf("unknown agent %q (want one of: %s)", name, strings.Join(agentNames(), ", "))
	}
	return a, nil
}

// containerfile renders the image definition for an agent. It follows
// .sandcastle/Dockerfile: node:22-bookworm, git/gh/jq, an `agent` user whose
// UID/GID match --userns=keep-id, and `sleep infinity` so the container stays
// up for `podagent shell` and VS Code.
func containerfile(a Agent) string {
	var root, user string
	if a.RootInstall {
		root = a.Install + "\n"
	} else {
		user = a.Install + "\n"
	}
	return `FROM docker.io/library/node:22-bookworm

RUN apt-get update && apt-get install -y \
  git curl jq ripgrep less procps openssh-client ca-certificates \
  && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
  | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg \
  && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
  > /etc/apt/sources.list.d/github-cli.list \
  && apt-get update && apt-get install -y gh \
  && rm -rf /var/lib/apt/lists/*

` + root + `
ARG AGENT_UID=1000
ARG AGENT_GID=1000
RUN groupmod -o -g $AGENT_GID node && usermod -o -u $AGENT_UID -g $AGENT_GID -d /home/agent -m -l agent node
USER ${AGENT_UID}:${AGENT_GID}
ENV PATH="/home/agent/.local/bin:$PATH"
` + user + `
WORKDIR /home/agent/workspace
ENTRYPOINT ["sleep", "infinity"]
`
}
