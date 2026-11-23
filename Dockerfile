# syntax=docker/dockerfile:1
FROM golang:1.27.3-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/callharbor ./cmd/callharbor

FROM gcr.io/distroless/static-debian12:nonroot
ENV CALLHARBOR_LISTEN_ADDR=:8080 \
    CALLHARBOR_DATA_DIR=/data
COPY --from=build /out/callharbor /usr/local/bin/callharbor
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/callharbor"]
