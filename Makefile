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

STAGINGIMAGE=${GCE_PD_CSI_STAGING_IMAGE}
STAGINGVERSION=latest

PRODIMAGE=gcr.io/google-containers/volume-csi/compute-persistent-disk-csi-driver
PRODVERSION=v0.2.0.alpha
all: gce-pd-driver

gce-pd-driver:
	mkdir -p bin
	go build -o bin/gce-pd-csi-driver ./cmd/

build-container: gce-pd-driver
	docker build -t $(STAGINGIMAGE):$(STAGINGVERSION) .

push-container: build-container
	gcloud docker -- push $(STAGINGIMAGE):$(STAGINGVERSION)

prod-build-container: gce-pd-driver
	docker build -t $(PRODIMAGE):$(PRODVERSION)

prod-push-container: prod-build-container
	gcloud docker -- push $(PRODIMAGE):$(PRODVERSION)

test-sanity: gce-pd-driver
	go test -timeout 30s sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/pkg/test -run ^TestSanity$