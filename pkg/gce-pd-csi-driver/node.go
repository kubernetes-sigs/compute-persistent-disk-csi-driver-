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
	"os"
	"strconv"
	"strings"

	"context"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	csi "github.com/container-storage-interface/spec/lib/go/csi"

	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/util/mount"
	"k8s.io/kubernetes/pkg/util/resizefs"

	"sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/pkg/common"
	metadataservice "sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/pkg/gce-cloud-provider/metadata"
	mountmanager "sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/pkg/mount-manager"
)

type GCENodeServer struct {
	Driver          *GCEDriver
	Mounter         *mount.SafeFormatAndMount
	DeviceUtils     mountmanager.DeviceUtils
	MetadataService metadataservice.MetadataService

	// A map storing all volumes with ongoing operations so that additional operations
	// for that same volume (as defined by VolumeID) return an Aborted error
	volumeLocks *common.VolumeLocks
}

var _ csi.NodeServer = &GCENodeServer{}

// The constants are used to map from the machine type to the limit of
// persistent disks that can be attached to an instance. Please refer to gcloud
// doc https://cloud.google.com/compute/docs/disks/#pdnumberlimits
// These constants are all the documented attach limit minus one because the
// node boot disk is considered an attachable disk so effective attach limit is
// one less.
const (
	volumeLimitSmall int64 = 15
	volumeLimitBig   int64 = 127
)

