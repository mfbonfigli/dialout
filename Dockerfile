# syntax=docker/dockerfile:1.7
# dialout: one image with the server and cloudflared. Multi-arch: linux/amd64 and linux/arm64.
#   docker buildx build --platform linux/amd64,linux/arm64 -t ghcr.io/mfbonfigli/dialout:dev .

ARG GO_VERSION=1.25

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/dialout ./cmd/dialout \
 && mkdir -p /out/data

# cloudflared, pinned by version and SHA-256 (from the release notes of that version).
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS cloudflared
ARG TARGETARCH
ARG CLOUDFLARED_VERSION=2026.10.3
ARG CLOUDFLARED_SHA256_amd64=053b80b0a23031a038fdbf3c073f59f82b8f1f152aa67971d7383ffafc0dbab0
ARG CLOUDFLARED_SHA256_arm64=89297ebfb9e8c99c0dc013a1140601453cf3798d8306c5591651f4a3f5210be8
ADD --chmod=755 https://github.com/cloudflare/cloudflared/releases/download/${CLOUDFLARED_VERSION}/cloudflared-linux-${TARGETARCH} /cloudflared
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) sum="$CLOUDFLARED_SHA256_amd64" ;; \
      arm64) sum="$CLOUDFLARED_SHA256_arm64" ;; \
      *) echo "unsupported architecture $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    echo "$sum  /cloudflared" | sha256sum -c -

# No shell, no package manager: the two static binaries and CA certificates.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/dialout /usr/local/bin/dialout
COPY --from=cloudflared /cloudflared /usr/local/bin/cloudflared
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV DIALOUT_CONFIG=/config/dialout.yaml \
    DIALOUT_DATA=/data
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/dialout", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/dialout"]
CMD ["serve"]
