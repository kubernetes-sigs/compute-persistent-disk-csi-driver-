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

package gcecloudprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud/meta"
	csi "github.com/container-storage-interface/spec/lib/go/csi"
	computebeta "google.golang.org/api/compute/v0.beta"
	computev1 "google.golang.org/api/compute/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
	"sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/pkg/common"
)

const (
	operationStatusDone            = "DONE"
	waitForSnapshotCreationTimeOut = 2 * time.Minute
	diskKind                       = "compute#disk"
	cryptoKeyVerDelimiter          = "/cryptoKeyVersions"
)

type GCEAPIVersion string

const (
	// V1 key type
	GCEAPIVersionV1 GCEAPIVersion = "v1"
	// Alpha key type
	GCEAPIVersionBeta GCEAPIVersion = "beta"
	// OpPollInterval is the ineterval between polling an operation
	OpPollInterval = 1 * time.Second
	// OpTimeout is the default total timeout for polling on an operation
	OpTimeout = 2 * time.Minute
	// defaultCallTimeout is the ddefault context timeout for creating/getting on an operation
	defaultCallTimeout = 10 * time.Minute
	// Time interval for checking on an operation
	AttachDiskInitialDelay = 6 * time.Second

	InsertDiskInitialDelay = 3 * time.Second

	// volumeAttachmentConsecutiveErrorLimit is the number of consecutive errors we will ignore when waiting for a volume to attach/detach
	//volumeAttachmentStatusConsecutiveErrorLimit = 10

	// Attach typically takes 2-5 seconds (average is 2). Asking before 2 seconds is just waste of API quota.
	volumeAttachmentStatusInitialDelay = 2 * time.Second
	// Detach typically takes 5-10 seconds (average is 6). Asking before 5 seconds is just waste of API quota.
	volumeDetachmentStatusInitialDelay = 5 * time.Second
	// After the initial delay, poll attach/detach with exponential backoff (2046 seconds total)
	volumeAttachmentStatusPollDelay = 2 * time.Second
	volumeAttachmentStatusFactor    = 2
	volumeAttachmentStatusSteps     = 11
)

type GCECompute interface {
	// Metadata information
	GetDefaultProject() string
	GetDefaultZone() string
	// Disk Methods
	GetDisk(ctx context.Context, volumeKey *meta.Key, gceAPIVersion GCEAPIVersion) (*CloudDisk, error)
	RepairUnderspecifiedVolumeKey(ctx context.Context, volumeKey *meta.Key) (*meta.Key, error)
	ValidateExistingDisk(ctx context.Context, disk *CloudDisk, params common.DiskParameters, reqBytes, limBytes int64, multiWriter bool) error
	InsertDisk(ctx context.Context, volKey *meta.Key, params common.DiskParameters, capBytes int64, capacityRange *csi.CapacityRange, replicaZones []string, snapshotID string, multiWriter bool) error
	DeleteDisk(ctx context.Context, volumeKey *meta.Key) error
	AttachDisk(ctx context.Context, volKey *meta.Key, readWrite, diskType, instanceZone, instanceName string) error
	DetachDisk(ctx context.Context, deviceName string, instanceZone, instanceName string) error
	GetDiskSourceURI(volKey *meta.Key) string
	GetDiskTypeURI(volKey *meta.Key, diskType string) string
	WaitForAttach(ctx context.Context, volKey *meta.Key, instanceZone, instanceName string) error
	ResizeDisk(ctx context.Context, volKey *meta.Key, requestBytes int64) (int64, error)
	ListDisks(ctx context.Context, maxEntries int64, pageToken string) ([]*computev1.Disk, string, error)
	// Regional Disk Methods
	GetReplicaZoneURI(zone string) string
	// Instance Methods
	GetInstanceOrError(ctx context.Context, instanceZone, instanceName string) (*computev1.Instance, error)
	// Zone Methods
	ListZones(ctx context.Context, region string) ([]string, error)
	ListSnapshots(ctx context.Context, filter string, maxEntries int64, pageToken string) ([]*computev1.Snapshot, string, error)
	GetSnapshot(ctx context.Context, snapshotName string) (*computev1.Snapshot, error)
	CreateSnapshot(ctx context.Context, volKey *meta.Key, snapshotName string) (*computev1.Snapshot, error)
	DeleteSnapshot(ctx context.Context, snapshotName string) error
}

// GetDefaultProject returns the project that was used to instantiate this GCE client.
func (cloud *CloudProvider) GetDefaultProject() string {
	return cloud.project
}

// GetDefaultZone returns the zone that was used to instantiate this GCE client.
func (cloud *CloudProvider) GetDefaultZone() string {
	return cloud.zone
}

// ListDisks lists disks based on maxEntries and pageToken only in the project
// and zone that the driver is running in.
func (cloud *CloudProvider) ListDisks(ctx context.Context, maxEntries int64, pageToken string) ([]*computev1.Disk, string, error) {
	lCall := cloud.service.Disks.List(cloud.project, cloud.zone)
	if maxEntries != 0 {
		lCall = lCall.MaxResults(maxEntries)
	}
	if len(pageToken) != 0 {
		lCall = lCall.PageToken(pageToken)
	}
	diskList, err := lCall.Do()
	if err != nil {
		return nil, "", err
	}
	return diskList.Items, diskList.NextPageToken, nil
}

