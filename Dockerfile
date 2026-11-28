# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM node:26.13.1-bookworm@sha256:893ea48106e988a5f91e69629d6472600daa9d0494adf7db87eb0b886ce82255 AS web
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