func (ns *GCENodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	klog.V(4).Infof("NodePublishVolume called with req: %#v", req)

	// Validate Arguments
	targetPath := req.GetTargetPath()
	stagingTargetPath := req.GetStagingTargetPath()
	readOnly := req.GetReadonly()
	volumeID := req.GetVolumeId()
	volumeCapability := req.GetVolumeCapability()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume Volume ID must be provided")
	}
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume Staging Target Path must be provided")
	}
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume Target Path must be provided")
	}
	if volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume Volume Capability must be provided")
	}

	if acquired := ns.volumeLocks.TryAcquire(volumeID); !acquired {
		return nil, status.Errorf(codes.Aborted, common.VolumeOperationAlreadyExistsFmt, volumeID)
	}
	defer ns.volumeLocks.Release(volumeID)

	if err := validateVolumeCapability(volumeCapability); err != nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("VolumeCapability is invalid: %v", err))
	}

	notMnt, err := ns.Mounter.Interface.IsLikelyNotMountPoint(targetPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, status.Error(codes.Internal, fmt.Sprintf("cannot validate mount point: %s %v", targetPath, err))
	}
	if !notMnt {
		// TODO(#95): check if mount is compatible. Return OK if it is, or appropriate error.
		/*
			1) Target Path MUST be the vol referenced by vol ID
			2) TODO(#253): Check volume capability matches for ALREADY_EXISTS
			3) Readonly MUST match

		*/
		return &csi.NodePublishVolumeResponse{}, nil
	}

	// Perform a bind mount to the full path to allow duplicate mounts of the same PD.
	fstype := ""
	sourcePath := ""
	options := []string{"bind"}
	if readOnly {
		options = append(options, "ro")
	}

	if mnt := volumeCapability.GetMount(); mnt != nil {
		if mnt.FsType != "" {
			fstype = mnt.FsType
		} else {
			// Default fstype is ext4
			fstype = "ext4"
		}

		klog.V(4).Infof("NodePublishVolume with filesystem %s", fstype)

		for _, flag := range mnt.MountFlags {
			options = append(options, flag)
		}

		sourcePath = stagingTargetPath

		if err := ns.Mounter.Interface.MakeDir(targetPath); err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("mkdir failed on disk %s (%v)", targetPath, err))
		}
	} else if blk := volumeCapability.GetBlock(); blk != nil {
		klog.V(4).Infof("NodePublishVolume with block volume mode")

		partition := ""
		if part, ok := req.GetVolumeContext()[common.VolumeAttributePartition]; ok {
			partition = part
		}

		sourcePath, err = ns.getDevicePath(volumeID, partition)
		if err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Error when getting device path: %v", err))
		}

		// Expose block volume as file at target path
		err = ns.Mounter.MakeFile(targetPath)
		if err != nil {
			if removeErr := os.Remove(targetPath); removeErr != nil {
				return nil, status.Error(codes.Internal, fmt.Sprintf("Error removing block file at target path %v: %v, mounti error: %v", targetPath, removeErr, err))
			}
			return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to create block file at target path %v: %v", targetPath, err))
		}
	} else {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("NodePublishVolume volume capability must specify either mount or block mode"))
	}

	err = ns.Mounter.Interface.Mount(sourcePath, targetPath, fstype, options)
	if err != nil {
		notMnt, mntErr := ns.Mounter.Interface.IsLikelyNotMountPoint(targetPath)
		if mntErr != nil {
			klog.Errorf("IsLikelyNotMountPoint check failed: %v", mntErr)
			return nil, status.Error(codes.Internal, fmt.Sprintf("NodePublishVolume failed to check whether target path is a mount point: %v", err))
		}
		if !notMnt {
			if mntErr = ns.Mounter.Interface.Unmount(targetPath); mntErr != nil {
				klog.Errorf("Failed to unmount: %v", mntErr)
				return nil, status.Error(codes.Internal, fmt.Sprintf("NodePublishVolume failed to unmount target path: %v", err))
			}
			notMnt, mntErr := ns.Mounter.Interface.IsLikelyNotMountPoint(targetPath)
			if mntErr != nil {
				klog.Errorf("IsLikelyNotMountPoint check failed: %v", mntErr)
				return nil, status.Error(codes.Internal, fmt.Sprintf("NodePublishVolume failed to check whether target path is a mount point: %v", err))
			}
			if !notMnt {
				// This is very odd, we don't expect it.  We'll try again next sync loop.
				klog.Errorf("%s is still mounted, despite call to unmount().  Will try again next sync loop.", targetPath)
				return nil, status.Error(codes.Internal, fmt.Sprintf("NodePublishVolume something is wrong with mounting: %v", err))
			}
		}
		os.Remove(targetPath)
		klog.Errorf("Mount of disk %s failed: %v", targetPath, err)
		return nil, status.Error(codes.Internal, fmt.Sprintf("NodePublishVolume mount of disk failed: %v", err))
	}

	klog.V(4).Infof("Successfully mounted %s", targetPath)
	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *GCENodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	klog.V(4).Infof("NodeUnpublishVolume called with args: %v", req)

	// Validate Arguments
	targetPath := req.GetTargetPath()
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume Volume ID must be provided")
	}
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume Target Path must be provided")
	}

	if acquired := ns.volumeLocks.TryAcquire(volumeID); !acquired {
		return nil, status.Errorf(codes.Aborted, common.VolumeOperationAlreadyExistsFmt, volumeID)
	}
	defer ns.volumeLocks.Release(volumeID)

	err := mount.CleanupMountPoint(targetPath, ns.Mounter.Interface, false /* bind mount */)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Unmount failed: %v\nUnmounting arguments: %s\n", err, targetPath))
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *GCENodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	klog.V(4).Infof("NodeStageVolume called with req: %#v", req)

	// Validate Arguments
	volumeID := req.GetVolumeId()
	stagingTargetPath := req.GetStagingTargetPath()
	volumeCapability := req.GetVolumeCapability()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume Volume ID must be provided")
	}
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume Staging Target Path must be provided")
	}
	if volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume Volume Capability must be provided")
	}

	if acquired := ns.volumeLocks.TryAcquire(volumeID); !acquired {
		return nil, status.Errorf(codes.Aborted, common.VolumeOperationAlreadyExistsFmt, volumeID)
	}
	defer ns.volumeLocks.Release(volumeID)

	if err := validateVolumeCapability(volumeCapability); err != nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("VolumeCapability is invalid: %v", err))
	}

	// TODO(#253): Check volume capability matches for ALREADY_EXISTS

	volumeKey, err := common.VolumeIDToKey(volumeID)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("NodeStageVolume Volume ID is invalid: %v", err))
	}

	// Part 1: Get device path of attached device
	partition := ""

	if part, ok := req.GetVolumeContext()[common.VolumeAttributePartition]; ok {
		partition = part
	}

	devicePath, err := ns.getDevicePath(volumeID, partition)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Error when getting device path: %v", err))
	}

	klog.V(4).Infof("Successfully found attached GCE PD %q at device path %s.", volumeKey.Name, devicePath)

	// Part 2: Check if mount already exists at targetpath
	notMnt, err := ns.Mounter.Interface.IsLikelyNotMountPoint(stagingTargetPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := ns.Mounter.Interface.MakeDir(stagingTargetPath); err != nil {
				return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to create directory (%q): %v", stagingTargetPath, err))
			}
			notMnt = true
		} else {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Unknown error when checking mount point (%q): %v", stagingTargetPath, err))
		}
	}

	if !notMnt {
		// TODO(#95): Check who is mounted here. No error if its us
		/*
			1) Target Path MUST be the vol referenced by vol ID
			2) VolumeCapability MUST match
			3) Readonly MUST match

		*/
		return &csi.NodeStageVolumeResponse{}, nil

	}

	// Part 3: Mount device to stagingTargetPath
	// Default fstype is ext4
	fstype := "ext4"
	options := []string{}
	if mnt := volumeCapability.GetMount(); mnt != nil {
		if mnt.FsType != "" {
			fstype = mnt.FsType
		}
		for _, flag := range mnt.MountFlags {
			options = append(options, flag)
		}
	} else if blk := volumeCapability.GetBlock(); blk != nil {
		// Noop for Block NodeStageVolume
		return &csi.NodeStageVolumeResponse{}, nil
	}

	err = ns.Mounter.FormatAndMount(devicePath, stagingTargetPath, fstype, options)
	if err != nil {
		return nil, status.Error(codes.Internal,
			fmt.Sprintf("Failed to format and mount device from (%q) to (%q) with fstype (%q) and options (%q): %v",
				devicePath, stagingTargetPath, fstype, options, err))
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *GCENodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	klog.V(4).Infof("NodeUnstageVolume called with req: %#v", req)

	// Validate arguments
	volumeID := req.GetVolumeId()
	stagingTargetPath := req.GetStagingTargetPath()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeUnstageVolume Volume ID must be provided")
	}
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeUnstageVolume Staging Target Path must be provided")
	}

	if acquired := ns.volumeLocks.TryAcquire(volumeID); !acquired {
		return nil, status.Error(codes.Aborted, fmt.Sprintf("An operation with the given Volume ID %s already exists", volumeID))
	}
	defer ns.volumeLocks.Release(volumeID)

	err := mount.CleanupMountPoint(stagingTargetPath, ns.Mounter.Interface, false /* bind mount */)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("NodeUnstageVolume failed to unmount at path %s: %v", stagingTargetPath, err))
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *GCENodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	klog.V(4).Infof("NodeGetCapabilities called with req: %#v", req)

	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: ns.Driver.nscap,
	}, nil
}