// RepairUnderspecifiedVolumeKey will query the cloud provider and check each zone for the disk specified
// by the volume key and return a volume key with a correct zone
func (cloud *CloudProvider) RepairUnderspecifiedVolumeKey(ctx context.Context, volumeKey *meta.Key) (*meta.Key, error) {
	klog.V(5).Infof("Repairing potentially underspecified volume key %v", volumeKey)
	region, err := common.GetRegionFromZones([]string{cloud.zone})
	if err != nil {
		return nil, fmt.Errorf("failed to get region from zones: %v", err)
	}
	switch volumeKey.Type() {
	case meta.Zonal:
		foundZone := ""
		if volumeKey.Zone == common.UnspecifiedValue {
			// list all zones, try to get disk in each zone
			zones, err := cloud.ListZones(ctx, region)
			if err != nil {
				return nil, err
			}
			for _, zone := range zones {
				_, err := cloud.getZonalDiskOrError(ctx, zone, volumeKey.Name)
				if err != nil {
					if IsGCENotFoundError(err) {
						// Couldn't find the disk in this zone so we keep
						// looking
						continue
					}
					// There is some miscellaneous error getting disk from zone
					// so we return error immediately
					return nil, err
				}
				if len(foundZone) > 0 {
					return nil, fmt.Errorf("found disk %s in more than one zone: %s and %s", volumeKey.Name, foundZone, zone)
				}
				foundZone = zone
			}

			if len(foundZone) == 0 {
				return nil, notFoundError()
			}
			volumeKey.Zone = foundZone
			return volumeKey, nil
		}
		return volumeKey, nil
	case meta.Regional:
		if volumeKey.Region == common.UnspecifiedValue {
			volumeKey.Region = region
		}
		return volumeKey, nil
	default:
		return nil, fmt.Errorf("key was neither zonal nor regional, got: %v", volumeKey.String())
	}
}

func (cloud *CloudProvider) ListZones(ctx context.Context, region string) ([]string, error) {
	klog.V(5).Infof("Listing zones in region: %v", region)
	if len(cloud.zonesCache[region]) > 0 {
		return cloud.zonesCache[region], nil
	}
	zones := []string{}
	zoneList, err := cloud.service.Zones.List(cloud.project).Filter(fmt.Sprintf("region eq .*%s$", region)).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to list zones in region %s: %v", region, err)
	}
	for _, zone := range zoneList.Items {
		zones = append(zones, zone.Name)
	}
	cloud.zonesCache[region] = zones
	return zones, nil

}

func (cloud *CloudProvider) ListSnapshots(ctx context.Context, filter string, maxEntries int64, pageToken string) ([]*computev1.Snapshot, string, error) {
	klog.V(5).Infof("Listing snapshots with filter: %s, max entries: %v, page token: %s", filter, maxEntries, pageToken)
	snapshots := []*computev1.Snapshot{}
	snapshotList, err := cloud.service.Snapshots.List(cloud.project).Filter(filter).MaxResults(maxEntries).PageToken(pageToken).Do()
	if err != nil {
		return snapshots, "", err
	}
	for _, snapshot := range snapshotList.Items {
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, snapshotList.NextPageToken, nil

}

func (cloud *CloudProvider) GetDisk(ctx context.Context, key *meta.Key, gceAPIVersion GCEAPIVersion) (*CloudDisk, error) {
	klog.V(5).Infof("Getting disk %v", key)
	switch key.Type() {
	case meta.Zonal:
		if gceAPIVersion == GCEAPIVersionBeta {
			disk, err := cloud.getZonalBetaDiskOrError(ctx, key.Zone, key.Name)
			return CloudDiskFromBeta(disk), err
		} else {
			disk, err := cloud.getZonalDiskOrError(ctx, key.Zone, key.Name)
			return CloudDiskFromV1(disk), err
		}
	case meta.Regional:
		if gceAPIVersion == GCEAPIVersionBeta {
			disk, err := cloud.getRegionalAlphaDiskOrError(ctx, key.Region, key.Name)
			return CloudDiskFromBeta(disk), err
		} else {
			disk, err := cloud.getRegionalDiskOrError(ctx, key.Region, key.Name)
			return CloudDiskFromV1(disk), err
		}
	default:
		return nil, fmt.Errorf("key was neither zonal nor regional, got: %v", key.String())
	}
}

func (cloud *CloudProvider) getZonalDiskOrError(ctx context.Context, volumeZone, volumeName string) (*computev1.Disk, error) {
	svc := cloud.service
	project := cloud.project

	disk, err := svc.Disks.Get(project, volumeZone, volumeName).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return disk, nil
}

func (cloud *CloudProvider) getRegionalDiskOrError(ctx context.Context, volumeRegion, volumeName string) (*computev1.Disk, error) {
	project := cloud.project
	disk, err := cloud.service.RegionDisks.Get(project, volumeRegion, volumeName).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return disk, nil
}

func (cloud *CloudProvider) getZonalBetaDiskOrError(ctx context.Context, volumeZone, volumeName string) (*computebeta.Disk, error) {
	project := cloud.project
	disk, err := cloud.betaService.Disks.Get(project, volumeZone, volumeName).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return disk, nil
}

func (cloud *CloudProvider) getRegionalAlphaDiskOrError(ctx context.Context, volumeRegion, volumeName string) (*computebeta.Disk, error) {
	project := cloud.project
	disk, err := cloud.betaService.RegionDisks.Get(project, volumeRegion, volumeName).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return disk, nil
}

func (cloud *CloudProvider) GetReplicaZoneURI(zone string) string {
	return cloud.service.BasePath + fmt.Sprintf(
		replicaZoneURITemplateSingleZone,
		cloud.project,
		zone)
}

func (cloud *CloudProvider) getRegionURI(region string) string {
	return cloud.service.BasePath + fmt.Sprintf(
		regionURITemplate,
		cloud.project,
		region)
}

func (cloud *CloudProvider) ValidateExistingDisk(ctx context.Context, resp *CloudDisk, params common.DiskParameters, reqBytes, limBytes int64, multiWriter bool) error {
	klog.V(5).Infof("Validating existing disk %v with diskType: %s, reqested bytes: %v, limit bytes: %v", resp, params.DiskType, reqBytes, limBytes)
	if resp == nil {
		return fmt.Errorf("disk does not exist")
	}
	requestValid := common.GbToBytes(resp.GetSizeGb()) >= reqBytes || reqBytes == 0
	responseValid := common.GbToBytes(resp.GetSizeGb()) <= limBytes || limBytes == 0
	if !requestValid || !responseValid {
		return fmt.Errorf(
			"disk already exists with incompatible capacity. Need %v (Required) < %v (Existing) < %v (Limit)",
			reqBytes, common.GbToBytes(resp.GetSizeGb()), limBytes)
	}

	// We are assuming here that a multiWriter disk could be used as non-multiWriter
	if multiWriter && !resp.GetMultiWriter() {
		return fmt.Errorf("disk already exists with incompatible capability. Need MultiWriter. Got non-MultiWriter")
	}

	return ValidateDiskParameters(resp, params)
}

// ValidateDiskParameters takes a CloudDisk and returns true if the parameters
// specified validly describe the disk provided, and false otherwise.
func ValidateDiskParameters(disk *CloudDisk, params common.DiskParameters) error {
	if disk.GetPDType() != params.DiskType {
		return fmt.Errorf("actual pd type %s did not match the expected param %s", disk.GetPDType(), params.DiskType)
	}

	locationType := disk.LocationType()
	if (params.ReplicationType == "none" && locationType != meta.Zonal) || (params.ReplicationType == "regional-pd" && locationType != meta.Regional) {
		return fmt.Errorf("actual disk replication type %v did not match expected param %s", locationType, params.ReplicationType)
	}

	if !kmsKeyEqual(
		disk.GetKMSKeyName(), /* fetchedKMSKey */
		params.DiskEncryptionKMSKey /* storageClassKMSKey */) {
		return fmt.Errorf("actual disk KMS key name %s did not match expected param %s", disk.GetKMSKeyName(), params.DiskEncryptionKMSKey)
	}

	return nil
}

// ContextWithCallTimeout returns a context with a default timeout, used for generated client calls.
func ContextWithCallTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), defaultCallTimeout)
}

