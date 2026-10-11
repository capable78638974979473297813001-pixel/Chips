.PHONY: build test lint cross dist demo demo-mixed bench soak agent agent-test agent-cross clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X main.version=$(VERSION)

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/ ./cmd/...

test:
	go test -race ./...

lint:
	gofmt -l . | (! grep .) || (echo "run gofmt -w ." && exit 1)
	go vet ./...

# Starts three simulated GPU servers and the router on :8080. Ctrl-C stops all.
demo: build
	@trap 'kill 0' INT TERM EXIT; \
	bin/mockllm -addr :9001 -name h100 -prefill-tps 3000 -decode-tps 400 -max-concurrency 8 & \
	bin/mockllm -addr :9002 -name mac-studio -prefill-tps 1500 -decode-tps 200 -max-concurrency 4 & \
	bin/mockllm -addr :9003 -name mi300 -prefill-tps 2000 -decode-tps 300 -max-concurrency 4 & \
	sleep 0.5; bin/siliconrouter -config examples/demo.yaml

# Cross-compiles the router for every OS and CPU Go supports that a company
# might run it on. Pure Go with no cgo, so no extra toolchains are needed.
CROSS_TARGETS = linux/amd64 linux/386 linux/arm linux/arm64 linux/ppc64le linux/ppc64 linux/s390x \
	linux/riscv64 linux/mips linux/mipsle linux/mips64 linux/mips64le linux/loong64 \
	windows/amd64 windows/386 windows/arm64 darwin/amd64 darwin/arm64 \
	freebsd/amd64 freebsd/arm64 freebsd/386 openbsd/amd64 netbsd/amd64 illumos/amd64 aix/ppc64
cross:
	@for t in $(CROSS_TARGETS); do \
		echo "go build $$t"; \
		GOOS=$${t%/*} GOARCH=$${t#*/} CGO_ENABLED=0 go build -o bin/$${t%/*}-$${t#*/}/ ./cmd/... || exit 1; \
	done

# Release archives of the router for the common platforms, with checksums.
DIST_TARGETS = linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
dist:
	rm -rf dist && mkdir -p dist
	@for t in $(DIST_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; name=siliconrouter-$(VERSION)-$$os-$$arch; \
		echo "dist $$name"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$$name/ ./cmd/... || exit 1; \
		cp LICENSE README.md examples/siliconrouter.yaml dist/$$name/; \
		if [ $$os = windows ]; then (cd dist && zip -qr $$name.zip $$name); \
		else tar -C dist -czf dist/$$name.tar.gz $$name; fi; \
		rm -rf dist/$$name; \
	done
	cd dist && sha256sum * > SHA256SUMS

# chipagent, the C++ hardware agent that runs on each GPU server.
agent:
	cmake -S agent -B agent/build -DCMAKE_BUILD_TYPE=Release
	cmake --build agent/build -j

agent-test: agent
	ctest --test-dir agent/build --output-on-failure

# Builds chipagent for 7 more CPU architectures and runs its tests under QEMU.
agent-cross:
	scripts/agent-cross-test.sh

# Like demo, but every node runs a simulated chipagent (NVIDIA, AMD, Apple)
# and the router assigns tiers from the reported hardware.
demo-mixed: build agent
	@trap 'kill 0' INT TERM EXIT; \
	bin/mockllm -addr :9001 -name h100 -prefill-tps 3000 -decode-tps 400 -max-concurrency 8 & \
	bin/mockllm -addr :9002 -name mi300x -prefill-tps 2800 -decode-tps 380 -max-concurrency 8 & \
	bin/mockllm -addr :9003 -name m2-ultra -prefill-tps 1200 -decode-tps 150 -max-concurrency 4 & \
	agent/build/chipagent --listen 127.0.0.1:9401 --simulate "nvidia:NVIDIA H100 80GB HBM3:80:20" & \
	agent/build/chipagent --listen 127.0.0.1:9402 --simulate "amd:AMD Instinct MI300X:192:15" & \
	agent/build/chipagent --listen 127.0.0.1:9403 --simulate "apple:Apple M2 Ultra GPU:192" & \
	sleep 0.5; bin/siliconrouter -config examples/mixed-fleet.yaml

# Run against a running `make demo` in another terminal.
# Steady load with failing, killed and restarted backends and config reloads;
# fails on errors, held slots, panics, or growing goroutines or heap.
soak: build
	DURATION=$${DURATION:-120} scripts/soak.sh

bench: build
	bin/srbench -conversations 30 -turns 10 -concurrency 8 -system-chars 400 -user-chars 2000

clean:
	rm -rf bin dist agent/build agent/build-*