func (ns *GCENodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	klog.V(4).Infof("NodeGetInfo called with req: %#v", req)

	top := &csi.Topology{
		Segments: map[string]string{common.TopologyKeyZone: ns.MetadataService.GetZone()},
	}

	nodeID := common.CreateNodeID(ns.MetadataService.GetProject(), ns.MetadataService.GetZone(), ns.MetadataService.GetName())

	volumeLimits, err := ns.GetVolumeLimits()

	resp := &csi.NodeGetInfoResponse{
		NodeId:             nodeID,
		MaxVolumesPerNode:  volumeLimits,
		AccessibleTopology: top,
	}
	return resp, err
}

func (ns *GCENodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	if len(req.VolumePath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeGetVolumeStats volume path was empty")
	}
	statfs := &unix.Statfs_t{}
	err := unix.Statfs(req.VolumePath, statfs)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get fs info on path %s: %v", req.VolumePath, err)
	}

	// Available is blocks available * fragment size
	available := int64(statfs.Bavail) * int64(statfs.Bsize)
	// Capacity is total block count * fragment size
	capacity := int64(statfs.Blocks) * int64(statfs.Bsize)
	// Usage is block being used * fragment size (aka block size).
	usage := (int64(statfs.Blocks) - int64(statfs.Bfree)) * int64(statfs.Bsize)
	inodes := int64(statfs.Files)
	inodesFree := int64(statfs.Ffree)
	inodesUsed := inodes - inodesFree

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Unit:      csi.VolumeUsage_BYTES,
				Available: available,
				Total:     capacity,
				Used:      usage,
			},
			{
				Unit:      csi.VolumeUsage_INODES,
				Available: inodesFree,
				Total:     inodes,
				Used:      inodesUsed,
			},
		},
	}, nil
}

