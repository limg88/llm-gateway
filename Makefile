VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test fmt docker clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o llm-gateway .

test:
	go vet ./...
	go test -race -count=1 ./...

fmt:
	gofmt -l -w .

docker:
	docker build --build-arg VERSION=$(VERSION) -t llm-gateway:$(VERSION) .

clean:
	rm -f llm-gateway
