VERSION ?= dev

BLDFLAGS_LINUX := -X main.version=$(VERSION)
BLDFLAGS_WIN   := -H windowsgui -X main.version=$(VERSION)

.PHONY: all linux-x64 win-x64 bmslistener clean dist

all: linux-x64 win-x64 bmslistener

dist:
	mkdir -p dist

linux-x64: dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(BLDFLAGS_LINUX)" -o dist/sunReceiver-linux-amd64-$(VERSION) .
	@echo "OK: dist/sunReceiver-linux-amd64-$(VERSION)"

win-x64: dist
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(BLDFLAGS_WIN)" -o dist/sunReceiver-windows-amd64-$(VERSION).exe .
	@echo "OK: dist/sunReceiver-windows-amd64-$(VERSION).exe"

bmslistener: dist
	zig cc -O2 -std=gnu99 -Wall -Wextra -target arm-linux-musleabihf -static -o dist/bmslistener-armv7l-$(VERSION) bmslistener/bmslistener.c
	@echo "OK: dist/bmslistener-armv7l-$(VERSION)"

clean:
	rm -rf dist
