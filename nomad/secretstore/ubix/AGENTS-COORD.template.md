# AGENTS-COORD.template.md — multi-agent coordination (committed template)

> **This is the committed template.** The LIVE file is the **untracked**
> `AGENTS-COORD.md` at the repo root — per-sandbox session state, never pushed.
> Committing live coordination state causes cross-lane merge races and spliced log
> entries (decision ported from `ubixcore`). On a fresh sandbox:
>
> ```bash
> cp AGENTS-COORD.template.md AGENTS-COORD.md
> ```

More than one agent session may work this repo, and agents working in *sibling*
repos (notably `kubernetes`, which runs uBixVault in production) may need to hand
work over. This file is where lanes get registered and where cross-repo requests
get answered.

**Agent sessions cannot notify each other.** Nothing here pushes. This file works
because every agent on this machine shares one `~/git`, so it is a mailbox that
both sides *poll*. Write replies assuming the reader arrives days later with no
memory of the conversation.

## New agent — start here

1. Add a row to [§1](#1-lanes) with your role and a **distinct** branch prefix.
2. Append to [§3](#3-log-append-only-newest-at-bottom): your lane, the paths you
   will touch, and anything you are claiming.
3. Work inside your paths. Claim shared files in §3 before editing them.
4. Never rewrite another agent's entry. Append.

## 1. Lanes

| Agent | Scope | Branch prefix |
| ----- | ----- | ------------- |
| *(none registered)* | | |

## 2. Shared files — claim in §3 before editing

`README.md`, `CHANGELOG.md`, `docs/ROADMAP.md`, `docs/DECISIONS.md`,
`deploy/charts/ubixvault/*`, and anything under `internal/core/`.

Branches go through GitHub pull requests; `main` is not written to directly.
Prefixes in use: `fix/*`, `docs/*`, `ci/*`.

## 3. Log — append only, newest at bottom

<!-- Format: date · lane · what you are doing / replying -->

## 4. Cross-repo requests

Requests from sibling repos land here. A request is **not done** until the
requesting side has been answered in §3 — the asker is polling, not listening.

| Date | From | Request | Status |
| ---- | ---- | ------- | ------ |
| *(none)* | | | |
