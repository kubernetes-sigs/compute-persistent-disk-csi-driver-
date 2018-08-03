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

package gceGCEDriver

import (
	"fmt"
	"math/rand"
	"strings"
	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi/v0"
	"github.com/golang/glog"
	"golang.org/x/net/context"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/pkg/common"
	gce "sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/pkg/gce-cloud-provider/compute"
)

// TODO: Add noisy glog.V(5).Infof() EVERYWHERE
// TODO: Improve errors to only expose codes at top level
// TODO: Improve error prefix to explicitly state what function it is in.

type GCEControllerServer struct {
	Driver        *GCEDriver
	CloudProvider gce.GCECompute
}

var _ csi.ControllerServer = &GCEControllerServer{}

const (
	// MaxVolumeSizeInBytes is the maximum standard and ssd size of 64TB
	MaxVolumeSizeInBytes     int64 = 64 * 1024 * 1024 * 1024 * 1024
	MinimumVolumeSizeInBytes int64 = 5 * 1024 * 1024 * 1024
	MinimumDiskSizeInGb            = 5

	DiskTypeSSD      = "pd-ssd"
	DiskTypeStandard = "pd-standard"
	diskTypeDefault  = DiskTypeStandard

	attachableDiskTypePersistent = "PERSISTENT"
)

func (gceCS *GCEControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	glog.Infof("CreateVolume called with request %v", *req)

	// Validate arguments
	volumeCapabilities := req.GetVolumeCapabilities()
	name := req.GetName()
	capacityRange := req.GetCapacityRange()
	if len(name) == 0 {
		return nil, status.Error(codes.InvalidArgument, "CreateVolume Name must be provided")
	}
	if volumeCapabilities == nil || len(volumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, "CreateVolume Volume capabilities must be provided")
	}

	capBytes, err := getRequestCapacity(capacityRange)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("CreateVolume Request Capacity is invalid: %v", err))
	}

	// TODO: Validate volume capabilities

	// TODO: Support replica zones and fs type. Can vendor in api-machinery stuff for sets etc.
	// Apply Parameters (case-insensitive). We leave validation of
	// the values to the cloud provider.
	diskType := "pd-standard"
	var configuredZone string

	for k, v := range req.GetParameters() {
		if k == "csiProvisionerSecretName" || k == "csiProvisionerSecretNamespace" {
			// These are hardcoded secrets keys required to function but not needed by GCE PD
			continue
		}
		switch strings.ToLower(k) {
		case common.ParameterKeyType:
			glog.Infof("Setting type: %v", v)
			diskType = v
		case common.ParameterKeyZone:
			configuredZone = v
		default:
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("invalid option %q", k))
		}
	}

	if req.GetAccessibilityRequirements() != nil {
		if len(configuredZone) != 0 {
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("CreateVolume only one of parameter zone or topology zone may be specified"))
		}
		configuredZone, err = pickTopology(req.GetAccessibilityRequirements())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("CreateVolume failed to pick topology: %v", err))
		}
	}

	if len(configuredZone) == 0 {
		// Default to zone that the driver is in
		configuredZone = gceCS.CloudProvider.GetZone()
	}

	createResp := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			CapacityBytes: capBytes,
			Id:            common.CombineVolumeId(configuredZone, name),
			// TODO: Are there any attributes we need to add. These get sent to ControllerPublishVolume
			Attributes: nil,
			AccessibleTopology: []*csi.Topology{
				{
					Segments: map[string]string{common.TopologyKeyZone: configuredZone},
				},
			},
		},
	}

	// Check for existing disk of same name in same zone
	exists, err := gceCS.CloudProvider.GetAndValidateExistingDisk(ctx, configuredZone,
		name, diskType,
		capacityRange.GetRequiredBytes(),
		capacityRange.GetLimitBytes())
	if err != nil {
		return nil, err
	}
	if exists {
		glog.Warningf("GCE PD %s already exists, reusing", name)
		return createResp, nil
	}

	sizeGb := common.BytesToGb(capBytes)
	if sizeGb < MinimumDiskSizeInGb {
		sizeGb = MinimumDiskSizeInGb
	}
	diskToCreate := &compute.Disk{
		Name:        name,
		SizeGb:      sizeGb,
		Description: "Disk created by GCE-PD CSI Driver",
		Type:        gceCS.CloudProvider.GetDiskTypeURI(configuredZone, diskType),
	}

	insertOp, err := gceCS.CloudProvider.InsertDisk(ctx, configuredZone, diskToCreate)

	if err != nil {
		if gce.IsGCEError(err, "alreadyExists") {
			_, err := gceCS.CloudProvider.GetAndValidateExistingDisk(ctx, configuredZone,
				name, diskType,
				capacityRange.GetRequiredBytes(),
				capacityRange.GetLimitBytes())
			if err != nil {
				return nil, err
			}
			glog.Warningf("GCE PD %s already exists, reusing", name)
			return createResp, nil
		}
		return nil, status.Error(codes.Internal, fmt.Sprintf("unkown Insert disk error: %v", err))
	}

	err = gceCS.CloudProvider.WaitForOp(ctx, insertOp, configuredZone)

	if err != nil {
		if gce.IsGCEError(err, "alreadyExists") {
			_, err := gceCS.CloudProvider.GetAndValidateExistingDisk(ctx, configuredZone,
				name, diskType,
				capacityRange.GetRequiredBytes(),
				capacityRange.GetLimitBytes())
			if err != nil {
				return nil, err
			}
			glog.Warningf("GCE PD %s already exists after wait, reusing", name)
			return createResp, nil
		}
		return nil, status.Error(codes.Internal, fmt.Sprintf("unkown Insert disk operation error: %v", err))
	}

	glog.Infof("Completed creation of disk %v", name)
	return createResp, nil
}

