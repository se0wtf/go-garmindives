ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-trixie AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
COPY internal/ ./internal/
COPY docs/ ./docs/
# SQLite and PostgreSQL drivers are pure Go; no SQL client libraries are needed.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/go-garmindives .

FROM alpine:3.24 AS runtime

# ffmpeg includes ffprobe; libjxl-tools supplies cjxl and djxl and their libraries.
# Alpine FFmpeg includes libx264/AAC and zscale/tonemap for SDR/HDR video conversion.
# libheif-tools plus libde265 reconstruct complete HEIC photos instead of HEVC tiles.
RUN apk add --no-cache \
        ca-certificates \
        ffmpeg \
        libjxl-tools \
        tzdata \
    && addgroup -S -g 10001 garmindives \
    && adduser -S -D -H -u 10001 -G garmindives \
        -h /data -s /sbin/nologin garmindives \
    && mkdir -p /data /tokens \
    && chown 10001:10001 /data /tokens

# Keep optional-format decoders separate so changes reuse the larger media layer.
RUN apk add --no-cache libheif-tools libheif-libde265 exiftool

COPY --from=build /out/go-garmindives /usr/local/bin/go-garmindives

ENV HOME=/data \
    GARMIN_CONFIG=/data/config.yaml \
    GARMIN_API_ADDR=0.0.0.0:8080 \
    XDG_CONFIG_HOME=/tokens

WORKDIR /data
USER 10001:10001
EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["go-garmindives"]
CMD ["serve"]
