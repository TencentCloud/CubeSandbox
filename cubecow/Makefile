.PHONY: fmt test

fmt:
	cargo fmt
	cd examples/go-test && go fmt ./...

test:
	cargo test -p cubecow --lib