func (cloud *CloudProvider) InsertDisk(ctx context.Context, volKey *meta.Key, params common.DiskParameters, capBytes int64, capacityRange *csi.CapacityRange, replicaZones []string, snapshotID string, multiWriter bool) error {
	klog.V(5).Infof("Inserting disk %v", volKey)

	description, err := encodeDiskTags(params.Tags)
	if err != nil {
		return err
	}

	switch volKey.Type() {
	case meta.Zonal:
		if description == "" {
			description = "Disk created by GCE-PD CSI Driver"
		}
		return cloud.insertZonalDisk(ctx, volKey, params, capBytes, capacityRange, snapshotID, description, multiWriter)
	case meta.Regional:
		if description == "" {
			description = "Regional disk created by GCE-PD CSI Driver"
		}
		return cloud.insertRegionalDisk(ctx, volKey, params, capBytes, capacityRange, replicaZones, snapshotID, description, multiWriter)
	default:
		return fmt.Errorf("could not insert disk, key was neither zonal nor regional, instead got: %v", volKey.String())
	}
}

func convertV1CustomerEncryptionKeyToBeta(v1Key *computev1.CustomerEncryptionKey) *computebeta.CustomerEncryptionKey {
	return &computebeta.CustomerEncryptionKey{
		KmsKeyName:      v1Key.KmsKeyName,
		RawKey:          v1Key.RawKey,
		Sha256:          v1Key.Sha256,
		ForceSendFields: v1Key.ForceSendFields,
		NullFields:      v1Key.NullFields,
	}
}

func convertV1DiskToBetaDisk(v1Disk *computev1.Disk) *computebeta.Disk {
	var dek *computebeta.CustomerEncryptionKey = nil

	if v1Disk.DiskEncryptionKey != nil {
		dek = convertV1CustomerEncryptionKeyToBeta(v1Disk.DiskEncryptionKey)
	}

	// Note: this is an incomplete list. It only includes the fields we use for disk creation.
	return &computebeta.Disk{
		Name:              v1Disk.Name,
		SizeGb:            v1Disk.SizeGb,
		Description:       v1Disk.Description,
		Type:              v1Disk.Type,
		SourceSnapshot:    v1Disk.SourceSnapshot,
		ReplicaZones:      v1Disk.ReplicaZones,
		DiskEncryptionKey: dek,
	}
}

