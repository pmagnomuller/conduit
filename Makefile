.PHONY: setup start stop status uninstall build conduitctl test macos-app

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

# conduitctl is the control client (cmd/conduit). It builds to its own name so
# the gateway artifact path that setup.sh and the launchd plist exec never moves.
conduitctl:
	go build -o conduitctl ./cmd/conduit

test:
	go test ./...

macos-app:
	macos/ConduitBar/build-app.sh
