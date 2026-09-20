# janus — build and install the Janus binary
#
# `make janus` compiles cmd/janus — stock Caddy plus the Janus module and
# the Route 53 DNS provider — with every dependency pinned in go.mod.
# `make install` installs it the way install.sh does: on macOS as
# ~/Applications/Janus.app with the janus command linked into it, elsewhere
# the binary; into ~/.local/bin as a user, /usr/local/bin as root (BIN=
# overrides), using sudo only when the destination is not writable. GitHub releases use
# scripts/package-release.sh after building each native platform in CI.

.PHONY: all janus bundle unit test install clean

OUT ?= bin/janus
APP ?= dist/Janus.app
# BIN is where the janus command lands. Unset, the installer chooses as
# install.sh does: ~/.local/bin for a user, /usr/local/bin for root.
BIN ?=

# The macOS code-signing identifier. Local Network privacy files its allow/deny
# under this name (with the executable's path and build UUID), so it stays the
# same across releases; scripts/release-install.sh and install.sh carry the
# same literal.
IDENTIFIER ?= com.github.shreeve.janus

all: janus

janus:
	mkdir -p "$(dir $(OUT))"
	go build -trimpath -o "$(OUT)" ./cmd/janus
	@if [ "$$(uname -s)" = Darwin ]; then codesign -s - -f -i "$(IDENTIFIER)" "$(OUT)"; fi
	"$(OUT)" list-modules | grep janus >/dev/null
	@echo "built $(OUT)  ($$("$(OUT)" version))"

unit:
	go test ./...

test: janus unit
	CADDY_BIN="$(abspath $(OUT))" ./test.sh

# macOS installs the application bundle (what Local Network privacy
# identifies: name, icon, usage description) and links the command into
# it; elsewhere the bare binary. The installer performs the atomic swap.
bundle: janus
	scripts/bundle-macos.sh "$(OUT)" "$(APP)"

# A fresh inode preserves macOS signature behavior; atomic rename keeps the
# old executable available until its complete replacement is ready.
install: janus
	@if [ "$$(uname -s)" = Darwin ]; then \
		scripts/bundle-macos.sh "$(OUT)" "$(APP)" && $(if $(BIN),BIN="$(abspath $(BIN))") scripts/release-install.sh "$(abspath $(APP))"; \
	else $(if $(BIN),BIN="$(abspath $(BIN))") scripts/release-install.sh "$(abspath $(OUT))"; fi

clean:
	rm -rf bin
