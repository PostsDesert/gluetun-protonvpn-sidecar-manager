# Makefile for Gluetun ProtonVPN Sidecar Manager

# Configuration
BINARY_NAME=manager
GO_DIR=go-manager
DOCKER_IMAGE=proton-manager

.PHONY: all build clean run docker

all: build

build:
	@echo "Building $(BINARY_NAME)..."
	@cd $(GO_DIR) && go build -o ../$(BINARY_NAME) main.go
	@echo "Built $(BINARY_NAME) successfully."

clean:
	@echo "Cleaning up..."
	@rm -f $(BINARY_NAME)
	@rm -rf $(GO_DIR)/vendor

run: build
	@echo "Running $(BINARY_NAME)..."
	@./$(BINARY_NAME)

docker:
	@echo "Building Docker image $(DOCKER_IMAGE)..."
	@docker build -t $(DOCKER_IMAGE) .
	@echo "Docker image built successfully."

fmt:
	@cd $(GO_DIR) && go fmt ./...

vet:
	@cd $(GO_DIR) && go vet ./...

tidy:
	@cd $(GO_DIR) && go mod tidy