func (cloud *CloudProvider) insertRegionalDisk(
	ctx context.Context,
	volKey *meta.Key,
	params common.DiskParameters,
	capBytes int64,
	capacityRange *csi.CapacityRange,
	replicaZones []string,
	snapshotID string,
	description string,
	multiWriter bool) error {
	var (
		err           error
		opName        string
		gceAPIVersion = GCEAPIVersionV1
	)

	if multiWriter {
		gceAPIVersion = GCEAPIVersionBeta
	}

	diskToCreate := &computev1.Disk{
		Name:        volKey.Name,
		SizeGb:      common.BytesToGbRoundUp(capBytes),
		Description: description,
		Type:        cloud.GetDiskTypeURI(volKey, params.DiskType),
		Labels:      params.Labels,
	}
	if snapshotID != "" {
		diskToCreate.SourceSnapshot = snapshotID
	}
	if len(replicaZones) != 0 {
		diskToCreate.ReplicaZones = replicaZones
	}
	if params.DiskEncryptionKMSKey != "" {
		diskToCreate.DiskEncryptionKey = &computev1.CustomerEncryptionKey{
			KmsKeyName: params.DiskEncryptionKMSKey,
		}
	}

	if gceAPIVersion == GCEAPIVersionBeta {
		var insertOp *computebeta.Operation
		betaDiskToCreate := convertV1DiskToBetaDisk(diskToCreate)
		betaDiskToCreate.MultiWriter = multiWriter
		insertOp, err = cloud.betaService.RegionDisks.Insert(cloud.project, volKey.Region, betaDiskToCreate).Context(ctx).Do()
		if insertOp != nil {
			opName = insertOp.Name
		}
	} else {
		var insertOp *computev1.Operation
		insertOp, err = cloud.service.RegionDisks.Insert(cloud.project, volKey.Region, diskToCreate).Context(ctx).Do()
		if insertOp != nil {
			opName = insertOp.Name
		}
	}
	if err != nil {
		if IsGCEError(err, "alreadyExists") {
			disk, err := cloud.GetDisk(ctx, volKey, gceAPIVersion)
			if err != nil {
				return err
			}
			err = cloud.ValidateExistingDisk(ctx, disk, params,
				int64(capacityRange.GetRequiredBytes()),
				int64(capacityRange.GetLimitBytes()),
				multiWriter)
			if err != nil {
				return err
			}
			klog.Warningf("GCE PD %s already exists, reusing", volKey.Name)
			return nil
		}
		return status.Error(codes.Internal, fmt.Sprintf("unknown Insert disk error: %v", err))
	}

	err = cloud.waitForRegionalOp(ctx, opName, volKey.Region)
	if err != nil {
		if IsGCEError(err, "alreadyExists") {
			disk, err := cloud.GetDisk(ctx, volKey, gceAPIVersion)
			if err != nil {
				return err
			}
			err = cloud.ValidateExistingDisk(ctx, disk, params,
				int64(capacityRange.GetRequiredBytes()),
				int64(capacityRange.GetLimitBytes()),
				multiWriter)
			if err != nil {
				return err
			}
			klog.Warningf("GCE PD %s already exists after wait, reusing", volKey.Name)
			return nil
		}
		return fmt.Errorf("unknown Insert disk operation error: %v", err)
	}
	return nil
}

func (cloud *CloudProvider) insertZonalDisk(
	ctx context.Context,
	volKey *meta.Key,
	params common.DiskParameters,
	capBytes int64,
	capacityRange *csi.CapacityRange,
	snapshotID string,
	description string,
	multiWriter bool) error {
	var (
		err           error
		opName        string
		gceAPIVersion = GCEAPIVersionV1
	)

	if multiWriter {
		gceAPIVersion = GCEAPIVersionBeta
	}

	diskToCreate := &computev1.Disk{
		Name:        volKey.Name,
		SizeGb:      common.BytesToGbRoundUp(capBytes),
		Description: description,
		Type:        cloud.GetDiskTypeURI(volKey, params.DiskType),
		Labels:      params.Labels,
	}

	if snapshotID != "" {
		diskToCreate.SourceSnapshot = snapshotID
	}

	if params.DiskEncryptionKMSKey != "" {
		diskToCreate.DiskEncryptionKey = &computev1.CustomerEncryptionKey{
			KmsKeyName: params.DiskEncryptionKMSKey,
		}
	}

	callCtx, cancel := ContextWithCallTimeout()
	defer cancel()

	if gceAPIVersion == GCEAPIVersionBeta {
		var insertOp *computebeta.Operation
		betaDiskToCreate := convertV1DiskToBetaDisk(diskToCreate)
		betaDiskToCreate.MultiWriter = multiWriter
		insertOp, err = cloud.betaService.Disks.Insert(cloud.project, volKey.Zone, betaDiskToCreate).Context(callCtx).Do()
		if insertOp != nil {
			opName = insertOp.Name
		}
	} else {
		var insertOp *computev1.Operation
		insertOp, err = cloud.service.Disks.Insert(cloud.project, volKey.Zone, diskToCreate).Context(callCtx).Do()
		if insertOp != nil {
			opName = insertOp.Name
		}
	}

	if err != nil {
		if IsGCEError(err, "alreadyExists") {
			disk, err := cloud.GetDisk(ctx, volKey, gceAPIVersion)
			if err != nil {
				return err
			}
			err = cloud.ValidateExistingDisk(ctx, disk, params,
				int64(capacityRange.GetRequiredBytes()),
				int64(capacityRange.GetLimitBytes()),
				multiWriter)
			if err != nil {
				return err
			}
			klog.Warningf("GCE PD %s already exists, reusing", volKey.Name)
			return nil
		}
		return fmt.Errorf("unknown Insert disk error: %v", err)
	}
	klog.Infof("Insert disk %s op id %s", volKey.Name, opName)
	err = cloud.waitForZonalOp(ctx, opName, volKey.Zone)

	if err != nil {
		if IsGCEError(err, "alreadyExists") {
			disk, err := cloud.GetDisk(ctx, volKey, gceAPIVersion)
			if err != nil {
				return err
			}
			err = cloud.ValidateExistingDisk(ctx, disk, params,
				int64(capacityRange.GetRequiredBytes()),
				int64(capacityRange.GetLimitBytes()),
				multiWriter)
			if err != nil {
				return err
			}
			klog.Warningf("GCE PD %s already exists after wait, reusing", volKey.Name)
			return nil
		}
		return fmt.Errorf("unknown Insert disk operation error: %v", err)
	}
	return nil
}

