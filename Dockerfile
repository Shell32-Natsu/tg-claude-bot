FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bot .

# The bot shells out to the Claude Code CLI, which needs Node. node:slim
# ships a non-root "node" user; add the small set of system tools the CLI
# expects at runtime (certificates for HTTPS, git for its environment
# checks, curl for diagnostics).
FROM node:22-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git curl \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g @anthropic-ai/claude-code \
    && npm cache clean --force

COPY --from=build /bot /usr/local/bin/bot

# Claude Code stores session state under $HOME/.claude — mount a volume at
# /home/node to persist it. Auto-updates are pointless in a container.
ENV DISABLE_AUTOUPDATER=1
USER node
WORKDIR /home/node
ENTRYPOINT ["bot"]
