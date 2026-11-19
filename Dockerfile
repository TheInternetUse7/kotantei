FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie@sha256:f18c3d73a184fb0582828510722e723b63a93f7188d11dd223d9be6f6880e65d AS build

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
ARG REVISION=
ARG SOURCE_URL=

ENV GOTOOLCHAIN=local CGO_ENABLED=0 GOPROXY=https://proxy.golang.org GOFLAGS="-mod=readonly"
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY scripts/ ./scripts/
COPY LICENSE NOTICE ./
RUN GOOS="$TARGETOS" GOARCH="$TARGETARCH" bash scripts/build-binary.sh "$VERSION" "$REVISION" "$SOURCE_URL" /out/kotantei \
    && GOOS="$TARGETOS" GOARCH="$TARGETARCH" bash scripts/collect-licenses.sh /out/licenses \
    && mkdir -p /out/data \
    && chmod 0700 /out/data

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931 AS runtime

ARG VERSION=dev
ARG REVISION=
ARG SOURCE_URL=
ARG SOURCE_REPOSITORY=https://github.com/theinternetuse7/Kotantei
LABEL org.opencontainers.image.title="Kotantei" \
      org.opencontainers.image.description="Public AniList and MyAnimeList updates for Discord" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$REVISION" \
      org.opencontainers.image.source="$SOURCE_REPOSITORY" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      io.kotantei.corresponding-source="$SOURCE_URL"

COPY --from=build /out/kotantei /usr/local/bin/kotantei
COPY --from=build /out/licenses /usr/share/kotantei/licenses
COPY LICENSE NOTICE /usr/share/kotantei/
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
# Resolve the application's default data/ paths inside the persistent /data volume.
WORKDIR /
VOLUME ["/data"]
HEALTHCHECK NONE
ENTRYPOINT ["/usr/local/bin/kotantei"]
CMD ["run"]