func (gceCS *GCEControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	// TODO: Only allow deletion of volumes that were created by the driver
	// Assuming ID is of form {zone}/{id}
	glog.Infof("DeleteVolume called with request %v", *req)

	// Validate arguments
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "DeleteVolume Volume ID must be provided")
	}

	zone, name, err := common.SplitZoneNameId(volumeID)
	if err != nil {
		// Cannot find volume associated with this ID because can't even get the name or zone
		// This is a success according to the spec
		return &csi.DeleteVolumeResponse{}, nil
	}

	deleteOp, err := gceCS.CloudProvider.DeleteDisk(ctx, zone, name)
	if err != nil {
		if gce.IsGCEError(err, "resourceInUseByAnotherResource") {
			return nil, status.Error(codes.FailedPrecondition, fmt.Sprintf("Volume in use: %v", err))
		}
		if gce.IsGCEError(err, "notFound") {
			// Already deleted
			return &csi.DeleteVolumeResponse{}, nil
		}
		return nil, status.Error(codes.Internal, fmt.Sprintf("unknown Delete disk error: %v", err))
	}

	err = gceCS.CloudProvider.WaitForOp(ctx, deleteOp, zone)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("unknown Delete disk operation error: %v", err))
	}

	return &csi.DeleteVolumeResponse{}, nil
}

func (gceCS *GCEControllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	glog.Infof("ControllerPublishVolume called with request %v", *req)

	// Validate arguments
	volumeID := req.GetVolumeId()
	readOnly := req.GetReadonly()
	nodeID := req.GetNodeId()
	volumeCapability := req.GetVolumeCapability()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Volume ID must be provided")
	}
	if len(nodeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Node ID must be provided")
	}
	if volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Volume capability must be provided")
	}

	volumeZone, volumeName, err := common.SplitZoneNameId(volumeID)
	if err != nil {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("Could not find volume with ID %v: %v", volumeID, err))
	}

	// TODO: Check volume capability matches

	pubVolResp := &csi.ControllerPublishVolumeResponse{
		// TODO: Info gets sent to NodePublishVolume. Send something if necessary.
		PublishInfo: nil,
	}

	disk, err := gceCS.CloudProvider.GetDiskOrError(ctx, volumeZone, volumeName)
	if err != nil {
		return nil, err
	}
	instance, err := gceCS.CloudProvider.GetInstanceOrError(ctx, volumeZone, nodeID)
	if err != nil {
		if gce.IsGCEError(err, "notFound") {
			return nil, status.Error(codes.NotFound, fmt.Sprintf("Could not find instance %v: %v", nodeID, err))
		}
		return nil, status.Error(codes.Internal, fmt.Sprintf("Unknown get instance error: %v", err))
	}

	readWrite := "READ_WRITE"
	if readOnly {
		readWrite = "READ_ONLY"
	}

	attached, err := diskIsAttachedAndCompatible(disk, instance, volumeCapability, readWrite)
	if err != nil {
		return nil, status.Error(codes.AlreadyExists, fmt.Sprintf("Disk %v already published to node %v but incompatbile: %v", volumeName, nodeID, err))
	}
	if attached {
		// Volume is attached to node. Success!
		glog.Infof("Attach operation is successful. PD %q was already attached to node %q.", volumeName, nodeID)
		return pubVolResp, nil
	}

	source := gceCS.CloudProvider.GetDiskSourceURI(disk, volumeZone)

	attachedDiskV1 := &compute.AttachedDisk{
		DeviceName: disk.Name,
		Kind:       disk.Kind,
		Mode:       readWrite,
		Source:     source,
		Type:       attachableDiskTypePersistent,
	}

	glog.Infof("Attaching disk %#v to instance %v", attachedDiskV1, nodeID)
	attachOp, err := gceCS.CloudProvider.AttachDisk(ctx, volumeZone, nodeID, attachedDiskV1)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("unknown Attach error: %v", err))
	}

	glog.Infof("Waiting for attach of disk %v to instance %v to complete...", disk.Name, nodeID)
	err = gceCS.CloudProvider.WaitForOp(ctx, attachOp, volumeZone)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("unknown Attach operation error: %v", err))
	}

	err = gceCS.CloudProvider.WaitForAttach(ctx, volumeZone, disk.Name, nodeID)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("unknown WaitForAttach error: %v", err))
	}

	glog.Infof("Disk %v attached to instance %v successfully", disk.Name, nodeID)
	return pubVolResp, nil
}

