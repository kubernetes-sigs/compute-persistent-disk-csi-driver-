# Copyright 2018 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Args:
# GCE_PD_CSI_STAGING_IMAGE: Staging image repository
REV=$(shell git describe --long --tags --match='v*' --dirty 2>/dev/null || git rev-list -n1 HEAD)
GCE_PD_CSI_STAGING_VERSION ?= ${REV}
STAGINGVERSION=${GCE_PD_CSI_STAGING_VERSION}
STAGINGIMAGE=${GCE_PD_CSI_STAGING_IMAGE}
DRIVERBINARY=gce-pd-csi-driver
DRIVERWINDOWSBINARY=${DRIVERBINARY}.exe

DOCKER=DOCKER_CLI_EXPERIMENTAL=enabled docker

BASE_IMAGE_LTSC2019=mcr.microsoft.com/windows/servercore:ltsc2019
BASE_IMAGE_1909=mcr.microsoft.com/windows/servercore:1909
BASE_IMAGE_2004=mcr.microsoft.com/windows/servercore:2004
BASE_IMAGE_20H2=mcr.microsoft.com/windows/servercore:20H2

# Both arrays MUST be index aligned.
WINDOWS_IMAGE_TAGS=ltsc2019 1909 2004 20H2
WINDOWS_BASE_IMAGES=$(BASE_IMAGE_LTSC2019) $(BASE_IMAGE_1909) $(BASE_IMAGE_2004) $(BASE_IMAGE_20H2)

all: gce-pd-driver gce-pd-driver-windows
gce-pd-driver:
	mkdir -p bin
	go build -mod=vendor -gcflags=$(GCFLAGS) -ldflags "-X main.version=$(STAGINGVERSION)" -o bin/${DRIVERBINARY} ./cmd/gce-pd-csi-driver/

gce-pd-driver-windows:
	mkdir -p bin
	GOOS=windows go build -mod=vendor -ldflags -X=main.version=$(STAGINGVERSION) -o bin/${DRIVERWINDOWSBINARY} ./cmd/gce-pd-csi-driver/

# build and keep in the local registry the container. It build only for linux/amd64 platform
build-container-linux: require-GCE_PD_CSI_STAGING_IMAGE init-buildx
	$(DOCKER) buildx build --platform=linux \
		-t $(STAGINGIMAGE):$(STAGINGVERSION) \
		--build-arg STAGINGVERSION=$(STAGINGVERSION) --load .

# build and push to a remote registry defined in GCE_PD_CSI_STAGING_IMAGE. It build only for linux/amd64 platform
build-and-push-container-linux: require-GCE_PD_CSI_STAGING_IMAGE init-buildx
	$(DOCKER) buildx build --platform=linux \
		-t $(STAGINGIMAGE):$(STAGINGVERSION) \
		--build-arg STAGINGVERSION=$(STAGINGVERSION) --push .

build-and-push-windows-container-ltsc2019: require-GCE_PD_CSI_STAGING_IMAGE init-buildx
	$(DOCKER) buildx build --file=Dockerfile.Windows --platform=windows \
		-t $(STAGINGIMAGE):$(STAGINGVERSION)_ltsc2019 \
		--build-arg BASE_IMAGE=$(BASE_IMAGE_LTSC2019) \
		--build-arg STAGINGVERSION=$(STAGINGVERSION) --push .

build-and-push-windows-container-1909: require-GCE_PD_CSI_STAGING_IMAGE init-buildx
	$(DOCKER) buildx build --file=Dockerfile.Windows --platform=windows \
		-t $(STAGINGIMAGE):$(STAGINGVERSION)_1909 \
		--build-arg BASE_IMAGE=$(BASE_IMAGE_1909) \
		--build-arg STAGINGVERSION=$(STAGINGVERSION) --push .

build-and-push-windows-container-2004: require-GCE_PD_CSI_STAGING_IMAGE init-buildx
	$(DOCKER) buildx build --file=Dockerfile.Windows --platform=windows \
		-t $(STAGINGIMAGE):$(STAGINGVERSION)_2004 \
		--build-arg BASE_IMAGE=$(BASE_IMAGE_2004) \
		--build-arg STAGINGVERSION=$(STAGINGVERSION) --push .

