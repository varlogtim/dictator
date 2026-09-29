# dictator — local speech-to-text daemon (dictatord) + client (dictator).
#
# Targets you will actually use:
#   make models      download Silero VAD + Parakeet TDT 0.6B v2 (int8) to MODEL_DIR
#   make install     build, copy binaries/libs/unit into ~/.local and ~/.config
#   make enable      systemctl --user enable --now dictator
#   make reinstall   install + restart the running service
#   make test        unit tests (no model, no cgo libs needed at runtime)
#   make test-model  integration tests against the installed models
#   make smoke       end-to-end run with a stub recorder and stub wtype
SHELL := /bin/bash

PREFIX    ?= $(HOME)/.local
BIN_DIR   ?= $(PREFIX)/bin
LIB_DIR   ?= $(PREFIX)/lib/dictator
DATA_DIR  ?= $(or $(XDG_DATA_HOME),$(HOME)/.local/share)/dictator
MODEL_DIR ?= $(DATA_DIR)/models
UNIT_DIR  ?= $(HOME)/.config/systemd/user
CONF_DIR  ?= $(HOME)/.config/dictator

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

# The Go binding ships prebuilt shared libraries and bakes an rpath into the
# module cache. We add our own rpath ($ORIGIN/../lib/dictator, i.e.
# $(LIB_DIR) when installed to $(BIN_DIR)) and copy the two libraries there
# so the daemon survives `go clean -modcache`.
SHERPA_MOD := github.com/k2-fsa/sherpa-onnx-go-linux
SHERPA_LIB  = $(shell go list -m -f '{{.Dir}}' $(SHERPA_MOD))/lib/x86_64-unknown-linux-gnu
SHERPA_SOS := libsherpa-onnx-c-api.so libonnxruntime.so

MODEL_NAME := sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8
MODEL_URL  := https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models

.PHONY: all build daemon client test test-model smoke vet fmt libs models \
        install enable reinstall uninstall clean

all: build

build: daemon client

daemon:
	CGO_ENABLED=1 CGO_LDFLAGS='-Wl,-rpath,$$ORIGIN/../lib/dictator' \
		go build -ldflags '$(LDFLAGS)' -o build/dictatord ./cmd/dictatord

client:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o build/dictator ./cmd/dictator

vet:
	go vet ./...

fmt:
	gofmt -l -w cmd internal

test: vet
	go test -race ./...

test-model:
	DICTATOR_TEST_MODEL_DIR=$(MODEL_DIR)/$(MODEL_NAME) \
	DICTATOR_TEST_VAD_MODEL=$(MODEL_DIR)/silero_vad.onnx \
		go test -count=1 -v ./internal/asr/

smoke: build
	DICTATOR_MODEL_DIR=$(MODEL_DIR)/$(MODEL_NAME) \
	DICTATOR_VAD_MODEL=$(MODEL_DIR)/silero_vad.onnx \
		scripts/smoke.sh

libs:
	@test -d "$(SHERPA_LIB)" || { echo "sherpa-onnx libs not found; run 'go mod download' first"; exit 1; }
	install -d $(LIB_DIR)
	for so in $(SHERPA_SOS); do install -m 0644 "$(SHERPA_LIB)/$$so" $(LIB_DIR)/; done

models: $(MODEL_DIR)/silero_vad.onnx $(MODEL_DIR)/$(MODEL_NAME)/tokens.txt

$(MODEL_DIR)/silero_vad.onnx:
	install -d $(MODEL_DIR)
	curl -fL --progress-bar -o $@.tmp $(MODEL_URL)/silero_vad.onnx && mv $@.tmp $@

$(MODEL_DIR)/$(MODEL_NAME)/tokens.txt:
	install -d $(MODEL_DIR)
	@echo "downloading $(MODEL_NAME) (~630 MB)"
	curl -fL --progress-bar $(MODEL_URL)/$(MODEL_NAME).tar.bz2 | tar xj -C $(MODEL_DIR)

install: build libs
	install -d $(BIN_DIR) $(UNIT_DIR) $(CONF_DIR)
	install -m 0755 build/dictatord build/dictator $(BIN_DIR)/
	install -m 0644 systemd/dictator.service $(UNIT_DIR)/dictator.service
	@test -e $(CONF_DIR)/dictator.env || install -m 0644 contrib/dictator.env.example $(CONF_DIR)/dictator.env
	systemctl --user daemon-reload
	@echo
	@echo "installed. next:"
	@echo "  make models            # if you have not yet"
	@echo "  make enable            # start now and at login"
	@echo "  sudo apt install wtype # needed for 'type' mode"
	@echo "  see contrib/ for sway and waybar snippets"

enable:
	systemctl --user enable --now dictator
	systemctl --user --no-pager status dictator | head -n 5

reinstall: install
	systemctl --user restart dictator
	systemctl --user --no-pager status dictator | head -n 5

uninstall:
	-systemctl --user disable --now dictator
	rm -f $(BIN_DIR)/dictatord $(BIN_DIR)/dictator $(UNIT_DIR)/dictator.service
	rm -rf $(LIB_DIR)
	systemctl --user daemon-reload
	@echo "kept: $(CONF_DIR) and $(MODEL_DIR)"

clean:
	rm -rf build
