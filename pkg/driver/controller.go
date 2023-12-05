package driver

import (
	"context"
	"reflect"

	"dfs-csi/pkg/juicefs"
	"dfs-csi/pkg/juicefs/config"
	"dfs-csi/pkg/util"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/timestamp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
)

var (
	volumeCaps = []csi.VolumeCapability_AccessMode{
		{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
		{
			Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		},
		{
			Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		},
	}

	controllerCaps = []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
	}
)

type controllerService struct {
	juicefs   juicefs.Interface
	vols      map[string]int64
	snapLocks *util.ObjLocks
}

func newControllerService() controllerService {
	jfs, err := juicefs.NewJfsProvider(nil)
	if err != nil {
		panic(err)
	}

	stdoutStderr, err := jfs.Version()
	if err != nil {
		panic(err)
	}
	klog.V(4).Infof("Controller: %s", stdoutStderr)

	return controllerService{
		juicefs:   jfs,
		vols:      make(map[string]int64),
		snapLocks: util.NewObjLocks(),
	}
}

// CreateVolume create directory in an existing JuiceFS filesystem
func (d *controllerService) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	// DEBUG only, secrets exposed in args
	// klog.V(5).Infof("CreateVolume: called with args: %#v", req)
	klog.V(5).Infof("CreateVolume: parameters %v", req.Parameters)

	if len(req.Name) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume Name cannot be empty")
	}
	if req.VolumeCapabilities == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capabilities cannot be empty")
	}

	if acquired := d.snapLocks.TryAcquire(req.GetName()); !acquired {
		klog.Infof(util.VolumeOperationAlreadyExistsFmt, req.GetName())
		return nil, status.Errorf(codes.Aborted, util.VolumeOperationAlreadyExistsFmt, req.GetName())
	}
	defer d.snapLocks.Release(req.GetName())

	volumeId := req.Name
	subPath := req.Name
	secrets := req.Secrets
	klog.V(5).Infof("CreateVolume: Secrets contains keys %+v", reflect.ValueOf(secrets).MapKeys())

	requiredCap := req.CapacityRange.GetRequiredBytes()
	if capa, ok := d.vols[req.Name]; ok && capa < requiredCap {
		return nil, status.Errorf(codes.AlreadyExists, "Volume: %q, capacity bytes: %d", req.Name, requiredCap)
	}
	d.vols[req.Name] = requiredCap

	qos := util.ParseQosParams(req.GetParameters())

	// create volume
	// 1. mount juicefs
	jfs, err := d.juicefs.JfsMount(volumeId, "", secrets, nil, []string{}, false)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not mount juicefs: %v", err)
	}
	contentSource := req.GetVolumeContentSource()
	if contentSource == nil {
		// 2. create subPath volume
		klog.V(5).Infof("CreateVolume: Creating volume %q", volumeId)
		_, err = jfs.CreateVol(volumeId, subPath, requiredCap, qos)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "Could not create volume: %q, err: %v", volumeId, err)
		}
	} else if contentSource.GetSnapshot() != nil {
		snapId := contentSource.GetSnapshot().SnapshotId
		_, err = jfs.CreateVolFromSnap(volumeId, subPath, snapId, requiredCap, qos)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "Could not create volume: %q from snap %q, err: %v", volumeId, snapId, err)
		}
	} else if contentSource.GetVolume() != nil {
		srcVolId := contentSource.GetVolume().VolumeId
		_, err = jfs.CreateVolFromVol(volumeId, subPath, srcVolId, requiredCap, qos)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "Could not create volume: %q from volume %q, err: %v", volumeId, srcVolId, err)
		}
	}

	// 3. umount
	if err = d.juicefs.JfsUnmount(jfs.GetBasePath()); err != nil {
		return nil, status.Errorf(codes.Internal, "Could not unmount volume %q: %v", volumeId, err)
	}

	// set volume context
	volCtx := make(map[string]string)
	for k, v := range req.Parameters {
		volCtx[k] = v
	}
	volCtx["subPath"] = subPath
	volume := csi.Volume{
		VolumeId:      volumeId,
		CapacityBytes: requiredCap,
		VolumeContext: volCtx,
		ContentSource: req.GetVolumeContentSource(),
	}
	return &csi.CreateVolumeResponse{Volume: &volume}, nil
}

