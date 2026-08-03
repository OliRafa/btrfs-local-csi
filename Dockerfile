FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/btrfs-local-csi ./cmd/btrfs-local-csi

FROM alpine:3.22
# btrfs-progs and util-linux are baked in deliberately: the driver must come up
# at cluster bootstrap without reaching the network for a package install.
RUN apk add --no-cache btrfs-progs util-linux
COPY --from=build /out/btrfs-local-csi /usr/local/bin/btrfs-local-csi
ENTRYPOINT ["/usr/local/bin/btrfs-local-csi"]
