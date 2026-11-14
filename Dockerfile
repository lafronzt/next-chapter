# Pinned by digest (the multi-arch index for golang:1.27-alpine). Dependabot's
# docker ecosystem proposes digest updates; see CONTRIBUTING.md to bump by hand.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS=linux
ARG TARGETARCH
# VERSION is the release shown in the app and by "next-chapter version". The
# publish workflow passes the tag without its "v"; CI passes git describe.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X github.com/lafronzt/next-chapter/internal/app.Version=${VERSION}" -o /next-chapter ./cmd/next-chapter \
    && mkdir /data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=10001:10001 /data /data
COPY --from=build /next-chapter /next-chapter
ENV DATABASE_PATH=/data/next-chapter.sqlite3
USER 10001:10001
EXPOSE 8000
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD ["/next-chapter", "healthcheck"]
ENTRYPOINT ["/next-chapter"]
