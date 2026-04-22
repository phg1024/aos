GOCACHE ?= $(CURDIR)/.cache/go-build
GOTMPDIR ?= $(CURDIR)/.cache/tmp

test:
	mkdir -p $(GOCACHE) $(GOTMPDIR)
	GOCACHE=$(GOCACHE) GOTMPDIR=$(GOTMPDIR) go test ./...

test-unit:
	mkdir -p $(GOCACHE) $(GOTMPDIR)
	GOCACHE=$(GOCACHE) GOTMPDIR=$(GOTMPDIR) go test ./...

fmt:
	gofmt -w ./cmd ./internal

report:
	@echo "HTML reports live in reports/"