func (cloud *CloudProvider) DeleteDisk(ctx context.Context, volKey *meta.Key) error {
	klog.V(5).Infof("Deleting disk: %v", volKey)
	switch volKey.Type() {
	case meta.Zonal:
		return cloud.deleteZonalDisk(ctx, volKey.Zone, volKey.Name)
	case meta.Regional:
		return cloud.deleteRegionalDisk(ctx, volKey.Region, volKey.Name)
	default:
		return fmt.Errorf("could not delete disk, key was neither zonal nor regional, instead got: %v", volKey.String())
	}
}

func (cloud *CloudProvider) deleteZonalDisk(ctx context.Context, zone, name string) error {
	op, err := cloud.service.Disks.Delete(cloud.project, zone, name).Context(ctx).Do()
	if err != nil {
		if IsGCEError(err, "notFound") {
			// Already deleted
			return nil
		}
		return err
	}
	klog.Infof("delete disk %s op id %s", name, op.Name)
	err = cloud.waitForZonalOp(ctx, op.Name, zone)
	if err != nil {
		return err
	}
	return nil
}

func (cloud *CloudProvider) deleteRegionalDisk(ctx context.Context, region, name string) error {
	op, err := cloud.service.RegionDisks.Delete(cloud.project, region, name).Context(ctx).Do()
	if err != nil {
		if IsGCEError(err, "notFound") {
			// Already deleted
			return nil
		}
		return err
	}
	err = cloud.waitForRegionalOp(ctx, op.Name, region)
	if err != nil {
		return err
	}
	return nil
}

func (cloud *CloudProvider) AttachDisk(ctx context.Context, volKey *meta.Key, readWrite, diskType, instanceZone, instanceName string) error {
	klog.V(5).Infof("Attaching disk %v to %s", volKey, instanceName)
	source := cloud.GetDiskSourceURI(volKey)

	deviceName, err := common.GetDeviceName(volKey)
	if err != nil {
		return fmt.Errorf("failed to get device name: %v", err)
	}
	attachedDiskV1 := &computev1.AttachedDisk{
		DeviceName: deviceName,
		Kind:       diskKind,
		Mode:       readWrite,
		Source:     source,
		Type:       diskType,
	}

	callCtx, cancel := ContextWithCallTimeout()
	defer cancel()

	op, err := cloud.service.Instances.AttachDisk(cloud.project, instanceZone, instanceName, attachedDiskV1).Context(callCtx).Do()
	if err != nil {
		return fmt.Errorf("failed cloud service attach disk %v call: %v", volKey, err)
	}
	klog.V(5).Infof("Attaching disk %s operation id is %s", volKey, op.Name)
	time.Sleep(2 * time.Second)

	op2, err2 := cloud.service.Instances.AttachDisk(cloud.project, instanceZone, instanceName, attachedDiskV1).Context(callCtx).Do()
	if err2 != nil {
		klog.Errorf("failed cloud service attach disk %v call op2 %v: %v", volKey, op2, err2)
		//return fmt.Errorf("failed cloud service attach disk %v call op2 %v: %v", volKey, op2, err2)
	} else {
		klog.V(2).Infof("Attaching disk %s op2 id is %s", volKey, op2.Name)
	}

	klog.V(5).Infof("Attaching disk %s operation id is %s", volKey, op.Name)

	/*err = cloud.waitForZonalOp2(ctx, op.Name, instanceZone)
	if err != nil {
		klog.Errorf("failed when waiting for zonal op %s on attaching volume %v: %v", op.Name, volKey, err)
		//return fmt.Errorf("failed when waiting for zonal op %s on attaching volume %v: %v", op.Name, volKey, err)
	}*/

	err = cloud.waitForZonalOp2(ctx, op.Name, op2.Name, instanceName, instanceZone, volKey)
	if err != nil {
		return fmt.Errorf("failed when waiting for zonal op %s op2 %s on attaching volume %v: %v", op.Name, op2.Name, volKey, err)
	}
	return nil
}

func (cloud *CloudProvider) DetachDisk(ctx context.Context, deviceName, instanceZone, instanceName string) error {

	callCtx, cancel := ContextWithCallTimeout()
	defer cancel()

	klog.V(5).Infof("Detaching disk %s from %v", deviceName, instanceName)
	op, err := cloud.service.Instances.DetachDisk(cloud.project, instanceZone, instanceName, deviceName).Context(callCtx).Do()
	if err != nil {
		return err
	}
	klog.V(5).Infof("Detaching disk %s operation id is %s", deviceName, op.Name)
	err = cloud.waitForZonalOp(ctx, op.Name, instanceZone)
	if err != nil {
		return fmt.Errorf("failed when waiting for zonal op %s on detaching volume %s: %v", op.Name, deviceName, err)
	}
	return nil
}

func (cloud *CloudProvider) GetDiskSourceURI(volKey *meta.Key) string {
	switch volKey.Type() {
	case meta.Zonal:
		return cloud.getZonalDiskSourceURI(volKey.Name, volKey.Zone)
	case meta.Regional:
		return cloud.getRegionalDiskSourceURI(volKey.Name, volKey.Region)
	default:
		return ""
	}
}

