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

package driver

import (
	"context"
	"dfs-csi/pkg/util"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
	"k8s.io/utils/mount"

	"dfs-csi/pkg/juicefs"
	"dfs-csi/pkg/juicefs/k8sclient"
	podmount "dfs-csi/pkg/juicefs/mount"
	k8sMount "k8s.io/utils/mount"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

const (
	fsTypeNone          = "none"
	defaultCheckTimeout = 2 * time.Second
	procMountInfoPath   = "/proc/self/mountinfo"
)

var (
	nodeCaps = []csi.NodeServiceCapability_RPC_Type{}
)

type nodeService struct {
	juicefs   juicefs.Interface
	nodeID    string
	k8sClient k8sclient.K8sClient
}

func newNodeService(nodeID string) (*nodeService, error) {
	jfsProvider, err := juicefs.NewJfsProvider(nil)
	if err != nil {
		panic(err)
	}

	stdoutStderr, err := jfsProvider.Version()
	if err != nil {
		panic(err)
	}
	klog.V(4).Infof("Node: %s", stdoutStderr)

	k8sClient, err := k8sclient.NewClient()
	if err != nil {
		klog.V(5).Infof("Can't get k8s client: %v", err)
		return nil, err
	}

	return &nodeService{
		juicefs:   jfsProvider,
		nodeID:    nodeID,
		k8sClient: k8sClient,
	}, nil
}

// NodeStageVolume is called by the CO prior to the volume being consumed by any workloads on the node by `NodePublishVolume`
func (d *nodeService) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// NodeUnstageVolume is a reverse operation of `NodeStageVolume`
func (d *nodeService) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// NodePublishVolume is called by the CO when a workload that wants to use the specified volume is placed (scheduled) on a node
func (d *nodeService) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	// WARNING: debug only, secrets included
	// klog.V(5).Infof("NodePublishVolume: called with args %+v", req)

	volumeID := req.GetVolumeId()
	klog.V(5).Infof("NodePublishVolume: volume_id is %s", volumeID)

	target := req.GetTargetPath()
	if len(target) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path not provided")
	}

	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability not provided")
	}
	klog.V(5).Infof("NodePublishVolume: volume_capability is %s", volCap)

	if !isValidVolumeCapabilities([]*csi.VolumeCapability{volCap}) {
		return nil, status.Error(codes.InvalidArgument, "Volume capability not supported")
	}

	klog.V(5).Infof("NodePublishVolume: creating dir %s", target)
	if err := CreateTarget(ctx, target); err != nil {
		return nil, status.Errorf(codes.Internal, "Could not create dir %q: %v", target, err)
	}

	options := make(map[string]string)
	if req.GetReadonly() || req.VolumeCapability.AccessMode.GetMode() == csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY {
		options["ro"] = ""
	}
	if m := volCap.GetMount(); m != nil {
		for _, f := range m.MountFlags {
			options[f] = ""
		}
	}

	volCtx := req.GetVolumeContext()
	klog.V(5).Infof("NodePublishVolume: volume context: %v", volCtx)

	secrets := req.Secrets
	mountOptions := []string{}
	if opts, ok := volCtx["mountOptions"]; ok {
		mountOptions = strings.Split(opts, ",")
	}
	for k, v := range options {
		if v != "" {
			k = fmt.Sprintf("%s=%s", k, v)
		}
		mountOptions = append(mountOptions, k)
	}

	klog.V(5).Infof("NodePublishVolume: mounting juicefs with secret %+v, options %v", reflect.ValueOf(secrets).MapKeys(), mountOptions)
	jfs, err := d.juicefs.JfsMount(volumeID, target, secrets, volCtx, mountOptions, true)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not mount juicefs: %v", err)
	}

	bindSource, err := jfs.CreateVol(volumeID, volCtx["subPath"], 0, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not create volume: %s, %v", volumeID, err)
	}

	if err := d.checkBindAndMount(ctx, jfs.GetBasePath(), bindSource, target); err != nil {
		klog.Errorf("bind mount failed, error:%s", err)
		return nil, status.Errorf(codes.Internal, "Could not bind %q at %q: %v", bindSource, target, err)
	}
	klog.V(5).Infof("NodePublishVolume: mounted %s at %s with options %v", volumeID, target, mountOptions)
	return &csi.NodePublishVolumeResponse{}, nil
}

