#REGISTRY ?= gcr.io/dordel-gke-dev
REGISTRY ?= gcr.io/gke-networking-test-images/scale-tests
IMAGE_NAME = network-policy-latency-client
VERSION ?= v0.0.4

all: push


.PHONY: build
build:
	docker build -t $(REGISTRY)/$(IMAGE_NAME):$(VERSION) .

.PHONY: push
push: build
	docker push "$(REGISTRY)/$(IMAGE_NAME):$(VERSION)"