func (cloud *CloudProvider) getZonalDiskSourceURI(diskName, zone string) string {
	return cloud.service.BasePath + fmt.Sprintf(
		diskSourceURITemplateSingleZone,
		cloud.project,
		zone,
		diskName)
}

func (cloud *CloudProvider) getRegionalDiskSourceURI(diskName, region string) string {
	return cloud.service.BasePath + fmt.Sprintf(
		diskSourceURITemplateRegional,
		cloud.project,
		region,
		diskName)
}

func (cloud *CloudProvider) GetDiskTypeURI(volKey *meta.Key, diskType string) string {
	switch volKey.Type() {
	case meta.Zonal:
		return cloud.getZonalDiskTypeURI(volKey.Zone, diskType)
	case meta.Regional:
		return cloud.getRegionalDiskTypeURI(volKey.Region, diskType)
	default:
		return fmt.Sprintf("could get disk type URI, key was neither zonal nor regional, instead got: %v", volKey.String())
	}
}

func (cloud *CloudProvider) getZonalDiskTypeURI(zone, diskType string) string {
	return cloud.service.BasePath + fmt.Sprintf(diskTypeURITemplateSingleZone, cloud.project, zone, diskType)
}

func (cloud *CloudProvider) getRegionalDiskTypeURI(region, diskType string) string {
	return cloud.service.BasePath + fmt.Sprintf(diskTypeURITemplateRegional, cloud.project, region, diskType)
}

// SleepWithContext will wait for the timer duration to expire, or the context
// is canceled. Which ever happens first. If the context is canceled the Context's
// error will be returned.
//
// Expects Context to always return a non-nil error if the Done channel is closed.
/*func SleepWithContext(ctx Context, dur time.Duration) error {
	t := time.NewTimer(dur)
	defer t.Stop()

	select {
	case <-t.C:
		break
	case <-ctx.Done():
		return ctx.Err()
	}

	return nil
}*/

func (cloud *CloudProvider) waitForZonalOp(ctx context.Context, opName string, zone string) error {
	// The v1 API can query for v1, alpha, or beta operations.
	svc := cloud.service
	project := cloud.project
	timeout := OpTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
		klog.Infof("Getting timeout from passed context  opname %s %v", opName, timeout)
	}
	//callCtx, cancel := ContextWithCallTimeout()
	//defer cancel()

	return wait.Poll(OpPollInterval, timeout, func() (bool, error) {
		pollOp, err := svc.ZoneOperations.Get(project, zone, opName).Context(ctx).Do()
		if err != nil {
			klog.Errorf("WaitForOp(op: %s, zone: %#v) failed to poll the operation: %v", opName, zone, err)
			return false, err
		}
		done, err := opIsDone(pollOp)
		klog.V(4).Infof("checking op %s is done %t", opName, done)
		return done, err
	})
}

func (cloud *CloudProvider) waitForZonalOp2(ctx context.Context, opName, opName2 string, instance, zone string, volKey *meta.Key) error {
	// The v1 API can query for v1, alpha, or beta operations.
	svc := cloud.service
	project := cloud.project
	timeout := OpTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
		klog.Infof("Getting timeout from passed context  opname %s %v", opName, timeout)
	}
	//callCtx, cancel := ContextWithCallTimeout()
	//defer cancel()

	return wait.Poll(OpPollInterval, timeout, func() (bool, error) {
		pollOp, err := svc.ZoneOperations.Get(project, zone, opName).Context(ctx).Do()
		if err != nil {
			klog.Errorf("WaitForOp(op: %s, zone: %#v) failed to poll the operation: %v", opName, zone, err)
			return false, err
		}
		done, err := opIsDone(pollOp)
		klog.V(4).Infof("checking op %s is done %t %v", opName, done, err)

		pollOp, err = svc.ZoneOperations.Get(project, zone, opName2).Context(ctx).Do()
		if err != nil {
			klog.Errorf("WaitForOp(op: %s, zone: %#v) failed to poll the operation2: %v", opName2, zone, err)
			return false, err
		}
		done2, err2 := opIsDone(pollOp)
		klog.V(4).Infof("checking op2 %s is done %t %v", opName2, done2, err2)

		if done || done2 {
			_, err = cloud.WaitForAttach2(ctx, volKey, zone, instance)
			if err != nil {
				klog.Errorf("volkey  %v unknown WaitForAttach error: %v", volKey, err)
				//return nil, status.Error(codes.Internal, fmt.Sprintf("unknown WaitForAttach error: %v", err))
			}
		}
		return done && done2, err
	})
}

func (cloud *CloudProvider) waitForRegionalOp(ctx context.Context, opName string, region string) error {
	// The v1 API can query for v1, alpha, or beta operations.
	return wait.Poll(3*time.Second, 5*time.Minute, func() (bool, error) {
		pollOp, err := cloud.service.RegionOperations.Get(cloud.project, region, opName).Context(ctx).Do()
		if err != nil {
			klog.Errorf("WaitForOp(op: %s, region: %#v) failed to poll the operation", opName, region)
			return false, err
		}
		done, err := opIsDone(pollOp)
		return done, err
	})
}

func (cloud *CloudProvider) waitForGlobalOp(ctx context.Context, opName string) error {
	svc := cloud.service
	project := cloud.project
	return wait.Poll(3*time.Second, 5*time.Minute, func() (bool, error) {
		pollOp, err := svc.GlobalOperations.Get(project, opName).Context(ctx).Do()
		if err != nil {
			klog.Errorf("waitForGlobalOp(op: %s) failed to poll the operation", opName)
			return false, err
		}
		done, err := opIsDone(pollOp)
		return done, err
	})
}

