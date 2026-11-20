FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie@sha256:f18c3d73a184fb0582828510722e723b63a93f7188d11dd223d9be6f6880e65d AS build

# Inherit BuildKit's per-platform values; defaults would override them.
ARG TARGETOS
ARG TARGETARCH
ARG TARGETPLATFORM
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

# Check the executable itself against Docker's target, not the compiler arguments.
RUN set -eu; \
    case "$TARGETPLATFORM" in \
        linux/amd64) expected_machine=3e00 ;; \
        linux/arm64|linux/arm64/v8) expected_machine=b700 ;; \
        *) printf 'Unsupported target platform: %s\n' "$TARGETPLATFORM" >&2; exit 1 ;; \
    esac; \
    actual_header="$(od -An -tx1 -N6 /out/kotantei | tr -d ' \n')"; \
    actual_machine="$(od -An -tx1 -j18 -N2 /out/kotantei | tr -d ' \n')"; \
    if [ "$actual_header" != 7f454c460201 ] || [ "$actual_machine" != "$expected_machine" ]; then \
        printf 'Built executable does not match %s (ELF header %s, machine %s).\n' "$TARGETPLATFORM" "$actual_header" "$actual_machine" >&2; \
        exit 1; \
    fi

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
# Fixed Docker storage locations override storage fields in older JSON configs.
ENV KOTANTEI_DATABASE=/data/kotantei.db KOTANTEI_LEGACY_DATABASE=/setup/data.db
USER 65532:65532
WORKDIR /
VOLUME ["/data"]
HEALTHCHECK NONE
ENTRYPOINT ["/usr/local/bin/kotantei", "--config", "/setup/config.jsonc"]
CMD ["run"]
