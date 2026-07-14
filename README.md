# tg-claude-bot

A Telegram bot that answers messages by driving the **Claude Code CLI**,
authenticated with a Claude **Max/Pro subscription** (OAuth) — no API key
needed. Single `main.go`, Go standard library only (plus the `claude`
binary at runtime).

## Behavior

- Uses **long-polling** (`getUpdates`) — no webhook, no inbound port, works
  behind NAT without a public IP.
- **Access control (fail-closed):** only messages from `ALLOWED_USERS` or in
  `ALLOWED_CHATS` are answered by Claude. A blocked message gets a short
  reply with the sender's user ID (and, in groups, the chat ID) so getting
  allowlisted is self-service — rate-limited per chat with exponential
  backoff (1 min doubling to 6 h), so if several people are denied at once
  only the first gets the reply — and is also logged. The bot refuses to
  start with no allowlist unless `ALLOW_ALL=true` is set explicitly.
- **Private chats:** replies to every message (from an allowed user).
- **Group chats:** replies only when the bot is `@mentioned` (the mention is
  detected via Telegram message entities and stripped before the text is sent
  to Claude) or when someone replies to one of the bot's own messages.
- **One Claude Code session per chat**, so conversations have memory. In
  groups, each message is labeled with the sender's name so Claude can tell
  participants apart in the shared session.
- Sessions **idle for more than 1 hour are cleaned up** (session forgotten and
  its transcript deleted); the next message starts a fresh conversation.
- Even a chat that never goes idle gets a **fresh session once a week**, so
  context doesn't accumulate forever.
- Claude's tool access is restricted to **web search** only (which runs
  server-side at Anthropic) — no shell, no file access, no URL fetching from
  the bot's network, no MCP servers — since chat messages are untrusted
  input.
- Each session runs in its own **empty temporary directory** (under the OS
  temp dir), deleted when the session expires and cleared on bot restart.
- Reacts to a message it's about to answer with 👀 (configurable via
  `REACTION_EMOJI`, must be from Telegram's reaction set; `none` disables),
  then shows a "typing…" chat action while waiting for Claude.
- Renders Claude's Markdown in Telegram: replies are converted to Telegram
  HTML (bold, strikethrough, inline code, code blocks, links, bullets;
  headers become bold), with a plain-text fallback if Telegram rejects the
  formatting. Claude is steered toward Telegram-friendly formatting via a
  built-in system prompt (prepended to your `SYSTEM_PROMPT`).
- Splits long replies into multiple messages, well below Telegram's
  4096-character limit to leave room for formatting (capped at 4 messages
  per answer to prevent chat flooding; code blocks cut by a split are
  closed and reopened so each message renders correctly).
- Ignores messages from other bots.
- Verifies Claude Code authentication at startup (`claude auth status`) and
  exits with a clear error instead of failing message-by-message.

## Configuration

Environment variables (see `.env.example`):

| Variable                  | Required   | Default  | Description                                            |
| ------------------------- | ---------- | -------- | ------------------------------------------------------ |
| `TELEGRAM_BOT_TOKEN`      | yes        | —        | Bot token from @BotFather                              |
| `ALLOWED_USERS`           | see below  | (empty)  | Comma-separated user IDs or @usernames allowed anywhere (incl. private chat) |
| `ALLOWED_CHATS`           | see below  | (empty)  | Comma-separated group/channel IDs or @usernames; anyone in an allowed chat may use the bot there (not consulted for private chats) |
| `ALLOW_ALL`               | see below  | (empty)  | `true` opens the bot to everyone (explicit opt-out)    |
| `CLAUDE_CODE_OAUTH_TOKEN` | in Docker  | —        | Subscription token from `claude setup-token`           |
| `CLAUDE_MODEL`            | no         | (empty)  | Model alias (`sonnet`, `opus`) or full model ID        |
| `SYSTEM_PROMPT`           | no         | (empty)  | Appended to Claude Code's default system prompt        |
| `REACTION_EMOJI`          | no         | `👀`     | Ack reaction on answered messages (`none` disables)    |
| `SESSION_IDLE_MINUTES`    | no         | `60`     | Drop a chat's session after this much inactivity       |
| `SESSION_MAX_AGE_DAYS`    | no         | `7`      | Restart a chat's session (fresh context) after this    |
| `TELEGRAM_API_BASE`       | no         | `https://api.telegram.org` | For self-hosted Bot API servers      |
| `CLAUDE_BIN`              | no         | `claude` | Path to the Claude Code binary                         |

To get `CLAUDE_CODE_OAUTH_TOKEN`, run `claude setup-token` on any machine
where Claude Code is logged in with your subscription account, and paste the
resulting token into `.env`. When running the bot locally instead of in
Docker, you can skip the token entirely if you have already run
`claude login` — the CLI will use your stored credentials.

At least one of `ALLOWED_USERS` / `ALLOWED_CHATS` must be set (or
`ALLOW_ALL=true`); otherwise the bot exits at startup. To find an ID, just
message the bot (mention it in groups) while not yet allowlisted — the
access-denied reply contains your user ID (plus the chat's ID in groups; in
private chat they're the same number), and the same IDs appear in the bot's
log. Group/channel IDs are negative (the bot rejects
entries whose sign doesn't match the list at startup). Prefer numeric IDs —
usernames can be changed or reassigned, and a freed chat @username could be
claimed by someone else. Note that allowing a chat allows *every member* of
that chat, so think twice before allowlisting a public group.

For group mentions to work in all messages, either mention the bot explicitly
or disable BotFather's *Group Privacy* mode so the bot can see group messages
that don't mention it (needed for the reply-to-bot trigger).

## Run

### Docker Compose

```sh
cp .env.example .env   # fill in your tokens
docker compose up -d   # pulls ghcr.io/shell32-natsu/tg-claude-bot:main
```

To build the image locally instead, uncomment `build: .` in
`docker-compose.yml` and run `docker compose up -d --build`.

The container runs as a non-root user with a read-only root filesystem and no
exposed ports; the only writable paths are `/home/node` (a named volume for
Claude Code's session state) and a tmpfs `/tmp`.

### Locally

Requires the [Claude Code CLI](https://code.claude.com/docs) on `PATH`,
logged in via `claude login` (or `CLAUDE_CODE_OAUTH_TOKEN` set):

```sh
export TELEGRAM_BOT_TOKEN=...
go run .
```

## Notes

- Session transcripts live under `~/.claude/projects/`; the bot deletes a
  session's transcript when it expires it after an hour of inactivity.
- `ANTHROPIC_API_KEY` is deliberately stripped from the CLI's environment so
  replies always bill to the subscription, never to an API key.

## CI

`.github/workflows/docker.yml` runs `gofmt`/`go vet`/`go build`, validates
`docker-compose.yml`, builds the image, and pushes it to GHCR
(`ghcr.io/<owner>/<repo>`) on pushes to `main` and version tags.
