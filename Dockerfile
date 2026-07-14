FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bot .

# distroless/static ships CA certificates and a nonroot user; the bot needs
# nothing else (no shell, no libc, no writable filesystem).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /bot /bot
USER nonroot:nonroot
ENTRYPOINT ["/bot"]