func (gceCS *GCEControllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	glog.Infof("ControllerUnpublishVolume called with request %v", *req)

	// Validate arguments
	volumeID := req.GetVolumeId()
	nodeID := req.GetNodeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ControllerUnpublishVolume Volume ID must be provided")
	}
	if len(nodeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ControllerUnpublishVolume Node ID must be provided")
	}

	volumeZone, volumeName, err := common.SplitZoneNameId(volumeID)
	if err != nil {
		return nil, err
	}

	disk, err := gceCS.CloudProvider.GetDiskOrError(ctx, volumeZone, volumeName)
	if err != nil {
		return nil, err
	}
	instance, err := gceCS.CloudProvider.GetInstanceOrError(ctx, volumeZone, nodeID)
	if err != nil {
		return nil, err
	}

	attached := diskIsAttached(disk, instance)

	if !attached {
		// Volume is not attached to node. Success!
		glog.Infof("Detach operation is successful. PD %q was not attached to node %q.", volumeName, nodeID)
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	detachOp, err := gceCS.CloudProvider.DetachDisk(ctx, volumeZone, nodeID, volumeName)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("unknown detach error: %v", err))
	}

	err = gceCS.CloudProvider.WaitForOp(ctx, detachOp, volumeZone)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("unknown detach operation error: %v", err))
	}

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (gceCS *GCEControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	// TODO: Factor out the volume capability functionality and use as validation in all other functions as well
	glog.V(5).Infof("Using default ValidateVolumeCapabilities")
	// Validate Arguments
	if req.GetVolumeCapabilities() == nil || len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ValidateVolumeCapabilities Volume Capabilities must be provided")
	}
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ValidateVolumeCapabilities Volume ID must be provided")
	}
	z, n, err := common.SplitZoneNameId(volumeID)
	if err != nil {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("Volume ID is of improper format, got %v", volumeID))
	}
	_, err = gceCS.CloudProvider.GetDiskOrError(ctx, z, n)
	if err != nil {
		if gce.IsGCEError(err, "notFound") {
			return nil, status.Error(codes.NotFound, fmt.Sprintf("Could not find disk %v: %v", n, err))
		}
		return nil, status.Error(codes.Internal, fmt.Sprintf("Unknown get disk error: %v", err))
	}

	for _, c := range req.GetVolumeCapabilities() {
		found := false
		for _, c1 := range gceCS.Driver.vcap {
			if c1.Mode == c.GetAccessMode().Mode {
				found = true
			}
		}
		if !found {
			return &csi.ValidateVolumeCapabilitiesResponse{
				Supported: false,
				Message:   "Driver does not support mode:" + c.GetAccessMode().Mode.String(),
			}, status.Error(codes.InvalidArgument, "Driver does not support mode:"+c.GetAccessMode().Mode.String())
		}
		// TODO: Ignoring mount & block types for now.
	}

	for _, top := range req.GetAccessibleTopology() {
		for k, v := range top.GetSegments() {
			switch k {
			case common.TopologyKeyZone:
				// take the zone from v and see if it matches with zone
				if v == z {
					// Accessible zone matches with storage zone
					return &csi.ValidateVolumeCapabilitiesResponse{
						Supported: true,
					}, nil
				} else {
					// Accessible zone does not match
					return &csi.ValidateVolumeCapabilitiesResponse{
						Supported: false,
						Message:   fmt.Sprintf("Volume %s is not accesible from topology %s:%s", volumeID, k, v),
					}, nil
				}
			default:
				return nil, status.Error(codes.InvalidArgument, "ValidateVolumeCapabilities unknown topology segment key")
			}
		}
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Supported: true,
	}, nil
}

