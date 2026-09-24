BIN := bin/loopmath-agent
VERSION ?= 0.2.0

.PHONY: build test docker run clean
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/GreatPyreneseDad/loopmath-agent/internal/loop.Version=$(VERSION)" -o $(BIN) ./cmd/loopmath-agent
test:
	go vet ./... && go test -race -count=1 ./...
docker:
	docker build -t loopmath-agent:$(VERSION) .
run: build
	./$(BIN)
clean:
	rm -rf bin
