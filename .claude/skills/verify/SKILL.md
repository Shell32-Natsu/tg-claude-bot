---
name: verify
description: Verify tg-claude-bot end-to-end with a local fake Telegram Bot API server and a real (logged-in) claude CLI. Use before committing changes to the bot.
---

# Verifying tg-claude-bot

The bot has two external surfaces: the Telegram Bot API (long-polling) and
the `claude` CLI. Telegram is stubbed with a local fake server; claude runs
for real, so the machine must have a logged-in `claude` (or
`CLAUDE_CODE_OAUTH_TOKEN` set).

## Build

```sh
go build -o /tmp/tgbot .
```

## Fake Telegram server

Write a small Go HTTP server on `127.0.0.1:18081` implementing
`/bottest-token/getMe`, `/getUpdates` (long-poll: serve scripted updates by
`update_id >= offset`, block ~25s when empty), `/sendChatAction`, and
`/sendMessage` (record the reply texts). Script a group chat (`chat.id`
negative, `type: supergroup`) whose messages mention `@testbot` via an
`entities` array (`{"type":"mention","offset":0,"length":8}`).

A proven scenario (drives session create → resume → idle expiry → fresh
context):
1. alice: "@testbot My favorite color is teal." → reply must acknowledge.
2. bob: "@testbot What is my favorite color?" → expect "Unknown"-ish: the
   session HAS the context but bob ≠ alice (speaker labels work). The
   transcript under `~/.claude/projects/-tmp-tg-claude-bot-chat-*/<sid>.jsonl`
   proves the resume carried context.
3. Wait ~90s (with `SESSION_IDLE_MINUTES=1`), then alice: "@testbot What is
   my favorite color? If you do not know, say NOIDEA." → expect NOIDEA
   (fresh session), a new session id in the bot log, and the old session's
   workdir under `/tmp/tg-claude-bot/` plus its project dir deleted.

## Run the bot against it

```sh
TELEGRAM_BOT_TOKEN=test-token \
TELEGRAM_API_BASE=http://127.0.0.1:18081 \
SESSION_IDLE_MINUTES=1 \
SYSTEM_PROMPT="Keep replies very short." \
/tmp/tgbot
```

Watch the bot log for `session … created`, `session … expired (idle > 1m0s)`.

## Worthwhile probes

- Message text `@testbot --version`: must be answered as chat (stdin
  delivery means it can never be parsed as a CLI flag).
- Run the bot with `ANTHROPIC_API_KEY=garbage` in its env: replies must
  still succeed (the key is stripped; subscription auth is used).
- Tool lockdown: `echo 'read /some/file or list your tools' | claude -p
  --tools WebSearch --allowedTools WebSearch --strict-mcp-config` must
  report only WebSearch available (WebFetch is deliberately excluded — it
  would be an SSRF vector from untrusted chat input).

## Gotchas

- The sandbox blocks api.anthropic.com and home-directory writes — run the
  bot and claude outside the sandbox.
- `ls` on an empty dir exits 0; don't use `ls path/*/ && echo non-empty`.
- Clean up test state afterwards: `/tmp/tg-claude-bot`,
  `~/.claude/projects/-tmp-tg-claude-bot-chat-*`.
