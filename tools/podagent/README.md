# podagent

Go CLI that runs a coding agent — `claude`, `codex`, `opencode` or `pi` — in a
long-lived **Podman** container with this repo mounted, using the repo's own
prompts (`.sandcastle/*-prompt.md`, `.sandcastle/agent-workflows/*/prompt.md`)
and the software-factory implementer flow from `.factory/implement-task.ts`.
The container keeps running afterwards, so you can open a shell in it or
attach VS Code.

## Build

```bash
cd tools/podagent
go build -o podagent .   # or: go install .
```

Needs `podman` and `git` on the host (and `code` for `podagent code`).

## Use

```bash
podagent run claude                 # interactive TUI in the container
podagent run codex -d               # just start the container
podagent run opencode --prompt plan # headless run of .sandcastle/plan-prompt.md
podagent run claude --prompt implement \
  --arg ISSUE_NUMBER=42 --arg ISSUE_TITLE="Fix x" --branch fix-42

podagent shell claude               # bash in /home/agent/workspace
podagent code claude                # VS Code attached to the container
podagent ls                         # list containers
podagent rm claude                  # remove the container
podagent prompts                    # prompt names this repo provides
```

Commands that target a container take the same `--repo` / `--branch` flags as
the `run` that created it.

### Software factory

```bash
podagent factory claude --issue 42 --title "Fix x" --branch fix-42
```

Same steps as `.factory/implement-task.ts`, on Podman instead of Docker:
creates a worktree for the branch (from `--base`, default `origin/main`) in
`.sandcastle/worktrees/`, runs `npm install && npm run build` once, runs the
`implement` prompt, then the `review` prompt if the implementer committed.
With no flags it reads the daemon's `FACTORY_BRANCH`, `FACTORY_BASE`,
`FACTORY_REPO_PATH`, `FACTORY_TASK_DESCRIPTION` and
`FACTORY_TASK_ISSUE_NUMBER`, so the daemon can spawn `podagent factory claude`
as its implementer. It doesn't push or open the PR; the daemon does that.

## Prompts

`--prompt` takes a file path or a name:

| Name               | File                                           |
| ------------------ | ---------------------------------------------- |
| `implement`        | `.sandcastle/implement-prompt.md`              |
| `factory/implement`| `.factory/implement-prompt.md`                 |
| `workflow/review`  | `.sandcastle/agent-workflows/review/prompt.md` |

`{{KEY}}` is replaced from `--arg KEY=VALUE`. `SOURCE_BRANCH`,
`TARGET_BRANCH` and `BRANCH` are filled in for you. Each ``!`command` `` in the
prompt file runs inside the container and is replaced by its output, as in
Sandcastle. Commands that arrive through argument values are not run, and a
`{{KEY}}` inside a block is replaced by a single-quoted shell word, so write
``!`gh issue view {{ISSUE_NUMBER}}` `` without adding quotes of your own.

## Auth and environment

- Env vars come from `.sandcastle/.env` (or `--env-file`), plus these host
  variables when set: `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`,
  `OPENAI_API_KEY`, `OPENROUTER_API_KEY`, `GEMINI_API_KEY`, `GH_TOKEN`,
  `GITHUB_TOKEN`.
- `--host-auth` copies the agent's credential files from your home into the
  container when it is created: `~/.claude/.credentials.json` and
  `~/.claude.json`, `~/.codex/auth.json`,
  `~/.local/share/opencode/auth.json`, or `~/.pi/agent/auth.json`. They are
  copies, so the agent can't change your host's agent settings (a hook or an
  MCP command written there would run the next time you used the agent on
  your host). A token the agent refreshes stays in the container. If it
  expires, `podagent rm` and run again.
- Commits use your host `git config user.name` / `user.email`.
- Env and mounts are fixed when the container is created. `podagent rm` and
  run again to change them.

## Container details

- Image `localhost/podagent-<agent>` from `node:22-bookworm` with git, gh,
  jq and ripgrep, like `.sandcastle/Dockerfile`. `podagent build <agent>
  --no-cache` picks up new agent releases.
- `--userns=keep-id:uid=1000,gid=1000`: your host user is `agent` inside, so
  files the agent writes stay yours.
- Repo mounts use the SELinux `:z` label. Worktree runs also mount the main
  `.git` at its host path so git works inside.
- `.git/hooks` and `.git/config` are mounted read-only, so the agent can't
  plant a hook or a `core.fsmonitor` command that would run on your host the
  next time you use git. podagent's own host git calls also disable hooks
  and fsmonitor. Everything else in the repo, including the rest of `.git`,
  stays writable to the agent.
- The agent runs with its permission prompts turned off
  (`--dangerously-skip-permissions` and similar). The container is the
  sandbox, and the mounted repo is writable.

## VS Code

`podagent code` opens VS Code attached to the running container. It needs the
**Dev Containers** extension and, in VS Code settings:

```json
{ "dev.containers.dockerPath": "podman" }
```

Or, inside VS Code: **Dev Containers: Attach to Running Container…** and pick
the container from `podagent ls`.
