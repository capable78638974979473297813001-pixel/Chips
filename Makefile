.PHONY: build test lint demo bench clean

build:
	go build -o bin/ ./cmd/...

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

# Run against a running `make demo` in another terminal.
bench: build
	bin/srbench -conversations 30 -turns 10 -concurrency 8 -system-chars 400 -user-chars 2000

clean:
	rm -rf bin
