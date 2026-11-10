# The build stage runs on the builder's own platform and cross-compiles, so a
# multi-arch image needs no emulation for the Go build.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH
# The release workflow passes all three; .git is not in the build context,
# so without them `habakkuk version` prints "unknown" for commit and date.
ARG VERSION=dev
ARG COMMIT=
ARG DATE=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o /out/habakkuk ./cmd/habakkuk

FROM debian:trixie-slim AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates bash git curl && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/habakkuk /usr/local/bin/habakkuk
RUN ln -s habakkuk /usr/local/bin/hk
ENV HABAKKUK_HOME=/data
VOLUME /data
ENTRYPOINT ["habakkuk"]
CMD ["serve"]

# Optional: the same image plus Chromium for the browser toolset (about 1 GB
# more, unpacked). Build it with `docker build --target browser -t habakkuk:browser .`
FROM runtime AS browser
RUN apt-get update && apt-get install -y --no-install-recommends chromium fonts-liberation fonts-noto-color-emoji \
    && rm -rf /var/lib/apt/lists/*

# The default target: no browser.
FROM runtime
