/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package common

const (
	// Keys for Storage Class Parameters
	ParameterKeyType                 = "type"
	ParameterKeyReplicationType      = "replication-type"
	ParameterKeyDiskEncryptionKmsKey = "disk-encryption-kms-key"

	// Keys for Topology. This key will be shared amongst drivers from GCP
	TopologyKeyZone           = "topology.gke.io/zone"
	KubernetesTopologyKeyZone = "failure-domain.beta.kubernetes.io/zone"

	// VolumeAttributes for Partition
	VolumeAttributePartition = "partition"

	UnspecifiedValue = "UNSPECIFIED"
)