func (cloud *CloudProvider) WaitForAttach(ctx context.Context, volKey *meta.Key, instanceZone, instanceName string) error {
	klog.V(5).Infof("Waiting for attach of disk %v to instance %v to complete...", volKey.Name, instanceName)
	start := time.Now()
	return wait.Poll(5*time.Second, 2*time.Minute, func() (bool, error) {
		klog.V(5).Infof("Polling for attach of disk %v to instance %v to complete for %v", volKey.Name, instanceName, time.Since(start))
		disk, err := cloud.GetDisk(ctx, volKey, GCEAPIVersionV1)
		if err != nil {
			return false, fmt.Errorf("GetDisk failed to get disk: %v", err)
		}

		if disk == nil {
			return false, fmt.Errorf("Disk %v could not be found", volKey.Name)
		}

		for _, user := range disk.GetUsers() {
			if strings.Contains(user, instanceName) && strings.Contains(user, instanceZone) {
				return true, nil
			}
		}
		return false, nil
	})
}

func (cloud *CloudProvider) WaitForAttach2(ctx context.Context, volKey *meta.Key, instanceZone, instanceName string) (bool, error) {
	klog.V(5).Infof("Waiting for attach of disk %v to instance %v to complete...", volKey.Name, instanceName)
	start := time.Now()
	//return wait.Poll(1*time.Second, 2*time.Second, func() (bool, error) {
	klog.V(5).Infof("Polling for attach of disk %v to instance %v to complete for %v", volKey.Name, instanceName, time.Since(start))
	disk, err := cloud.GetDisk(ctx, volKey, GCEAPIVersionV1)
	if err != nil {
		return false, fmt.Errorf("GetDisk failed to get disk: %v", err)
	}

	if disk == nil {
		return false, fmt.Errorf("Disk %v could not be found", volKey.Name)
	}

	for _, user := range disk.GetUsers() {
		if strings.Contains(user, instanceName) && strings.Contains(user, instanceZone) {
			klog.Infof("disk %v is attachd ", volKey)
			return true, nil
		}
	}
	return false, nil
	//})
}

func opIsDone(op *computev1.Operation) (bool, error) {
	if op == nil {
		return true, fmt.Errorf("operation is nil")
	}
	if op.Status != operationStatusDone {
		return false, nil
	}
	if op.Error != nil && len(op.Error.Errors) > 0 && op.Error.Errors[0] != nil {
		return true, fmt.Errorf("operation %v failed (%v): %v", op.Name, op.Error.Errors[0].Code, op.Error.Errors[0].Message)
	}
	return true, nil
}

func (cloud *CloudProvider) GetInstanceOrError(ctx context.Context, instanceZone, instanceName string) (*computev1.Instance, error) {
	klog.V(5).Infof("Getting instance %v from zone %v", instanceName, instanceZone)
	svc := cloud.service
	project := cloud.project
	instance, err := svc.Instances.Get(project, instanceZone, instanceName).Do()
	if err != nil {
		return nil, err
	}
	return instance, nil
}

