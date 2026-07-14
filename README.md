# tg-claude-bot

A Telegram bot that answers messages using the Anthropic Claude API. Single
`main.go`, Go standard library only.

## Behavior

- Uses **long-polling** (`getUpdates`) — no webhook, no inbound port, works
  behind NAT without a public IP.
- **Private chats:** replies to every message.
- **Group chats:** replies only when the bot is `@mentioned` (the mention is
  detected via Telegram message entities and stripped before the text is sent
  to Claude) or when someone replies to one of the bot's own messages.
- Every message is answered independently — the bot keeps **no conversation
  memory**.
- Shows a "typing…" chat action while waiting for Claude.
- Splits replies longer than Telegram's 4096-character limit into multiple
  messages.
- Ignores messages from other bots.
- Has Anthropic's server-side **web search** tool enabled (up to 3 searches
  per message), so the model can verify facts instead of guessing. Searches
  are billed per use by Anthropic.

## Configuration

Environment variables (see `.env.example`):

| Variable             | Required | Default           | Description                          |
| -------------------- | -------- | ----------------- | ------------------------------------ |
| `TELEGRAM_BOT_TOKEN` | yes      | —                 | Bot token from @BotFather            |
| `ANTHROPIC_API_KEY`  | yes      | —                 | Anthropic API key                    |
| `CLAUDE_MODEL`       | no       | `claude-sonnet-5` | Claude model ID                      |
| `SYSTEM_PROMPT`      | no       | (empty)           | System prompt for every request      |

> Note: verify that `CLAUDE_MODEL` is a model ID your Anthropic account can
> use (`GET /v1/models`). If `claude-sonnet-5` returns a `not_found_error`,
> set e.g. `CLAUDE_MODEL=claude-sonnet-4-6`.

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

The container runs as a non-root user on a distroless base image, with a
read-only filesystem and no exposed ports.

### Locally

```sh
export TELEGRAM_BOT_TOKEN=... ANTHROPIC_API_KEY=...
go run .
```

## CI

`.github/workflows/docker.yml` runs `gofmt`/`go vet`/`go build`, validates
`docker-compose.yml`, builds the image, and pushes it to GHCR
(`ghcr.io/<owner>/<repo>`) on pushes to `main` and version tags.
