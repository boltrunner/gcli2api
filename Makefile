
test:
	go test ./...

build:
	go build -o dist/gcli2api main.go

fmt:
	go fmt ./...

run:
	go run main.go server

.PHONY: test build fmt run
