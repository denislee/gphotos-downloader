BINARY := gphotos-downloader
PKG    := ./...

.PHONY: all build run tidy vet clean

all: build

build:
	go build -o $(BINARY) $(PKG)

run: build
	./$(BINARY)

tidy:
	go mod tidy

vet:
	go vet $(PKG)

clean:
	rm -f $(BINARY)
