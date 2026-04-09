REPO ?= review-dashboard
TAG ?= latest
IMAGE := $(REPO):$(TAG)

.PHONY: all image docker-build build-image push

all: image

image:
	docker build -t $(IMAGE) .

docker-build: image

build-image: image

push:
	docker push $(IMAGE)