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

ARG STAGINGVERSION
ARG BUILDPLATFORM
ARG TARGETPLATFORM

FROM --platform=$BUILDPLATFORM golang:1.13.15 as builder

WORKDIR /go/src/sigs.k8s.io/gcp-compute-persistent-disk-csi-driver
ADD . .
# RUN mkdir -p bin \
#   && GOARCH=$(echo $TARGETPLATFORM | cut -f2 -d '/') go build -mod=vendor -ldflags "-X main.version=${STAGINGVERSION}" -o bin/gce-pd-csi-driver ./cmd/gce-pd-csi-driver/
RUN GOARCH=$(echo $TARGETPLATFORM | cut -f2 -d '/') GCE_PD_CSI_STAGING_VERSION=${STAGINGVERSION} make gce-pd-driver

# MAD HACKS: Build a version first so we can take the scsi_id bin and put it somewhere else in our real build
FROM k8s.gcr.io/build-image/debian-base:buster-v1.6.0 as mad-hack
RUN ln -s /bin/rm /usr/sbin/rm \
  && clean-install udev

# Start from Kubernetes Debian base
FROM k8s.gcr.io/build-image/debian-base:buster-v1.6.0
COPY --from=builder /go/src/sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/bin/gce-pd-csi-driver /gce-pd-csi-driver
# Install necessary dependencies
RUN ln -s /bin/rm /usr/sbin/rm \
  && clean-install util-linux e2fsprogs mount ca-certificates udev xfsprogs
COPY --from=mad-hack /lib/udev/scsi_id /lib/udev_containerized/scsi_id

ENTRYPOINT ["/gce-pd-csi-driver"]
