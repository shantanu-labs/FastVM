.PHONY: all build build-agent build-linux test run clean assets lint

BIN_DIR := bin
AIRBOXD_BIN := $(BIN_DIR)/airboxd
AGENT_INIT_BIN := $(BIN_DIR)/agent-init

all: build

build: $(AIRBOXD_BIN) $(AGENT_INIT_BIN)

$(AIRBOXD_BIN):
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -o $(AIRBOXD_BIN) ./cmd/airboxd

$(AGENT_INIT_BIN):
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -o $(AGENT_INIT_BIN) ./cmd/agent-init
	@if [ "$$(uname -s)" = "Linux" ] && [ -f assets/rootfs.ext4 ]; then \
		echo "==> Injecting $(AGENT_INIT_BIN) into assets/rootfs.ext4 (/sbin/agent-init)..."; \
		truncate -s 350M assets/rootfs.ext4 && e2fsck -fy assets/rootfs.ext4 >/dev/null 2>&1 || true; \
		resize2fs assets/rootfs.ext4 >/dev/null 2>&1 || true; \
		sudo mkdir -p /mnt/airbox-rootfs && sudo mount -o loop assets/rootfs.ext4 /mnt/airbox-rootfs && \
		sudo cp $(AGENT_INIT_BIN) /mnt/airbox-rootfs/sbin/agent-init && \
		sudo chmod +x /mnt/airbox-rootfs/sbin/agent-init && \
		sudo umount /mnt/airbox-rootfs; \
	fi

build-linux:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o $(AIRBOXD_BIN)-linux ./cmd/airboxd
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o $(AGENT_INIT_BIN)-linux ./cmd/agent-init

test:
	go test -v -race ./tests/unit/...
	go test -v ./tests/adversarial/...

bench:
	go run scripts/benchmark.go --url http://localhost:8080 --requests 50 --concurrency 5

run: build
	./$(AIRBOXD_BIN) --port 8080 --kernel assets/vmlinux --rootfs assets/rootfs.ext4

assets:
	chmod +x scripts/download-assets.sh
	./scripts/download-assets.sh

setup-host:
	chmod +x scripts/setup-host.sh
	./scripts/setup-host.sh

clean:
	rm -rf $(BIN_DIR) /tmp/airbox-overlays /tmp/firecracker-*.sock /tmp/vhost-*.sock
