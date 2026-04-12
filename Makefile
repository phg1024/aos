test:
	go test ./...

test-unit:
	go test ./...

fmt:
	gofmt -w ./cmd ./internal

report:
	@echo "HTML reports live in reports/"
