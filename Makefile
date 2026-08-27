# janus — build and install the Janus binary
#
# `make janus` compiles cmd/janus — stock Caddy plus the Janus module and
# the Route 53 DNS provider — with every dependency pinned in go.mod.
# `make install` copies that binary onto PATH in /usr/local/bin, using sudo
# only when the destination is not writable. GitHub releases use
# scripts/package-release.sh after building each native platform in CI.

.PHONY: all janus unit test install clean

OUT ?= bin/janus
BIN ?= /usr/local/bin

all: janus

janus:
	mkdir -p "$(dir $(OUT))"
	go build -trimpath -o "$(OUT)" ./cmd/janus
	"$(OUT)" list-modules | grep janus >/dev/null
	@echo "built $(OUT)  ($$("$(OUT)" version))"

unit:
	go test ./...

test: janus unit
	CADDY_BIN="$(abspath $(OUT))" ./test.sh

# Match Harbor's local install behavior. Removing the destination first is
# load-bearing on macOS: replacing the inode avoids a stale kernel signature
# cache after installing a newly linked executable. Both names are removed so
# exactly one janus binary lives on PATH.
install: janus
	@sudo= ; \
	  if [ ! -d "$(BIN)" ]; then install -d -m 0755 "$(BIN)" 2>/dev/null || sudo=sudo; fi; \
	  [ -w "$(BIN)" ] || sudo=sudo; \
	  [ -z "$$sudo" ] || echo "  $(BIN) needs elevated access — using sudo"; \
	  $$sudo install -d -m 0755 "$(BIN)"; \
	  $$sudo rm -f "$(BIN)/janus" "$(BIN)/caddy-janus"; \
	  $$sudo install -m 0755 "$(OUT)" "$(BIN)/janus"; \
	  echo "installed janus -> $(BIN)  (on your PATH)"

clean:
	rm -rf bin
