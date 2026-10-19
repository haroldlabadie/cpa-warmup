.PHONY: test build
test:
	go test ./...
	go test -race ./...
	go vet ./...
	node --check web/settings.js
	node --test web/settings.test.js
build:
	mkdir -p dist
	CGO_ENABLED=1 go build -buildvcs=false -trimpath -buildmode=c-shared -o dist/cpa-warmup.so .
	CGO_ENABLED=1 go build -buildvcs=false -trimpath -o dist/cpa-warmup-helper .
