# Bangle in a container. Publish the port on the host's loopback only:
#
#   docker run -d --name bangle -p 127.0.0.1:7060:7060 \
#     -e SECRETS_KEY="$(openssl rand -base64 32)" -v bangle-data:/data \
#     ghcr.io/sharat87/bangle
#
# Keep SECRETS_KEY somewhere: losing it means logging in to every server
# again. Bangle has no logins of its own, so a port published on anything but
# 127.0.0.1 hands every stored login to whoever can reach it.

# The frontend and the binary are built on the build machine's own platform
# ($BUILDPLATFORM), never emulated: the bundle is platform-independent, and
# Go cross-compiles (pure Go, CGO off). Only the runtime stage below runs as
# the target platform, and its one RUN is quick under emulation.

# The frontend bundle, embedded by the Go build below.
FROM --platform=$BUILDPLATFORM node:24-bookworm-slim AS frontend
WORKDIR /src
RUN corepack enable && corepack prepare pnpm@10.30.0 --activate
COPY package.json pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY build.mjs ./
COPY frontend ./frontend
RUN node build.mjs

FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /src/frontend/dist ./frontend/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/bangle ./cmd/server

# The runtime is the static binary and CA certificates (every upstream is
# https), running as an unprivileged user with the DB in /data.
FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
	&& adduser -D -H -u 1000 bangle \
	&& mkdir /data && chown bangle:bangle /data
COPY --from=build /out/bangle /usr/local/bin/bangle
USER bangle
WORKDIR /data
VOLUME /data
ENV DB_PATH=/data/bangle.db \
	BASE_URL=http://127.0.0.1:7060
EXPOSE 7060
# Every interface, since the container's loopback isn't the host's; the
# warning it logs is the reason for publishing on 127.0.0.1 only.
CMD ["bangle", "-listen", "0.0.0.0:7060"]
