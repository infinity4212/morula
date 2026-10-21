# morula

A desktop workbench for AI agent teams.

You talk to an assistant in a single conversation. It figures out which project a message belongs to, breaks it into tasks, and dispatches them to agents — who is doing what, where each task stands, whether output passed review — all visible on screen.

**Single conversation thread.** From the user's perspective there is one continuous conversation. No switching windows, no picking agents. The assistant handles routing, scheduling, and follow-ups on its own.

**Agent formula.** Every agent is assembled as `LLM + harness + skill`. Swap the LLM, change the harness, load a different skill set — the agent's behavior changes accordingly. codex, opencode, pi, hermes, workbuddy — whatever is installed locally just works. The assistant can add and adjust agents freely, composing the configuration that fits the team best.

**Automatic task tree.** One assignment unfolds into a tree of subtasks. Children execute independently, report back to the parent; the parent runs automated checks and sends failed work back for rework.

macOS desktop app, written in Go, windowed with Wails v3 on WebKit. Current version 0.1.0, Go 1.27.2.

## Build

Prerequisites: macOS 13+, Go 1.27+. Plugin support requires Bun (`https://bun.sh`).

```sh
make dev        # run directly
make build      # compile to dist/morula
make app        # bundle as dist/morula.app
make package    # produce installer dist/morula-0.1.0.dmg
make install    # install to /Applications/morula.app
make release    # bump version, package, publish to local update channel
make bump       # increment patch version
make clean      # remove dist
```

`make package` produces a universal DMG (Apple Silicon and Intel). It is not notarized and carries only an ad-hoc signature. On first launch, allow it under System Settings → Privacy & Security.

### Subcommands

```sh
morula                   # open desktop window (default)
morula web               # start HTTP server only (default 127.0.0.1:7788)
morula mcp               # stdio MCP server
morula mcp --assistant   # read-only MCP server for the assistant to query projects and agents
morula version           # print version
```

## Features

**Assistant** — The user interacts through a single ongoing conversation. The assistant is itself an agent, assembled as `model + harness + skill`, running in a persistent session. It can call tools, query projects, ask the user for confirmation, and dispatch tasks. Long-term memory is viewable and editable in settings.

**Agents** — The roster is stored in a local SQLite database shared between the desktop window and `morula mcp`. Each entry holds a name, role, harness, model, thinking effort, bound skills, and a role description. Disabled agents stop accepting new tasks and finish their current ones.

**Projects & task trees** — Projects are created, renamed, and merged by the assistant. Each project has its own working directory, project memory, and summary. Every assignment becomes a task tree: child nodes execute independently and report to their parent; the parent runs automated checks and rejects work that does not pass.

**Routines** — Triggered automatically at scheduled times. The assistant processes them verbatim; results appear in the conversation and the daily digest.

**Subscriptions & quotas** — One card per subscription, showing quota across windows and reset times.

**Plugins** — npm packages or local folders that connect third-party subscriptions to morula: login, list models, forward requests, query quotas. Installed into the `plugins/` directory under the data folder and run by Bun. Compatible with OpenCode-format auth plugins.

**AI tools (harnesses)** — Agents run on `codex`, `opencode`, `pi`, `hermes`, `workbuddy`, etc., using locally installed commands and their respective models. Settings show each tool's version, install method, update procedure, session log location, and sandbox configuration.

## MCP

`morula mcp` is a stdio MCP server that lets external agent tools read and write the morula agent roster, and query daily records and long-term memory. Example:

```sh
claude mcp add morula -- /Applications/morula.app/Contents/MacOS/morula mcp
```

`morula mcp --assistant` is read-only, exposing only `list_projects` and `list_agents`.

## Data Directory

```
~/Library/Application Support/morula/
├── morula.db           agent roster, settings, projects, task trees, routines (SQLite)
├── memory.md           long-term memory
├── days/               daily conversation logs and summaries (*.jsonl, *.summary.md)
├── files/              attachments from conversations
├── sessions/           exported session files
├── assistant/          assistant's read-only runtime working directory
├── approval-rules.txt  dangerous-operations checklist
├── plugins/            plugin directory
├── plugins.json        installed plugins
└── plugin-auth.json    plugin credentials
```

Set `MORULA_HOME` to change the data directory location.

## Auto Update

`make release` packages the new version into `dist/channel/`: a zip file and a `latest.json` describing it (version, notes, filename, sha256). The installed morula reads `latest.json`, compares versions, verifies sha256, extracts the update, and waits for restart.

The channel path is baked in at compile time and can be overridden at runtime with `MORULA_UPDATE_FEED`. No signature verification, no admin privileges required.

## Architecture

| Package | Responsibility |
| --- | --- |
| `internal/gui` | Desktop window and HTTP interface |
| `internal/chat` | User–assistant conversation: event stream, daily logs, card interactions |
| `internal/work` | Execution engine: routing, task decomposition, dispatch, review, reporting |
| `internal/harness` | Harness adapter layer: detection, execution, model selection, session management, trace reading |
| `internal/gateway` | Local model gateway, OpenAI-compatible, forwards requests to plugins |
| `internal/chatgpt` | Bridges codex's ChatGPT subscription into the gateway |
| `internal/plugin` | Loads OpenCode-format provider plugins |
| `internal/subs` | Aggregates subscription accounts and quotas |
| `internal/store` | SQLite storage and data migration |
| `internal/journal` | Daily records, summaries, and long-term memory |
| `internal/mcpserver` | MCP server implementation |
| `internal/skills` | Lists locally installed skills |
| `internal/env` | Patches PATH when launched from Finder |
| `internal/update` | Local-channel auto update |

## License

MIT License, see [LICENSE](LICENSE).

`internal/chatgpt` is partially derived from [magpie](https://github.com/yetone/magpie) (MIT License, Copyright (c) 2026 yetone), see [NOTICE](NOTICE).

Icons from [lobe-icons](https://github.com/lobehub/lobe-icons) (MIT License). Fonts: IBM Plex Mono, IBM Plex Sans (SIL OFL 1.1) and Source Serif 4 (SIL OFL 1.1).
