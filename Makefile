.PHONY: setup start stop status uninstall build test

setup:
	./setup.sh

start:
	./start.sh

stop:
	./stop.sh

status:
	./status.sh

uninstall:
	./uninstall.sh

build:
	go build -o conduit ./cmd/gateway

test:
	go test ./...