func (gceCS *GCEControllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	// https://cloud.google.com/compute/docs/reference/beta/disks/list
	// List volumes in the whole region? In only the zone that this controller is running?
	return nil, status.Error(codes.Unimplemented, "")
}

func (gceCS *GCEControllerServer) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	// https://cloud.google.com/compute/quotas
	// DISKS_TOTAL_GB.
	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerGetCapabilities implements the default GRPC callout.
func (gceCS *GCEControllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: gceCS.Driver.cscap,
	}, nil
}

// CreatSnapshot sends a request to create snapshot to the cloud provider and then waits until the snapshot is created to return the snapshot object back.
func (gceCS *GCEControllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	glog.Infof("CreateSnapshot called with request %v", *req)
	name := req.GetName()
	sourceId := req.GetSourceVolumeId()
	volumeZone, volumeName, err := utils.SplitZoneNameId(sourceId)
	if err != nil {
		return nil, err
	}
	configuredZone := gceCS.CloudProvider.GetZone()
	glog.Infof("volumezone %s, configuredZone %s", volumeZone, configuredZone)

	if volumeZone != configuredZone {
		return nil, fmt.Errorf("volumezone %s does not match configuredZone %s", volumeZone, configuredZone)
	}
	snapshotToCreate := &compute.Snapshot{
		Name:        name,
	}

	createOp, err := gceCS.CloudProvider.CreateSnapshot(ctx, volumeZone, volumeName, snapshotToCreate)
	if createOp != nil {
		glog.Infof("Create snapshot operation %v, err %v", createOp, err)
	}
	if err != nil {
		if gce.IsGCEError(err, "alreadyExists") {
			glog.Warningf("GCE snapshot %s already exists, reusing", name)
		} else {
			return nil, status.Error(codes.Internal, fmt.Sprintf("unkown create snapshot error: %v", err))
		}
	}

	snapshot, err := gceCS.CloudProvider.WaitAndGetSnapshot(ctx, name)
	if err != nil {
		glog.Warning("Fail to get snapshot %s, %v", name, err)
		return nil, status.Error(codes.Internal, fmt.Sprintf("unkown create snapshot operation error: %v", err))
	}

	t, err := time.Parse(time.RFC3339, snapshot.CreationTimestamp)
	if err !=nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("unkown create snapshot operation error: %v", err))
	}
	glog.Infof("snapshot timestamp %d", t.UnixNano())
	var status csi.SnapshotStatus_Type
	switch snapshot.Status {
	case "READY":
			status = csi.SnapshotStatus_READY
	case "UPLOADING":
			status = csi.SnapshotStatus_UPLOADING
	case "FAILED":
			status = csi.SnapshotStatus_ERROR_UPLOADING
	case "DELETING":
		status = csi.SnapshotStatus_READY
		glog.Infof("snapshot is in DELETING")
	default:
		status = csi.SnapshotStatus_UNKNOWN
	}

	createResp := &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			Id: snapshot.Name,
			CreatedAt: t.UnixNano(),
			Status: &csi.SnapshotStatus{
				Type: status,
			},
		},
	}
	glog.V(2).Infof("Completed creation of snapshot %v", createResp)
	return createResp, nil
}

func (gceCS *GCEControllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	glog.Infof("Delete called with request %v", *req)
	snapshotID := req.GetSnapshotId()
	if len(snapshotID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "DeleteSnapshot Snapshot ID must be provided")
	}

	deleteOp, err := gceCS.CloudProvider.DeleteSnapshot(ctx, snapshotID)
	if err != nil {
		if gce.IsGCEError(err, "resourceInUseByAnotherResource") {
			return nil, status.Error(codes.FailedPrecondition, fmt.Sprintf("Volume in use: %v", err))
		}
		if gce.IsGCEError(err, "notFound") {
			// Already deleted
			return &csi.DeleteSnapshotResponse{}, nil
		}
		return nil, status.Error(codes.Internal, fmt.Sprintf("unknown Delete disk error: %v", err))
	}

	err = gceCS.CloudProvider.WaitForGlobalOp(ctx, deleteOp)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("unknown Delete disk operation error: %v", err))
	}

	return &csi.DeleteSnapshotResponse{}, nil
}

