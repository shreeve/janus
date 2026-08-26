# janus — build and install the Caddy + Janus binary
#
# `make janus` builds the working tree with the Caddy version pinned in
# go.mod. `make install` copies that binary onto PATH in /usr/local/bin,
# using sudo only when the destination is not writable. GitHub releases use
# scripts/package-release.sh after building each native platform in CI.

.PHONY: all janus unit test install clean

CADDY_VERSION := $(shell awk '$$1 == "github.com/caddyserver/caddy/v2" { print $$2 }' go.mod)
XCADDY ?= xcaddy
OUT ?= bin/caddy-janus
BIN ?= /usr/local/bin

all: janus

janus:
	mkdir -p "$(dir $(OUT))"
	$(XCADDY) build "$(CADDY_VERSION)" \
		--with github.com/shreeve/janus=. \
		--output "$(OUT)"
	"$(OUT)" list-modules | grep janus >/dev/null
	@echo "built $(OUT)  ($$("$(OUT)" version))"

unit:
	go test ./...

test: janus unit
	CADDY_BIN="$(abspath $(OUT))" ./test.sh

# Match Harbor's local install behavior. Removing the destination first is
# load-bearing on macOS: replacing the inode avoids a stale kernel signature
# cache after installing a newly linked executable.
install: janus
	@sudo= ; \
	  if [ ! -d "$(BIN)" ]; then install -d -m 0755 "$(BIN)" 2>/dev/null || sudo=sudo; fi; \
	  [ -w "$(BIN)" ] || sudo=sudo; \
	  [ -z "$$sudo" ] || echo "  $(BIN) needs elevated access — using sudo"; \
	  $$sudo install -d -m 0755 "$(BIN)"; \
	  $$sudo rm -f "$(BIN)/caddy-janus"; \
	  $$sudo install -m 0755 "$(OUT)" "$(BIN)/caddy-janus"; \
	  echo "installed caddy-janus -> $(BIN)  (on your PATH)"

clean:
	rm -rf bin
