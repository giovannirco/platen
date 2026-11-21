# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/platen ./cmd/platen
# The final image has no shell. Create /data here so the nonroot user (65532)
# owns it before VOLUME initialises a named volume. Otherwise Compose with
# read_only: true can mount a root-owned volume the process cannot write.
# .keep makes the directory copyable; COPY skips an empty directory.
RUN mkdir -p /data && touch /data/.keep && chown -R 65532:65532 /data

# The binary is static and needs nothing else: no CUPS, no SANE, no Ghostscript.
# distroless static nonroot, resolved 2026-11-03.
# Listen on every interface inside the container so a published port works.
# That address is not loopback, so the process refuses to start without a token
# or a trusted network. Publish the host port on 127.0.0.1 unless you mean to
# expose it. See deploy/compose.yaml.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/platen /usr/local/bin/platen
COPY --from=build --chown=65532:65532 /data /data
ENV PLATEN_LISTEN=":8080" PLATEN_DATA_DIR="/data"
VOLUME /data
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/usr/local/bin/platen"]
CMD ["serve"]
