# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/platen ./cmd/platen

# The binary is static and needs nothing else: no CUPS, no SANE, no Ghostscript.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/platen /usr/local/bin/platen
ENV PLATEN_LISTEN=":8080" PLATEN_DATA_DIR="/data"
VOLUME /data
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/usr/local/bin/platen"]
CMD ["serve"]