func (cloud *CloudProvider) GetSnapshot(ctx context.Context, snapshotName string) (*computev1.Snapshot, error) {
	klog.V(5).Infof("Getting snapshot %v", snapshotName)
	svc := cloud.service
	project := cloud.project
	snapshot, err := svc.Snapshots.Get(project, snapshotName).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (cloud *CloudProvider) DeleteSnapshot(ctx context.Context, snapshotName string) error {
	klog.V(5).Infof("Deleting snapshot %v", snapshotName)
	op, err := cloud.service.Snapshots.Delete(cloud.project, snapshotName).Context(ctx).Do()
	if err != nil {
		if IsGCEError(err, "notFound") {
			// Already deleted
			return nil
		}
		return err
	}
	err = cloud.waitForGlobalOp(ctx, op.Name)
	if err != nil {
		return err
	}
	return nil
}

func (cloud *CloudProvider) CreateSnapshot(ctx context.Context, volKey *meta.Key, snapshotName string) (*computev1.Snapshot, error) {
	klog.V(5).Infof("Creating snapshot %s for volume %v", snapshotName, volKey)
	switch volKey.Type() {
	case meta.Zonal:
		return cloud.createZonalDiskSnapshot(ctx, volKey, snapshotName)
	case meta.Regional:
		return cloud.createRegionalDiskSnapshot(ctx, volKey, snapshotName)
	default:
		return nil, fmt.Errorf("could not create snapshot, key was neither zonal nor regional, instead got: %v", volKey.String())
	}
}

// ResizeDisk takes in the requested disk size in bytes and returns the resized
// size in Gi
// TODO(#461) The whole driver could benefit from standardized usage of the
// k8s.io/apimachinery/quantity package for better size handling
func (cloud *CloudProvider) ResizeDisk(ctx context.Context, volKey *meta.Key, requestBytes int64) (int64, error) {
	klog.V(5).Infof("Resizing disk %v to size %v", volKey, requestBytes)
	cloudDisk, err := cloud.GetDisk(ctx, volKey, GCEAPIVersionV1)
	if err != nil {
		return -1, fmt.Errorf("failed to get disk: %v", err)
	}

	sizeGb := cloudDisk.GetSizeGb()
	requestGb := common.BytesToGbRoundUp(requestBytes)

	// If disk is already of size equal or greater than requested size, we
	// simply return the found size
	if sizeGb >= requestGb {
		return sizeGb, nil
	}

	switch volKey.Type() {
	case meta.Zonal:
		return cloud.resizeZonalDisk(ctx, volKey, requestGb)
	case meta.Regional:
		return cloud.resizeRegionalDisk(ctx, volKey, requestGb)
	default:
		return -1, fmt.Errorf("could not resize disk, key was neither zonal nor regional, instead got: %v", volKey.String())
	}
}

func (cloud *CloudProvider) resizeZonalDisk(ctx context.Context, volKey *meta.Key, requestGb int64) (int64, error) {
	resizeReq := &computev1.DisksResizeRequest{
		SizeGb: requestGb,
	}
	op, err := cloud.service.Disks.Resize(cloud.project, volKey.Zone, volKey.Name, resizeReq).Context(ctx).Do()
	if err != nil {
		return -1, fmt.Errorf("failed to resize zonal volume %v: %v", volKey.String(), err)
	}

	err = cloud.waitForZonalOp(ctx, op.Name, volKey.Zone)
	if err != nil {
		return -1, fmt.Errorf("failed waiting for op for zonal resize for %s: %v", volKey.String(), err)
	}

	return requestGb, nil
}

func (cloud *CloudProvider) resizeRegionalDisk(ctx context.Context, volKey *meta.Key, requestGb int64) (int64, error) {
	resizeReq := &computev1.RegionDisksResizeRequest{
		SizeGb: requestGb,
	}

	op, err := cloud.service.RegionDisks.Resize(cloud.project, volKey.Region, volKey.Name, resizeReq).Context(ctx).Do()
	if err != nil {
		return -1, fmt.Errorf("failed to resize regional volume %v: %v", volKey.String(), err)
	}

	err = cloud.waitForRegionalOp(ctx, op.Name, volKey.Region)
	if err != nil {
		return -1, fmt.Errorf("failed waiting for op for regional resize for %s: %v", volKey.String(), err)
	}

	return requestGb, nil
}

func (cloud *CloudProvider) createZonalDiskSnapshot(ctx context.Context, volKey *meta.Key, snapshotName string) (*computev1.Snapshot, error) {
	snapshotToCreate := &computev1.Snapshot{
		Name: snapshotName,
	}

	_, err := cloud.service.Disks.CreateSnapshot(cloud.project, volKey.Zone, volKey.Name, snapshotToCreate).Context(ctx).Do()

	if err != nil {
		return nil, err
	}

	return cloud.waitForSnapshotCreation(ctx, snapshotName)
}

func (cloud *CloudProvider) createRegionalDiskSnapshot(ctx context.Context, volKey *meta.Key, snapshotName string) (*computev1.Snapshot, error) {
	snapshotToCreate := &computev1.Snapshot{
		Name: snapshotName,
	}

	_, err := cloud.service.RegionDisks.CreateSnapshot(cloud.project, volKey.Region, volKey.Name, snapshotToCreate).Context(ctx).Do()
	if err != nil {
		return nil, err
	}

	return cloud.waitForSnapshotCreation(ctx, snapshotName)

}

func (cloud *CloudProvider) waitForSnapshotCreation(ctx context.Context, snapshotName string) (*computev1.Snapshot, error) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	timer := time.NewTimer(waitForSnapshotCreationTimeOut)
	defer timer.Stop()

	for {
		select {
		case <-ticker.C:
			klog.V(6).Infof("Checking GCE Snapshot %s.", snapshotName)
			snapshot, err := cloud.GetSnapshot(ctx, snapshotName)
			if err != nil {
				klog.Warningf("Error in getting snapshot %s, %v", snapshotName, err)
			} else if snapshot != nil {
				if snapshot.Status != "CREATING" {
					klog.V(6).Infof("Snapshot %s status is %s", snapshotName, snapshot.Status)
					return snapshot, nil
				} else {
					klog.V(6).Infof("Snapshot %s is still creating ...", snapshotName)
				}
			}
		case <-timer.C:
			return nil, fmt.Errorf("Timeout waiting for snapshot %s to be created.", snapshotName)
		}
	}
}

// kmsKeyEqual returns true if fetchedKMSKey and storageClassKMSKey refer to the same key.
// fetchedKMSKey - key returned by the server
//        example: projects/{0}/locations/{1}/keyRings/{2}/cryptoKeys/{3}/cryptoKeyVersions/{4}
// storageClassKMSKey - key as provided by the client
//        example: projects/{0}/locations/{1}/keyRings/{2}/cryptoKeys/{3}
// cryptoKeyVersions should be disregarded if the rest of the key is identical.
func kmsKeyEqual(fetchedKMSKey, storageClassKMSKey string) bool {
	return removeCryptoKeyVersion(fetchedKMSKey) == removeCryptoKeyVersion(storageClassKMSKey)
}

func removeCryptoKeyVersion(kmsKey string) string {
	i := strings.LastIndex(kmsKey, cryptoKeyVerDelimiter)
	if i > 0 {
		return kmsKey[:i]
	}
	return kmsKey
}

// encodeDiskTags encodes requested volume tags into JSON string, as GCE does
// not support tags on GCE PDs and we use Description field as fallback.
func encodeDiskTags(tags map[string]string) (string, error) {
	if len(tags) == 0 {
		// No tags -> empty JSON
		return "", nil
	}

	enc, err := json.Marshal(tags)
	if err != nil {
		return "", fmt.Errorf("failed to encodeDiskTags %v: %v", tags, err)
	}
	return string(enc), nil
}
