# The build stage always runs on the build machine's own architecture and
# cross-compiles, so a multi-platform image needs no emulation.
# Base images are pinned by digest, so the image that is published is the one
# that was tested; see docs/releasing.md for how to update them.
FROM --platform=$BUILDPLATFORM golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/hookinlet ./cmd/hookinlet
# Pre-create the data directory so it is owned by the non-root user.
RUN mkdir /out-data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/hookinlet /hookinlet
# The licenses of hookinlet and of everything in the binary, as their terms require.
COPY LICENSE NOTICE THIRD_PARTY_LICENSES /licenses/
COPY --from=build --chown=65532:65532 /out-data /data
ENV HOOKINLET_LISTEN_ADDR=:8080 \
    HOOKINLET_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
# Ready, not merely alive: a server that has not finished migrating, or cannot open its
# secrets, should not be sent traffic. The start period is for a long migration.
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 CMD ["/hookinlet", "healthcheck"]
USER 65532:65532
ENTRYPOINT ["/hookinlet"]
CMD ["serve"]
