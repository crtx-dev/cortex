# Cortex {{VERSION}}

Cortex {{VERSION}} is a {{RELEASE_KIND}}.

## What this release is

- One self-hosted Go executable with an embedded dashboard and SQLite-backed
  conversations.
- A focused browser coding agent for autonomous software development: parallel
  workspaces, durable server-side conversations, first-run authentication and
  explicit credential isolation.
- First-class OpenCode Go provider support alongside OpenCode Zen, using
  OpenCode's native provider routing.
- Checksum-verified release archives for Linux, macOS and Windows on amd64 and
  arm64, plus `checksums.txt`.

## Operator responsibilities

- Cortex expects OpenCode to be installed separately and available in PATH.
- Back up before upgrading.
- Read the security contract before relying on Cortex in production.
- Cortex is a public preview for evaluation and early self-hosting; it is not
  claimed to be production-proven or battle-proven.

## Installation

- https://crtx.dev/install.sh (per-user)
- https://crtx.dev/download.sh (download the archive)
- https://crtx.dev/update.sh (upgrade an existing install)