// DeleteVolume moves directory for the volume to trash (TODO)
func (d *controllerService) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	klog.V(4).Infof("DeleteVolume: called with args: %#v", req)
	volumeID := req.GetVolumeId()

	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}
	if acquired := d.snapLocks.TryAcquire(volumeID); !acquired {
		klog.Infof(util.VolumeOperationAlreadyExistsFmt, volumeID)
		return nil, status.Errorf(codes.Aborted, util.VolumeOperationAlreadyExistsFmt, volumeID)
	}
	defer d.snapLocks.Release(volumeID)

	secrets := req.Secrets
	klog.V(5).Infof("DeleteVolume: Secrets contains keys %+v", reflect.ValueOf(secrets).MapKeys())

	jfs, err := d.juicefs.JfsMount(volumeID, "", secrets, nil, []string{}, false)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not mount juicefs: %v", err)
	}

	klog.V(5).Infof("DeleteVolume: Deleting volume %q", volumeID)
	err = jfs.DeleteVol(volumeID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not delete volume: %q", volumeID)
	}
	delete(d.vols, volumeID)

	if err = d.juicefs.JfsUnmount(jfs.GetBasePath()); err != nil {
		return nil, status.Errorf(codes.Internal, "Could not unmount volume %q: %v", volumeID, err)
	}
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerGetCapabilities gets capabilities
func (d *controllerService) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	klog.V(4).Infof("ControllerGetCapabilities: called with args %#v", req)
	var caps []*csi.ControllerServiceCapability
	for _, cap := range controllerCaps {
		c := &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		}
		caps = append(caps, c)
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: caps}, nil
}

// GetCapacity unimplemented
func (d *controllerService) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	klog.V(4).Infof("GetCapacity: called with args %#v", req)
	return nil, status.Error(codes.Unimplemented, "")
}

// ListVolumes unimplemented
func (d *controllerService) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	klog.V(4).Infof("ListVolumes: called with args %#v", req)
	return nil, status.Error(codes.Unimplemented, "")
}

// ValidateVolumeCapabilities validates volume capabilities
func (d *controllerService) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	klog.V(4).Infof("ValidateVolumeCapabilities: called with args %#v", req)
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}

	volCaps := req.GetVolumeCapabilities()
	if len(volCaps) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities not provided")
	}

	if _, ok := d.vols[volumeID]; !ok {
		return nil, status.Errorf(codes.NotFound, "Could not get volume by ID %q", volumeID)
	}

	var confirmed *csi.ValidateVolumeCapabilitiesResponse_Confirmed
	if isValidVolumeCapabilities(volCaps) {
		confirmed = &csi.ValidateVolumeCapabilitiesResponse_Confirmed{VolumeCapabilities: volCaps}
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: confirmed,
	}, nil
}

func isValidVolumeCapabilities(volCaps []*csi.VolumeCapability) bool {
	hasSupport := func(cap *csi.VolumeCapability) bool {
		for _, c := range volumeCaps {
			if c.GetMode() == cap.AccessMode.GetMode() {
				return true
			}
		}
		return false
	}

	foundAll := true
	for _, c := range volCaps {
		if !hasSupport(c) {
			foundAll = false
		}
	}
	return foundAll
}

