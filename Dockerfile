FROM golang:1.27.2 AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/hookinlet ./cmd/hookinlet
# Pre-create the data directory so it is owned by the non-root user.
RUN mkdir /out-data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/hookinlet /hookinlet
COPY --from=build --chown=65532:65532 /out-data /data
ENV HOOKINLET_LISTEN_ADDR=:8080 \
    HOOKINLET_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/hookinlet"]
CMD ["serve"]
