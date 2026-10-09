PLUGIN_ID := staged-scheduler
OUTPUT := dist/$(PLUGIN_ID).so

.PHONY: fmt test race vet verify build

fmt:
	gofmt -w *.go

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

verify: fmt test race vet
	go mod verify

build:
	mkdir -p dist
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildmode=c-shared -o $(OUTPUT) .