build-and-push-windows-container-20H2: require-GCE_PD_CSI_STAGING_IMAGE init-buildx
	$(DOCKER) buildx build --file=Dockerfile.Windows --platform=windows \
		-t $(STAGINGIMAGE):$(STAGINGVERSION)_20H2 \
		--build-arg BASE_IMAGE=$(BASE_IMAGE_20H2) \
		--build-arg STAGINGVERSION=$(STAGINGVERSION) --push .

build-and-push-multi-arch: build-and-push-container-linux-amd64 build-and-push-container-linux-arm64 build-and-push-windows-container-ltsc2019 build-and-push-windows-container-1909 build-and-push-windows-container-2004 build-and-push-windows-container-20H2
	$(DOCKER) manifest create --amend $(STAGINGIMAGE):$(STAGINGVERSION) $(STAGINGIMAGE):$(STAGINGVERSION)_linux_amd64 $(STAGINGIMAGE):$(STAGINGVERSION)_linux_arm64 $(STAGINGIMAGE):$(STAGINGVERSION)_20H2 $(STAGINGIMAGE):$(STAGINGVERSION)_2004 $(STAGINGIMAGE):$(STAGINGVERSION)_1909 $(STAGINGIMAGE):$(STAGINGVERSION)_ltsc2019
	STAGINGIMAGE="$(STAGINGIMAGE)" STAGINGVERSION="$(STAGINGVERSION)" WINDOWS_IMAGE_TAGS="$(WINDOWS_IMAGE_TAGS)" WINDOWS_BASE_IMAGES="$(WINDOWS_BASE_IMAGES)" ./manifest_osversion.sh
	$(DOCKER) manifest push -p $(STAGINGIMAGE):$(STAGINGVERSION)

build-and-push-multi-arch-debug: build-and-push-container-linux-debug build-and-push-windows-container-ltsc2019
	$(DOCKER) manifest create --amend $(STAGINGIMAGE):$(STAGINGVERSION) $(STAGINGIMAGE):$(STAGINGVERSION)_linux $(STAGINGIMAGE):$(STAGINGVERSION)_ltsc2019
	STAGINGIMAGE="$(STAGINGIMAGE)" STAGINGVERSION="$(STAGINGVERSION)" WINDOWS_IMAGE_TAGS="ltsc2019" WINDOWS_BASE_IMAGES="$(BASE_IMAGE_LTSC2019)" ./manifest_osversion.sh
	$(DOCKER) manifest push -p $(STAGINGIMAGE):$(STAGINGVERSION)

build-and-push-container-linux-amd64: require-GCE_PD_CSI_STAGING_IMAGE init-buildx
	$(DOCKER) buildx build --platform=linux/amd64 \
		-t $(STAGINGIMAGE):$(STAGINGVERSION)_linux_amd64 \
		--build-arg STAGINGVERSION=$(STAGINGVERSION) --push .

build-and-push-container-linux-arm64: require-GCE_PD_CSI_STAGING_IMAGE init-buildx
	$(DOCKER) buildx build --platform=linux/arm64 \
		-t $(STAGINGIMAGE):$(STAGINGVERSION)_linux_arm64 \
		--build-arg STAGINGVERSION=$(STAGINGVERSION) --push .

build-and-push-container-linux-debug: require-GCE_PD_CSI_STAGING_IMAGE init-buildx
	$(DOCKER) buildx build --file=Dockerfile.debug --platform=linux \
		-t $(STAGINGIMAGE):$(STAGINGVERSION)_linux \
		--build-arg STAGINGVERSION=$(STAGINGVERSION) --push .

test-sanity: gce-pd-driver
	go test -mod=vendor --v -timeout 30s sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/test/sanity -run ^TestSanity$

test-k8s-integration:
	go build -mod=vendor -o bin/k8s-integration-test ./test/k8s-integration

require-GCE_PD_CSI_STAGING_IMAGE:
ifndef GCE_PD_CSI_STAGING_IMAGE
	$(error "Must set environment variable GCE_PD_CSI_STAGING_IMAGE to staging image repository")
endif

init-buildx:
	# Ensure we use a builder that can leverage it (the default on linux will not)
	-$(DOCKER) buildx rm multiarch-multiplatform-builder
	$(DOCKER) buildx create --use --name=multiarch-multiplatform-builder
	$(DOCKER) run --rm --privileged multiarch/qemu-user-static --reset --credential yes --persistent yes
	# Register gcloud as a Docker credential helper.
	# Required for "docker buildx build --push".
	gcloud auth configure-docker --quiet