func (ns *GCENodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ControllerExpandVolume volume ID must be provided")
	}
	capacityRange := req.GetCapacityRange()
	reqBytes, err := getRequestCapacity(capacityRange)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("ControllerExpandVolume capacity range is invalid: %v", err))
	}

	volumePath := req.GetVolumePath()
	if len(volumePath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ControllerExpandVolume volume path must be provided")
	}

	volKey, err := common.VolumeIDToKey(volumeID)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("ControllerExpandVolume Volume ID is invalid: %v", err))
	}

	devicePath, err := ns.getDevicePath(volumeID, "")
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("ControllerExpandVolume error when getting device path for %s: %v", volumeID, err))
	}

	// TODO(#328): Use requested size in resize if provided
	resizer := resizefs.NewResizeFs(ns.Mounter)
	_, err = resizer.Resize(devicePath, volumePath)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("ControllerExpandVolume error when resizing volume %s: %v", volKey.String(), err))

	}

	// Check the block size
	gotBlockSizeBytes, err := ns.getBlockSizeBytes(devicePath)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("ControllerExpandVolume error when getting size of block volume at path %s: %v", devicePath, err))
	}
	if gotBlockSizeBytes < reqBytes {
		// It's possible that the somewhere the volume size was rounded up, getting more size than requested is a success :)
		return nil, status.Errorf(codes.Internal, "ControllerExpandVolume resize requested for %v but after resize volume was size %v", reqBytes, gotBlockSizeBytes)
	}

	// TODO(dyzz) Some sort of formatted volume could also check the fs size.
	// Issue is that we don't know how to account for filesystem overhead, it
	// could be proportional to fs size and different for xfs, ext4 and we don't
	// know the proportions

	/*
		format, err := ns.Mounter.GetDiskFormat(devicePath)
		if err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("ControllerExpandVolume error checking format for device %s: %v", devicePath, err))
		}
		gotSizeBytes, err = ns.getFSSizeBytes(devicePath)

		if err != nil {
			return nil, status.Errorf(codes.Internal, "ControllerExpandVolume resize could not get fs size of %s: %v", volumePath, err)
		}
		if gotSizeBytes != reqBytes {
			return nil, status.Errorf(codes.Internal, "ControllerExpandVolume resize requested for size %v but after resize volume was size %v", reqBytes, gotSizeBytes)
		}
	*/

	// Respond
	return &csi.NodeExpandVolumeResponse{
		CapacityBytes: reqBytes,
	}, nil
}

func (ns *GCENodeServer) GetVolumeLimits() (int64, error) {
	var volumeLimits int64

	// Machine-type format: n1-type-CPUS or custom-CPUS-RAM or f1/g1-type
	machineType := ns.MetadataService.GetMachineType()
	if strings.HasPrefix(machineType, "n1-") || strings.HasPrefix(machineType, "custom-") {
		volumeLimits = volumeLimitBig
	} else {
		volumeLimits = volumeLimitSmall
	}
	return volumeLimits, nil
}

func (ns *GCENodeServer) getDevicePath(volumeID string, partition string) (string, error) {
	volumeKey, err := common.VolumeIDToKey(volumeID)
	if err != nil {
		return "", err
	}
	deviceName, err := common.GetDeviceName(volumeKey)
	if err != nil {
		return "", fmt.Errorf("error getting device name: %v", err)
	}

	devicePaths := ns.DeviceUtils.GetDiskByIdPaths(deviceName, partition)
	devicePath, err := ns.DeviceUtils.VerifyDevicePath(devicePaths)

	if err != nil {
		return "", fmt.Errorf("error verifying GCE PD (%q) is attached: %v", volumeKey.Name, err)
	}
	if devicePath == "" {
		return "", fmt.Errorf("unable to find device path out of attempted paths: %v", devicePaths)
	}
	return devicePath, nil
}

func (ns *GCENodeServer) getBlockSizeBytes(devicePath string) (int64, error) {
	output, err := ns.Mounter.Exec.Run("blockdev", "--getsize64", devicePath)
	if err != nil {
		return -1, fmt.Errorf("error when getting size of block volume at path %s: output: %s, err: %v", devicePath, string(output), err)
	}
	strOut := strings.TrimSpace(string(output))
	gotSizeBytes, err := strconv.ParseInt(strOut, 10, 64)
	if err != nil {
		return -1, fmt.Errorf("failed to parse size %s into int a size", strOut)
	}
	return gotSizeBytes, nil
}
