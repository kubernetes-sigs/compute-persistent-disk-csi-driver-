// +build windows

/*
Copyright 2020 The Kubernetes Authors.

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

package mountmanager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	diskv1alpha1 "github.com/kubernetes-csi/csi-proxy/client/api/disk/v1alpha1"
	diskclientv1alpha1 "github.com/kubernetes-csi/csi-proxy/client/groups/disk/v1alpha1"

	fsv1alpha1 "github.com/kubernetes-csi/csi-proxy/client/api/filesystem/v1alpha1"
	fsclientv1alpha1 "github.com/kubernetes-csi/csi-proxy/client/groups/filesystem/v1alpha1"

	volumev1alpha1 "github.com/kubernetes-csi/csi-proxy/client/api/volume/v1alpha1"
	volumeclientv1alpha1 "github.com/kubernetes-csi/csi-proxy/client/groups/volume/v1alpha1"

	utilexec "k8s.io/utils/exec"
	"k8s.io/utils/mount"
)

var _ mount.Interface = &CSIProxyMounter{}

type CSIProxyMounter struct {
	FsClient     *fsclientv1alpha1.Client
	DiskClient   *diskclientv1alpha1.Client
	VolumeClient *volumeclientv1alpha1.Client
}

func NewCSIProxyMounter() (*CSIProxyMounter, error) {
	fsClient, err := fsclientv1alpha1.NewClient()
	if err != nil {
		return nil, err
	}
	diskClient, err := diskclientv1alpha1.NewClient()
	if err != nil {
		return nil, err
	}
	volumeClient, err := volumeclientv1alpha1.NewClient()
	if err != nil {
		return nil, err
	}
	return &CSIProxyMounter{
		FsClient:     fsClient,
		DiskClient:   diskClient,
		VolumeClient: volumeClient,
	}, nil
}

func NewSafeMounter() (*mount.SafeFormatAndMount, error) {
	csiProxyMounter, err := NewCSIProxyMounter()
	if err != nil {
		return nil, err
	}
	return &mount.SafeFormatAndMount{
		Interface: csiProxyMounter,
		Exec:      utilexec.New(),
	}, nil
}

// Mount just creates a soft link at target pointing to source.
func (mounter *CSIProxyMounter) Mount(source string, target string, fstype string, options []string) error {
	// Mount is called after the format is done.
	// TODO: Confirm that fstype is empty.
	// Call the LinkPath CSI proxy from the source path to the target path
	parentDir := filepath.Dir(target)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return err
	}
	linkRequest := &fsv1alpha1.LinkPathRequest{
		SourcePath: mount.NormalizeWindowsPath(source),
		TargetPath: mount.NormalizeWindowsPath(target),
	}
	response, err := mounter.FsClient.LinkPath(context.Background(), linkRequest)
	if err != nil {
		return err
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	return nil
}

// Delete the given directory with Pod context. CSI proxy does a check for path prefix
// based on context
func (mounter *CSIProxyMounter) RemovePodDir(target string) error {
	rmdirRequest := &fsv1alpha1.RmdirRequest{
		Path:    mount.NormalizeWindowsPath(target),
		Context: fsv1alpha1.PathContext_POD,
		Force:   true,
	}
	_, err := mounter.FsClient.Rmdir(context.Background(), rmdirRequest)
	if err != nil {
		return err
	}
	return nil
}

// Delete the given directory with plugin context. CSI proxy does a check for path prefix
// based on context
func (mounter *CSIProxyMounter) RemovePluginDir(target string) error {
	rmdirRequest := &fsv1alpha1.RmdirRequest{
		Path:    mount.NormalizeWindowsPath(target),
		Context: fsv1alpha1.PathContext_PLUGIN,
		Force:   true,
	}
	_, err := mounter.FsClient.Rmdir(context.Background(), rmdirRequest)
	if err != nil {
		return err
	}
	return nil
}

func (mounter *CSIProxyMounter) Unmount(target string) error {
	return mounter.RemovePodDir(target)
}

func (mounter *CSIProxyMounter) GetDevicePath(deviceName string, partition string, volumeKey string) (string, error) {
	getDiskNumberRequest := &diskv1alpha1.GetDiskNumberByNameRequest{
		DiskName: deviceName,
	}
	getDiskNumberResponse, err := mounter.DiskClient.GetDiskNumberByName(context.Background(), getDiskNumberRequest)
	if err != nil {
		return "", err
	}
	return getDiskNumberResponse.DiskNumber, nil

}

// FormatAndMount accepts the source disk number, target path to mount, the fstype to format with and options to be used.
// After formatting, it will mount the disk to target path on the host
func (mounter *CSIProxyMounter) FormatAndMount(source string, target string, fstype string, options []string) error {
	// Call PartitionDisk CSI proxy call to partition the disk and return the volume id
	partionDiskRequest := &diskv1alpha1.PartitionDiskRequest{
		DiskID: source,
	}

	_, err := mounter.DiskClient.PartitionDisk(context.Background(), partionDiskRequest)
	if err != nil {
		return err
	}
	volumeIDsRequest := &volumev1alpha1.ListVolumesOnDiskRequest{
		DiskId: source,
	}
	volumeIdResponse, err := mounter.VolumeClient.ListVolumesOnDisk(context.Background(), volumeIDsRequest)
	if err != nil {
		return err
	}
	// TODO: consider partitions and choose the right partition.
	volumeID := volumeIdResponse.VolumeIds[0]
	isVolumeFormattedRequest := &volumev1alpha1.IsVolumeFormattedRequest{
		VolumeId: volumeID,
	}
	isVolumeFormattedResponse, err := mounter.VolumeClient.IsVolumeFormatted(context.Background(), isVolumeFormattedRequest)
	if err != nil {
		return err
	}
	if !isVolumeFormattedResponse.Formatted {
		formatVolumeRequest := &volumev1alpha1.FormatVolumeRequest{
			VolumeId: volumeID,
			// TODO (jingxu97): Accept the filesystem and other options
		}
		_, err = mounter.VolumeClient.FormatVolume(context.Background(), formatVolumeRequest)
		if err != nil {
			return err
		}
	}
	// Mount the volume by calling the CSI proxy call.
	mountVolumeRequest := &volumev1alpha1.MountVolumeRequest{
		VolumeId: volumeID,
		Path:     target,
	}
	_, err = mounter.VolumeClient.MountVolume(context.Background(), mountVolumeRequest)
	if err != nil {
		return err
	}
	return nil
}

func (mounter *CSIProxyMounter) GetMountRefs(pathname string) ([]string, error) {
	return []string{}, fmt.Errorf("GetMountRefs not implemented for ProxyMounter")
}

func (mounter *CSIProxyMounter) IsLikelyNotMountPoint(file string) (bool, error) {
	isMountRequest := &fsv1alpha1.IsMountPointRequest{
		Path: file,
	}

	isMountResponse, err := mounter.FsClient.IsMountPoint(context.Background(), isMountRequest)
	if err != nil {
		return false, err
	}

	return !isMountResponse.IsMountPoint, nil
}

func (mounter *CSIProxyMounter) List() ([]mount.MountPoint, error) {
	return []mount.MountPoint{}, nil
}

func (mounter *CSIProxyMounter) IsMountPointMatch(mp mount.MountPoint, dir string) bool {
	return mp.Path == dir
}

// ExistsPath - Checks if a path exists. Unlike util ExistsPath, this call does not perform follow link.
func (mounter *CSIProxyMounter) ExistsPath(path string) (bool, error) {
	isExistsResponse, err := mounter.FsClient.PathExists(context.Background(),
		&fsv1alpha1.PathExistsRequest{
			Path: mount.NormalizeWindowsPath(path),
		})
	return isExistsResponse.Exists, err
}
