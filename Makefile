PLATFORM=linux/amd64
BINARIES := proxyd
IMAGE=market-proxy
TAG=latest
PKG_REPO=ghcr.io/fox-foundation
# MAKEFLAGS += --no-print-directory

.PHONY: clean build $(BINARIES)

build:
	go build ./...

clean:
	go clean ./...

test:
	go test ./...

test-unit:
	go test -v -short ./...

run-api: build
	go run cmd/proxyd/main.go

lint:
	@./scripts/lint.sh

install:
	go install ./cmd/...

docker-build:
	@docker build --no-cache --platform=${PLATFORM} . --file Dockerfile -t ${IMAGE}:${TAG}

docker-tag:
	@docker tag ${IMAGE}:${TAG} ${PKG_REPO}/${IMAGE}:${TAG}

docker-push:
	@docker push ${PKG_REPO}/${IMAGE}:${TAG}

push-image: docker-build docker-tag docker-push
