.DEFAULT_GOAL := help

export GOTOOLCHAIN := go1.27.3
export GOWORK := off
export CGO_ENABLED := 0

# Every rule delegates to the Go builder, so make and CI cannot drift apart.
RELEASE := go run ./internal/build/release

PLATFORMS := windows linux darwin
ARCHES    := amd64 arm64
PORTABLE  := $(foreach p,$(PLATFORMS),$(foreach a,$(ARCHES),$(p)-$(a)))
WEB_ONLY  := $(addprefix no-ui-,$(PORTABLE))

.PHONY: help web check all native no-ui matrix $(PORTABLE) $(WEB_ONLY)

help:
	@$(RELEASE) -help

web:
	$(RELEASE) -web-only

check:
	$(RELEASE) -check

all:
	$(RELEASE)

native:
	$(RELEASE) -web=false -native

no-ui:
	$(RELEASE) -web=false -no-ui

# Every platform and architecture, compiled concurrently.
matrix:
	$(RELEASE) -web=false -os windows,linux,darwin -arch amd64,arm64 -jobs $(if $(jobs),$(jobs),0)

# Portable goals take the platform from the goal name, e.g. no-ui-darwin-arm64.
$(PORTABLE) $(WEB_ONLY): TARGET_OS = $(word 1,$(subst -, ,$(patsubst no-ui-%,%,$@)))
$(PORTABLE) $(WEB_ONLY): TARGET_ARCH = $(word 2,$(subst -, ,$(patsubst no-ui-%,%,$@)))
$(PORTABLE) $(WEB_ONLY):
	$(RELEASE) -web=false -os $(TARGET_OS) -arch $(TARGET_ARCH) $(if $(filter no-ui-%,$@),-no-ui,-native)
