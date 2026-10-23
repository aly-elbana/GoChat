BIN      := bin
ADDR     ?= localhost:9090
NAME     ?=
VERSION  := $(shell grep -o 'Version = "[^"]*"' internal/protocol/protocol.go | cut -d'"' -f2)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: all build server client test race vet fmt demo dist clean

all: vet test build

build:
	go build -o $(BIN)/gochat-server ./cmd/server
	go build -o $(BIN)/gochat-client ./cmd/client

server: ## run the server
	go run ./cmd/server -addr $(ADDR)

client: ## run a client: make client NAME=alice
	go run ./cmd/client -server $(ADDR) $(if $(NAME),-name $(NAME))

test:
	go test ./...

race:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

demo: ## 4-pane tmux demo (server + 3 clients)
	./scripts/demo.sh

dist: ## cross-compile release binaries into dist/
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		for cmd in server client; do \
			GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="-s -w" \
				-o dist/gochat-$$cmd-$(VERSION)-$$os-$$arch$$ext ./cmd/$$cmd || exit 1; \
		done; echo "built $$os/$$arch"; \
	done

clean:
	rm -rf $(BIN) dist