// CreateSnapshot
func (d *controllerService) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	klog.V(4).Infof("CreateSnapshot: called with args: %#v", req)
	volumeID := req.GetSourceVolumeId()

	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}

	secrets := req.Secrets
	klog.V(5).Infof("CreateSnapshot: Secrets contains keys %+v", reflect.ValueOf(secrets).MapKeys())

	if acquired := d.snapLocks.TryAcquire(req.GetName()); !acquired {
		klog.Infof(util.SnapshotOperationAlreadyExistsFmt, req.GetName())
		return nil, status.Errorf(codes.Aborted, util.SnapshotOperationAlreadyExistsFmt, req.GetName())
	}
	defer d.snapLocks.Release(req.GetName())

	jfs, err := d.juicefs.JfsMount(volumeID, "", secrets, nil, []string{}, false)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not mount juicefs: %v", err)
	}

	klog.V(5).Infof("CreateSnapshot: CreateSnapshot volume %q name %q", volumeID, req.GetName())
	snapdirname, err := jfs.CreateSnap(volumeID, req.GetName())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not CreateSnap volume: %q", volumeID)
	}

	if err = d.juicefs.JfsUnmount(jfs.GetBasePath()); err != nil {
		return nil, status.Errorf(codes.Internal, "Could not unmount volume %q: %v", volumeID, err)
	}

	createdAt := ptypes.TimestampNow().GetSeconds()
	return &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			//SizeBytes:      TODO get vol quotasize?,
			SnapshotId:     snapdirname,
			SourceVolumeId: volumeID,
			CreationTime: &timestamp.Timestamp{
				Seconds: createdAt,
			},
			ReadyToUse: true,
		},
	}, nil

}

// DeleteSnapshot unimplemented
func (d *controllerService) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	klog.V(4).Infof("DeleteSnapshot: called with args: %#v", req)
	snapID := req.GetSnapshotId()

	if len(snapID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "snapID ID not provided")
	}

	if acquired := d.snapLocks.TryAcquire(snapID); !acquired {
		klog.Infof(util.SnapshotOperationAlreadyExistsFmt, snapID)
		return nil, status.Errorf(codes.Aborted, util.SnapshotOperationAlreadyExistsFmt, snapID)
	}
	defer d.snapLocks.Release(snapID)

	secrets := req.Secrets
	klog.V(5).Infof("CreateSnapshot: Secrets contains keys %+v", reflect.ValueOf(secrets).MapKeys())

	jfs, err := d.juicefs.JfsMount(snapID, "", secrets, nil, []string{}, false)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not mount juicefs: %v", err)
	}

	klog.V(5).Infof("DeleteSnapshot:  snapid %q ", snapID)
	err = jfs.DeleteSnap(snapID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not delete snapshot: %q", snapID)
	}

	if err = d.juicefs.JfsUnmount(jfs.GetBasePath()); err != nil {
		return nil, status.Errorf(codes.Internal, "Could not unmount snap %q: %v", snapID, err)
	}

	return &csi.DeleteSnapshotResponse{}, nil
}

// ListSnapshots unimplemented
func (d *controllerService) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerExpandVolume
func (d *controllerService) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {

	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID cannot be empty")
	}

	capRange := req.GetCapacityRange()
	if capRange == nil {
		return nil, status.Error(codes.InvalidArgument, "capacityRange cannot be empty")
	}

	if acquired := d.snapLocks.TryAcquire(volumeID); !acquired {
		klog.Infof(util.VolumeOperationAlreadyExistsFmt, volumeID)
		return nil, status.Errorf(codes.Aborted, util.VolumeOperationAlreadyExistsFmt, volumeID)
	}
	defer d.snapLocks.Release(volumeID)
	setting, err := config.ParseSetting(req.Secrets, nil, false)
	if err != nil {
		klog.V(5).Infof("Parse config error: %v", err)
		return nil, err
	}
	console, err := juicefs.GetConsole(setting, req.Secrets)
	if err != nil {
		return nil, err
	}
	quota, err := console.GetQuota(setting.Name, "/"+volumeID)
	if err != nil {
		klog.V(5).Infof("get quota error: %v", err)
		return nil, err
	}
	if quota == nil {
		err := console.CreateQuota(setting.Name, "/"+volumeID, 0, capRange.RequiredBytes)
		if err != nil {
			klog.V(5).Infof("create quota error: %v", err)
			return nil, err
		}
	} else {
		quota.Size = capRange.RequiredBytes
		err := console.UpdateQuota(quota)
		if err != nil {
			klog.V(5).Infof("update quota error: %v", err)
			return nil, err
		}
	}

	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         capRange.RequiredBytes,
		NodeExpansionRequired: false,
	}, nil
}

// ControllerPublishVolume unimplemented
func (d *controllerService) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerUnpublishVolume unimplemented
func (d *controllerService) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}
