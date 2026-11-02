GO ?= go
REPOSITORY ?= https://github.com/njzjz/cpa-ustc
CCACHE := $(shell command -v ccache 2>/dev/null)
HOST_CC := $(shell $(GO) env CC 2>/dev/null)
ifeq ($(shell uname -s),Darwin)
  LIBRARY := ustc.dylib
else ifeq ($(OS),Windows_NT)
  LIBRARY := ustc.dll
else
  LIBRARY := ustc.so
endif

.PHONY: build test check clean
build:
	mkdir -p dist
	$(GO) build -trimpath -o dist/cpa-ustc ./cmd/cpa-ustc
	CGO_ENABLED=1 CC="$(if $(CCACHE),$(CCACHE) ,)$(HOST_CC)" $(GO) build -trimpath -ldflags '-X cpa-ustc/internal/plugin.Repository=$(REPOSITORY)' -buildmode=c-shared -o dist/$(LIBRARY) ./cmd/ustc-plugin

test:
	$(GO) test -race ./...

check:
	$(GO) vet ./...

clean:
	rm -rf dist
