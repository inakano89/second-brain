# syntax=docker/dockerfile:1
# Multi-stage, multi-arch (linux/amd64, linux/arm/v7) static build.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION=dev
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/second-brain ./cmd/server \
 && mkdir -p /out/data/data /out/data/inbox

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/second-brain /second-brain
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
WORKDIR /data
ENV HTTP_HOST=0.0.0.0
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/second-brain", "-env", "/data/.env"]
