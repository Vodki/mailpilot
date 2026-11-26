# syntax=docker/dockerfile:1

# --- Build the Go binary (static, no CGO) -----------------------------------
ARG GO_VERSION=1.27.4
FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/mailpilot ./cmd/mailpilot

# --- Fetch a pinned Claude Code release (native binary, needs glibc) --------
# The Go image already ships curl, bash and CA certificates.
FROM golang:${GO_VERSION}-bookworm AS claude
ARG CLAUDE_CODE_VERSION=2.1.332
RUN curl -fsSL https://claude.ai/install.sh -o /tmp/install.sh \
 && bash /tmp/install.sh "${CLAUDE_CODE_VERSION}" \
 && mkdir -p /out \
 && cp -L "$HOME/.local/bin/claude" /out/claude \
 && /out/claude --version

# --- Runtime: distroless with glibc, non-root --------------------------------
FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=build  /out/mailpilot /usr/local/bin/mailpilot
COPY --from=claude /out/claude    /usr/local/bin/claude
ENV MAILPILOT_CONFIG=/config/config.yaml \
    HOME=/home/nonroot \
    DISABLE_AUTOUPDATER=1
USER nonroot:nonroot
VOLUME ["/data", "/config"]
ENTRYPOINT ["/usr/local/bin/mailpilot"]
CMD ["run"]
