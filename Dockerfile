# amuxify container: static binary plus the tools it drives.
# Run as the uid that owns your library, never as root:
#   docker run --rm -u 1000:1000 -v /srv/media:/data ghcr.io/nxame/amuxify scan /data
FROM golang:1.27-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/nxame/amuxify/internal/cli.Version=${VERSION}" -o /out/amuxify ./cmd/amuxify

FROM alpine:3.21
RUN apk add --no-cache ffmpeg mkvtoolnix exiftool tini
COPY --from=build /out/amuxify /usr/local/bin/amuxify
ENV XDG_STATE_HOME=/state
RUN mkdir -p /state /data && chmod 1777 /state
WORKDIR /data
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/amuxify"]
CMD ["doctor"]
