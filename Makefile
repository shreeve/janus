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

# A fresh inode preserves macOS signature behavior; atomic rename keeps the
# old executable available until its complete replacement is ready. Source
# builds keep their explicit system-wide destination default.
install: janus
	@BIN="$(abspath $(BIN))" scripts/release-install.sh "$(abspath $(OUT))"

clean:
	rm -rf bin
