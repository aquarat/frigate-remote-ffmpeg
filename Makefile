MODULE := github.com/aquarat/frigate-ffmpeg-proxy

DIST := dist
WRAPPER_BIN_AMD64 := $(DIST)/wrapper-linux-amd64
WRAPPER_BIN_ARM64 := $(DIST)/wrapper-linux-arm64
COORDINATOR_BIN  := $(DIST)/coordinator-darwin-arm64

# Go build flags: statically linked, strip debug symbols for smaller binaries.
GO_FLAGS := -trimpath -ldflags="-s -w"

.PHONY: all coordinator wrapper-amd64 wrapper-arm64 docker-wrapper clean install-launchd uninstall-launchd

all: coordinator wrapper-amd64 wrapper-arm64

## coordinator — native macOS arm64 binary (must be run on the host)
coordinator:
	@mkdir -p $(DIST)
	GOOS=darwin GOARCH=arm64 go build $(GO_FLAGS) -o $(COORDINATOR_BIN) ./cmd/coordinator
	@echo "Built coordinator: $(COORDINATOR_BIN)"

## wrapper-amd64 — Linux amd64 binary (for x86-64 Docker containers)
wrapper-amd64:
	@mkdir -p $(DIST)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build $(GO_FLAGS) -o $(WRAPPER_BIN_AMD64) ./cmd/wrapper
	@echo "Built wrapper: $(WRAPPER_BIN_AMD64)"

## wrapper-arm64 — Linux arm64 binary (for arm64 Docker containers, e.g. Apple Silicon)
wrapper-arm64:
	@mkdir -p $(DIST)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build $(GO_FLAGS) -o $(WRAPPER_BIN_ARM64) ./cmd/wrapper
	@echo "Built wrapper: $(WRAPPER_BIN_ARM64)"

## docker-wrapper — build both wrapper variants inside Docker (no local Go needed)
docker-wrapper:
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		--output type=local,dest=$(DIST)/docker \
		-f docker/Dockerfile.wrapper \
		.
	@echo "Wrapper binaries written to $(DIST)/docker/"

## install-launchd — install and load the coordinator as a launchd user agent
install-launchd: $(COORDINATOR_BIN)
	@echo "Installing coordinator to /usr/local/bin/frigate-coordinator"
	sudo install -m 755 $(COORDINATOR_BIN) /usr/local/bin/frigate-coordinator
	@mkdir -p ~/Library/LaunchAgents
	install -m 644 launchd/com.frigate.coordinator.plist ~/Library/LaunchAgents/
	launchctl load -w ~/Library/LaunchAgents/com.frigate.coordinator.plist
	@echo "Service loaded. Check status with: launchctl list com.frigate.coordinator"

## uninstall-launchd — unload and remove the launchd user agent
uninstall-launchd:
	-launchctl unload ~/Library/LaunchAgents/com.frigate.coordinator.plist
	-rm ~/Library/LaunchAgents/com.frigate.coordinator.plist
	@echo "Service removed"

clean:
	rm -rf $(DIST)
