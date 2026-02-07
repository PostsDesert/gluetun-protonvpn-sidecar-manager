# Stage 1: Build the Manager
FROM golang:1.25.7-alpine AS builder

WORKDIR /app
# Install git for fetching dependencies and upx for compression
# We also install docker-cli and docker-cli-compose here to copy them later
# This avoids pulling from external images that might flake
RUN apk add --no-cache git upx docker-cli docker-cli-compose

COPY go-manager /app/go-manager
WORKDIR /app/go-manager

RUN go mod download && go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/manager main.go

# Compress binaries
# We copy the docker binaries to a temp location to compress them safely
# Use --lzma for good ratio, but avoid --best to keep build time reasonable
RUN cp /usr/bin/docker /tmp/docker && \
    cp /usr/libexec/docker/cli-plugins/docker-compose /tmp/docker-compose && \
    upx --lzma /app/manager && \
    upx --lzma /tmp/docker && \
    upx --lzma /tmp/docker-compose

# Stage 2: Final Runtime Image
FROM alpine:3.19

# Install minimal runtime dependencies (only CA certs needed)
# docker-cli-compose is a plugin, so we need to place it correctly
RUN apk add --no-cache ca-certificates

WORKDIR /app

# Copy compressed binaries
COPY --from=builder /app/manager /app/manager
COPY --from=builder /tmp/docker /usr/local/bin/docker
# Docker Compose v2 expects the plugin at this location
COPY --from=builder /tmp/docker-compose /usr/local/lib/docker/cli-plugins/docker-compose

# Create symlink for convenience if user wants to run `docker-compose` directly
RUN ln -s /usr/local/lib/docker/cli-plugins/docker-compose /usr/local/bin/docker-compose

# Run command
CMD ["/app/manager"]
