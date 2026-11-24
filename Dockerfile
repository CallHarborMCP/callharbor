# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM node:26.13.0-bookworm@sha256:a546d3f8acd2af4382668a7e6acd3bc4418e6a27478373e3246f8162f5181509 AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build && test -f dist/index.html

FROM golang:1.27.4-bookworm AS build
WORKDIR /src
COPY . .
COPY --from=web /src/web/dist/ ./web/static/
RUN test -f web/static/index.html \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/callharbor ./cmd/callharbor \
    && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
ENV CALLHARBOR_LISTEN_ADDR=:8080 \
    CALLHARBOR_DATA_DIR=/data
COPY --from=build /out/callharbor /usr/local/bin/callharbor
COPY --from=build --chown=nonroot:nonroot /out/data /data
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/callharbor"]