func (gceCS *GCEControllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	glog.Infof("ListSnapshots called with request %v", *req)
	if snapshotId := req.GetSnapshotId(); snapshotId != "" {
	
		snapshot, err := gceCS.CloudProvider.GetSnapshotOrError(ctx, snapshotId)
		if err!= nil {
			return nil, err
		}
		var status csi.SnapshotStatus_Type
		switch snapshot.Status {
		case "READY":
				status = csi.SnapshotStatus_READY
		case "UPLOADING":
				status = csi.SnapshotStatus_UPLOADING
		case "FAILED":
				status = csi.SnapshotStatus_ERROR_UPLOADING
		}
		now := time.Now()
		entry := &csi.ListSnapshotsResponse_Entry {
			Snapshot: &csi.Snapshot{
				Id: snapshot.Name,
				CreatedAt: now.UnixNano(),
				Status: &csi.SnapshotStatus{
					Type: status,
				},
			},
		}
		entries := []*csi.ListSnapshotsResponse_Entry {entry}
		listSnapshotResp := &csi.ListSnapshotsResponse {
			Entries: entries,
		}
		return listSnapshotResp, nil
	}
	return nil, status.Error(codes.Unimplemented, "")
}

func getRequestCapacity(capRange *csi.CapacityRange) (int64, error) {
	// TODO: Take another look at these casts/caps. Make sure this func is correct
	var capBytes int64
	// Default case where nothing is set
	if capRange == nil {
		capBytes = MinimumVolumeSizeInBytes
		return capBytes, nil
	}

	rBytes := capRange.GetRequiredBytes()
	rSet := rBytes > 0
	lBytes := capRange.GetLimitBytes()
	lSet := lBytes > 0

	if lSet && rSet && lBytes < rBytes {
		return 0, fmt.Errorf("Limit bytes %v is less than required bytes %v", lBytes, rBytes)
	}
	if lSet && lBytes < MinimumVolumeSizeInBytes {
		return 0, fmt.Errorf("Limit bytes %v is less than minimum volume size: %v", lBytes, MinimumVolumeSizeInBytes)
	}

	// If Required set just set capacity to that which is Required
	if rSet {
		capBytes = rBytes
	}

	// Limit is more than Required, but larger than Minimum. So we just set capcity to Minimum
	// Too small, default
	if capBytes < MinimumVolumeSizeInBytes {
		capBytes = MinimumVolumeSizeInBytes
	}
	return capBytes, nil
}

func diskIsAttached(volume *compute.Disk, instance *compute.Instance) bool {
	for _, disk := range instance.Disks {
		if disk.DeviceName == volume.Name {
			// Disk is attached to node
			return true
		}
	}
	return false
}

func diskIsAttachedAndCompatible(volume *compute.Disk, instance *compute.Instance, volumeCapability *csi.VolumeCapability, readWrite string) (bool, error) {
	for _, disk := range instance.Disks {
		if disk.DeviceName == volume.Name {
			// Disk is attached to node
			if disk.Mode != readWrite {
				return true, fmt.Errorf("disk mode does not match. Got %v. Want %v", disk.Mode, readWrite)
			}
			// TODO: Check volume_capability.
			return true, nil
		}
	}
	return false, nil
}

func pickTopology(top *csi.TopologyRequirement) (string, error) {
	reqTop := top.GetRequisite()
	prefTop := top.GetPreferred()

	// Pick the preferred topology in order
	if len(prefTop) != 0 {
		if prefTop[0].GetSegments() == nil {
			return "", fmt.Errorf("preferred topologies specified but no segments")
		}

		// GCE PD cloud provider Create has no restrictions so just create in top preferred zone
		zone, err := getZoneFromSegment(prefTop[0].GetSegments())
		if err != nil {
			return "", fmt.Errorf("could not get zone from preferred topology: %v", err)
		}
		return zone, nil
	} else if len(reqTop) != 0 {
		r := rand.Intn(len(reqTop))
		if reqTop[r].GetSegments() == nil {
			return "", fmt.Errorf("requisite topologies specified but no segments in requisite topology %v", r)
		}

		zone, err := getZoneFromSegment(reqTop[r].GetSegments())
		if err != nil {
			return "", fmt.Errorf("could not get zone from requisite topology: %v", err)
		}
		return zone, nil
	} else {
		return "", fmt.Errorf("accessibility requirements specified but no requisite or preferred topologies")
	}

}

func getZoneFromSegment(seg map[string]string) (string, error) {
	var zone string
	for k, v := range seg {
		switch k {
		case common.TopologyKeyZone:
			zone = v
		default:
			return "", fmt.Errorf("topology segment has unknown key %v", k)
		}
	}
	if len(zone) == 0 {
		return "", fmt.Errorf("topology specified but could not find zone in segment: %v", seg)
	}
	return zone, nil
}
