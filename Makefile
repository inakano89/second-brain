BINARY  := second-brain
PKG     := ./cmd/server
DIST    := dist
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Chave pública ed25519 (base64) embutida para exigir releases assinadas no auto-update.
UPDATE_PUBLIC_KEY ?=
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.updatePublicKey=$(UPDATE_PUBLIC_KEY)
GOFLAGS := -trimpath
IMAGE   ?= ghcr.io/inakano89/second-brain

export CGO_ENABLED=0

.PHONY: all build run test vet fmt check tidy clean release sign \
        build-linux-amd64 build-linux-armv7 build-windows-amd64 \
        docker docker-buildx

all: check release

## build: binário para a plataforma atual
build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY) $(PKG)

## run: executa localmente (usa ./.env)
run:
	go run $(PKG) -env .env

## release: cross-compile para os 3 alvos (assets embutidos via embed.FS)
release: build-linux-amd64 build-linux-armv7 build-windows-amd64
	cd $(DIST) && sha256sum $(BINARY)-* > SHA256SUMS

## sign: assina dist/SHA256SUMS com UPDATE_SIGNING_KEY (gera dist/SHA256SUMS.sig)
sign:
	go run ./cmd/signer sign $(DIST)/SHA256SUMS

build-linux-amd64:
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-amd64 $(PKG)

build-linux-armv7:
	GOOS=linux GOARCH=arm GOARM=7 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-armv7 $(PKG)

build-windows-amd64:
	GOOS=windows GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-windows-amd64.exe $(PKG)

## check: verifica que os 3 alvos compilam sem gerar artefatos
check: vet test
	GOOS=linux   GOARCH=amd64         go build $(GOFLAGS) -o /dev/null $(PKG)
	GOOS=linux   GOARCH=arm GOARM=7   go build $(GOFLAGS) -o /dev/null $(PKG)
	GOOS=windows GOARCH=amd64         go build $(GOFLAGS) -o /dev/null $(PKG)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w ./cmd ./internal

tidy:
	go mod tidy

## docker: imagem local (arquitetura do host)
docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

## docker-buildx: imagem multi-arch (amd64 + arm/v7) e push
docker-buildx:
	docker buildx build --platform linux/amd64,linux/arm/v7 --build-arg VERSION=$(VERSION) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):latest --push .

clean:
	rm -rf $(DIST)
