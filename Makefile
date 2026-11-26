# mailpilot — cibles de développement.
GO        ?= go
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)
BIN       := bin/mailpilot
IMAGE     ?= mailpilot:$(VERSION)
COMPOSE_TEST := docker compose -f deploy/docker-compose.test.yml

.PHONY: build test test-integration lint run docker test-env-up test-env-down check-test clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/mailpilot

test:
	$(GO) test -race ./...

# Démarre GreenMail, lance les tests d'intégration, puis l'arrête.
test-integration: test-env-up
	$(GO) test -race -tags integration ./... ; status=$$?; $(MAKE) test-env-down; exit $$status

lint:
	golangci-lint run ./...

run: build
	$(BIN) run -config $${MAILPILOT_CONFIG:-config.example/config.yaml}

docker:
	docker build -f deploy/Dockerfile --build-arg VERSION=$(VERSION) -t $(IMAGE) .

test-env-up:
	$(COMPOSE_TEST) up -d

test-env-down:
	$(COMPOSE_TEST) down -v

# `mailpilot check` contre le serveur de test (critère de la phase 0).
check-test: build test-env-up
	MAILPILOT_IMAP_PASSWORD=secret $(BIN) check -config deploy/test/config.yaml -only store,imap,smtp

clean:
	$(GO) clean
	rm -r -f ./bin ./tmp
