# syntax=docker/dockerfile:1
FROM golang:1.27.4-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/callharbor ./cmd/callharbor \
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
