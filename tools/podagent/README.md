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
podagent sync claude                # copy the container's commits to your repo
podagent ls                         # list containers
podagent rm claude                  # sync, then remove the container
podagent prompts                    # prompt names this repo provides
```

Commands that target a container take the same `--repo` / `--branch` flags as
the `run` that created it.

Without `--branch` the container works on your checkout's files and current
branch. With `--branch fix-42` it works in a separate folder,
`.sandcastle/worktrees/fix-42/`, on that branch (created from `--base`,
default `origin/main`). Either way, commits reach your repo only through
sync; see [Git and commits](#git-and-commits).

### Software factory

```bash
podagent factory claude --issue 42 --title "Fix x" --branch fix-42
```

Same steps as `.factory/implement-task.ts`, on Podman instead of Docker:
works on the branch in `.sandcastle/worktrees/<branch>/` (from `--base`,
default `origin/main`), runs `npm install && npm run build` once, runs the
`implement` prompt, then the `review` prompt if the implementer committed,
and syncs the commits to the branch in your repo after each.
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
- Repo mounts use the SELinux `:z` label.
- The agent runs with its permission prompts turned off
  (`--dangerously-skip-permissions` and similar). The container is the
  sandbox, and the workspace files are writable.

## Git and commits

The agent never gets write access to your repo's `.git`. Anything it wrote
there (a hook, a `core.fsmonitor` setting, a `commondir` file) could make git
on your host run its code. So:

- The workspace **files** are shared live, so you see edits as they happen.
- The container has its **own git repo**, in a podman volume mounted over
  `workspace/.git`, seeded from your repo when the container is created.
  Your `.git` is mounted read-only, only for that seeding.
- **Sync** copies the container branch's commits back as a git bundle, which
  is plain data, and fetches it with hooks and fsmonitor turned off. It runs
  after `run` and after each `factory` step, before `rm`, and on
  `podagent sync`.

What sync does with the commits:

- Your branch moves forward to the container's commit when it's a
  fast-forward and the branch isn't checked out elsewhere.
- Without `--branch`, your checked-out branch moves forward and your index is
  refreshed (`git reset`, which keeps your files but drops anything you had
  staged), since the files already match.
- When your branch has diverged, or is checked out in another worktree, the
  commits are kept at `refs/podagent/out/<branch>` for you to merge.

`podagent rm` refuses to remove a container whose final sync fails, because
the container holds the only copy of its commits; `--force` removes it
anyway.

The agent can still write any file in the workspace, including a nested
`.git` folder in a subdirectory. Don't run git inside a subfolder the agent
created until you've looked at it.

## VS Code

`podagent code` opens VS Code attached to the running container. It needs the
**Dev Containers** extension and, in VS Code settings:

```json
{ "dev.containers.dockerPath": "podman" }
```

Or, inside VS Code: **Dev Containers: Attach to Running Container…** and pick
the container from `podagent ls`.
