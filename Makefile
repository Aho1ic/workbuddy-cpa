GO ?= go
SUFFIX := $(shell $(GO) env GOOS | sed 's/darwin/.dylib/;s/linux/.so/;s/windows/.dll/')
PLUGIN = workbuddy$(SUFFIX)

.PHONY: build test smoke clean

build:
	CGO_ENABLED=1 $(GO) build -buildmode=c-shared -trimpath -ldflags "-X main.pluginVersion=0.2.0" -o $(PLUGIN) .
	-codesign --force --sign - $(PLUGIN) 2>/dev/null || true

test:
	CGO_ENABLED=1 $(GO) vet ./...
	CGO_ENABLED=1 $(GO) test ./...

smoke: build
	clang -o hostsmoke/host_smoke hostsmoke/host_smoke.c
	./hostsmoke/host_smoke ./$(PLUGIN)

clean:
	rm -f workbuddy.dylib workbuddy.so workbuddy.dll workbuddy.h hostsmoke/host_smoke