func (d *nodeService) checkBindAndMount(ctx context.Context, mountPath, bindSource, target string) error {
	klog.V(5).Infof("mountPath:%s, bindSource:%s, target:%s", mountPath, bindSource, target)
	mountInfos, err := mount.ParseMountInfo(procMountInfoPath)
	if err != nil {
		return err
	}
	var mountMinor, targetMinor *int
	for _, mi := range mountInfos {
		if mi.MountPoint == mountPath {
			minor := mi.Minor
			mountMinor = &minor
		}
		if mi.MountPoint == target {
			targetMinor = &mi.Minor
		}
	}
	if mountMinor == nil {
		return fmt.Errorf("BindTarget: mountPath %s not mounted", mountPath)
	}
	if targetMinor != nil {
		if *targetMinor == *mountMinor {
			// target already binded mountpath
			klog.V(5).Infof("BindTarget: target %s already bind mount to %s", target, mountPath)
			return nil
		}
		// target is bind by other path, umount it
		klog.Infof("BindTarget: target %s bind mount to other path, umount it", target)
		util.UmountPath(ctx, target)
	}
	// bind target to mountpath
	klog.V(5).Infof("NodePublishVolume: binding %s at %s", bindSource, target)
	if err := d.juicefs.Mount(bindSource, target, fsTypeNone, []string{"bind"}); err != nil {
		os.Remove(target)
		return fmt.Errorf("could not bind %q at %q: %v", bindSource, target, err)
	}
	return nil
}

// NodeUnpublishVolume is a reverse operation of NodePublishVolume. This RPC is typically called by the CO when the workload using the volume is being moved to a different node, or all the workload using the volume on a node has finished.
func (d *nodeService) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	klog.V(4).Infof("NodeUnpublishVolume: called with args %+v", req)

	target := req.GetTargetPath()
	if len(target) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path not provided")
	}

	volumeId := req.GetVolumeId()
	klog.V(5).Infof("NodeUnpublishVolume: volume_id is %s", volumeId)

	klog.V(5).Infof("JfsUnmount: start umount %s", target)
	// 设置超时ctx，避免某些情况下Umount卡死导致
	subCtx, _ := context.WithTimeout(ctx, 150*time.Second)
	for {
		command := exec.CommandContext(subCtx, "umount", target)
		out, err := command.CombinedOutput()
		if err == nil {
			continue
		}
		klog.V(6).Infoln(string(out))
		if !strings.Contains(string(out), "not mounted") &&
			!strings.Contains(string(out), "mountpoint not found") &&
			!strings.Contains(string(out), "no mount point specified") {
			klog.V(5).Infof("Unmount %s failed: %q, try to lazy unmount", target, err)
			output, err := exec.CommandContext(subCtx, "umount", "-l", target).CombinedOutput()
			if err != nil {
				klog.V(5).Infof("Could not lazy unmount %q: %v, output: %s", target, err, string(output))
				return nil, err
			}
		}
		break
	}
	klog.V(5).Infof("Unmount %s success", target)

	mounter := &mount.SafeFormatAndMount{
		Interface: mount.New(""),
	}
	// cleanup target path
	if err := k8sMount.CleanupMountPoint(target, mounter.Interface, false); err != nil {
		klog.V(5).Infof("Clean mount point error: %v", err)
		return nil, err
	}

	volumeId, err := d.juicefs.GetUniqueId(volumeId)
	if err != nil {
		klog.V(5).Infof("GetUniqueId from volumeId:%s error: %v", volumeId, err)
		return nil, err
	}
	mnt := podmount.NewPodMount(nil, d.k8sClient)
	if err := mnt.JUmount(volumeId, target); err != nil {
		return &csi.NodeUnpublishVolumeResponse{}, err
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeGetCapabilities response node capabilities to CO
func (d *nodeService) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	klog.V(4).Infof("NodeGetCapabilities: called with args %+v", req)
	var caps []*csi.NodeServiceCapability
	for _, cap := range nodeCaps {
		c := &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: cap,
				},
			},
		}
		caps = append(caps, c)
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: caps}, nil
}

// NodeGetInfo is called by CO for the node at which it wants to place the workload. The result of this call will be used by CO in ControllerPublishVolume.
func (d *nodeService) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	klog.V(4).Infof("NodeGetInfo: called with args %+v", req)

	return &csi.NodeGetInfoResponse{
		NodeId: d.nodeID,
	}, nil
}

// NodeExpandVolume unimplemented
func (d *nodeService) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *nodeService) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeGetVolumeStats is not implemented yet")
}

func CreateTarget(ctx context.Context, target string) error {
	var corruptedMnt bool
	var exists bool

	for {
		err := util.DoWithTimeout(ctx, defaultCheckTimeout, func() (err error) {
			exists, err = k8sMount.PathExists(target)
			return
		})
		if err == nil {
			if !exists {
				if err := os.MkdirAll(target, os.FileMode(0755)); err != nil {
					return fmt.Errorf("could not create dir %q: %v", target, err)
				}
			}
			return nil
		} else if corruptedMnt = k8sMount.IsCorruptedMnt(err); corruptedMnt {
			// if target is a corrupted mount, umount it
			util.UmountPath(ctx, target)
			continue
		} else {
			return err
		}
	}
}
