/*
Copyright 2021 Juicedata Inc

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

package juicefs

import (
	"context"
	"fmt"
	"io/ioutil"
	urlpkg "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"dfs-csi/pkg/juicefs/config"
	"dfs-csi/pkg/juicefs/k8sclient"
	podmount "dfs-csi/pkg/juicefs/mount"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog"
	k8sexec "k8s.io/utils/exec"
	"k8s.io/utils/mount"
)

// Interface of juicefs provider
type Interface interface {
	mount.Interface
	GetUniqueId(volumeId string) (string, error)
	JfsMount(volumeID string, target string, secrets, volCtx map[string]string, options []string, usePod bool) (Jfs, error)
	JfsUnmount(mountPath string) error
	AuthFs(secrets map[string]string, extraEnvs map[string]string) ([]byte, error)
	MountFs(volumeID string, target string, options []string, jfsSetting *config.JfsSetting) (string, error)
	Version() ([]byte, error)
}

type juicefs struct {
	mount.SafeFormatAndMount
	k8sclient.K8sClient
}

var _ Interface = &juicefs{}

type jfs struct {
	Console    *Console
	Provider   *juicefs
	Name       string
	MountPath  string
	Options    []string
	JfsSetting *config.JfsSetting
}

// Jfs is the interface of a mounted file system
type Jfs interface {
	GetBasePath() string
	CreateVol(volumeID, subPath string, size int64, qos string) (string, error)
	CreateVolFromSnap(volumeID, subPath, snapID string, size int64, qos string) (string, error)
	CreateVolFromVol(volumeID, subPath, srcVolID string, size int64, qos string) (string, error)
	DeleteVol(volumeID string) error
	CreateSnap(volumeID, snapName string) (string, error)
	DeleteSnap(snapID string) error
	createSnap(snapPath, srcPath string) (string, error)
}

var _ Jfs = &jfs{}

func (fs *jfs) GetBasePath() string {
	return fs.MountPath
}

func (fs *jfs) Exec(cmd string, args ...string) ([]byte, error) {
	ccmd := fs.Provider.Exec.Command(cmd, args...)
	envs := syscall.Environ()
	for key, val := range fs.JfsSetting.Envs {
		envs = append(envs, fmt.Sprintf("%s=%s", key, val))
	}
	ccmd.SetEnv(envs)
	return ccmd.CombinedOutput()
}

func (fs *jfs) DeleteSnap(snapID string) error {

	snapPath := filepath.Join(fs.MountPath, snapID)
	args := []string{"snapshot", "-d", snapPath}
	exists, err := mount.PathExists(snapPath)
	if err != nil {
		return status.Errorf(codes.Internal, "Could not check path %q exists: %v", snapPath, err)
	}
	if exists {
		out, err := fs.Exec(config.CliPath, args...)
		if err != nil {
			klog.Errorf("%v output: %s", args, out)
			return err
		}
	}
	return nil
}

func (fs *jfs) CreateSnap(volumeID, snapName string) (string, error) {

	volPath := filepath.Join(fs.MountPath, volumeID)
	snapPath := fmt.Sprintf("%s_%s", volPath, snapName)
	snapID := filepath.Base(snapPath)

	_, err := fs.createSnap(snapPath, volPath)
	if err != nil {

		return "", err
	}
	return snapID, nil
}

func (fs *jfs) CreateVolFromSnap(volumeID, subPath, snapID string, size int64, qos string) (string, error) {
	volPath := filepath.Join(fs.MountPath, subPath)
	srcsnapPath := filepath.Join(fs.MountPath, snapID)
	_, err := fs.createSnap(volPath, srcsnapPath)
	if err != nil {
		return "", status.Errorf(codes.Internal, "Could not create vol from snap  %s: %q", subPath, err)
	}
	if size > 0 {
		path := "/" + subPath
		_ = fs.Console.DeleteQuota(fs.JfsSetting.Name, path)
		err = fs.Console.CreateQuota(fs.JfsSetting.Name, path, 0, size)
		if err != nil {
			return "", status.Errorf(codes.Internal, "Could not create quota  %s: %q", subPath, err)
		}
	}
	if len(qos) > 0 {
		if err := fs.Console.SetQos(fs.JfsSetting.Name, qos); err != nil {
			return "", status.Errorf(codes.Internal, "set qos error:%s", err)
		}
	}
	return volPath, nil

}

func (fs *jfs) CreateVolFromVol(volumeID, subPath, srcVolID string, size int64, qos string) (string, error) {
	volPath := filepath.Join(fs.MountPath, subPath)
	srcvolPath := filepath.Join(fs.MountPath, srcVolID)
	_, err := fs.createSnap(volPath, srcvolPath)
	if err != nil {
		return "", status.Errorf(codes.Internal, "Could not create vol from vol  %s: %q", subPath, err)
	}
	if size > 0 {
		path := "/" + subPath
		_ = fs.Console.DeleteQuota(fs.JfsSetting.Name, path)
		err = fs.Console.CreateQuota(fs.JfsSetting.Name, path, 0, size)
		if err != nil {
			return "", status.Errorf(codes.Internal, "Could not create quota  %s: %q", subPath, err)
		}
	}
	if len(qos) > 0 {
		if err := fs.Console.SetQos(fs.JfsSetting.Name, qos); err != nil {
			return "", status.Errorf(codes.Internal, "set qos error:%s", err)
		}
	}
	return volPath, nil
}

func (fs *jfs) createSnap(snapPath, srcPath string) (string, error) {
	exists, err := mount.PathExists(snapPath)
	if err != nil {
		return "", status.Errorf(codes.Internal, "Could not check path %q exists: %v", snapPath, err)
	}
	if !exists {
		args := []string{"snapshot", srcPath, snapPath}
		out, err := fs.Exec(config.CliPath, args...)
		if err != nil {
			klog.Errorf("createSnap %v output: %s", args, out)
			return "", err
		}
	}
	if fi, err := os.Stat(snapPath); err != nil {
		return "", status.Errorf(codes.Internal, "Could not stat directory %s: %q", snapPath, err)
	} else if fi.Mode().Perm() != 0777 { // The perm of `volPath` may not be 0777 when the umask applied
		err = os.Chmod(snapPath, os.FileMode(0777))
		if err != nil {
			return "", status.Errorf(codes.Internal, "Could not chmod directory %s: %q", snapPath, err)
		}
	}
	return snapPath, nil
}

// CreateVol creates the directory needed
func (fs *jfs) CreateVol(volumeID, subPath string, size int64, qos string) (string, error) {
	volPath := filepath.Join(fs.MountPath, subPath)

	klog.V(5).Infof("CreateVol: checking %q exists in %v", volPath, fs)
	exists, err := mount.PathExists(volPath)
	if err != nil {
		return "", status.Errorf(codes.Internal, "Could not check volume path %q exists: %v", volPath, err)
	}
	if !exists {
		klog.V(5).Infof("CreateVol: volume not existed")
		err := os.MkdirAll(volPath, os.FileMode(0777))
		if err != nil {
			return "", status.Errorf(codes.Internal, "Could not make directory for meta %q: %v", volPath, err)
		}
	}
	if fi, err := os.Stat(volPath); err != nil {
		return "", status.Errorf(codes.Internal, "Could not stat directory %s: %q", volPath, err)
	} else if fi.Mode().Perm() != 0777 { // The perm of `volPath` may not be 0777 when the umask applied
		err = os.Chmod(volPath, os.FileMode(0777))
		if err != nil {
			return "", status.Errorf(codes.Internal, "Could not chmod directory %s: %q", volPath, err)
		}
	}
	if size > 0 {
		path := "/" + subPath
		_ = fs.Console.DeleteQuota(fs.JfsSetting.Name, path)
		err = fs.Console.CreateQuota(fs.JfsSetting.Name, path, 0, size)
		if err != nil {
			return "", status.Errorf(codes.Internal, "Could not create quota  %s: %q", subPath, err)
		}
	}

	if len(qos) > 0 {
		if err := fs.Console.SetQos(fs.JfsSetting.Name, qos); err != nil {
			return "", status.Errorf(codes.Internal, "set qos error:%s", err)
		}
	}

	return volPath, nil
}

func (fs *jfs) DeleteVol(volumeID string) error {
	volPath := filepath.Join(fs.MountPath, volumeID)
	if existed, err := mount.PathExists(volPath); err != nil {
		return status.Errorf(codes.Internal, "Could not check volume path %q exists: %v", volPath, err)
	} else if existed {
		stdoutStderr, err := fs.Provider.RmrDir(volPath, fs.JfsSetting.IsCe)
		klog.V(5).Infof("DeleteVol: rmr output is '%s'", stdoutStderr)
		if err != nil {
			return status.Errorf(codes.Internal, "Could not delete volume path %q: %v", volPath, err)
		}
	}
	return fs.Console.DeleteQuota(fs.JfsSetting.Name, "/"+volumeID)
}

// NewJfsProvider creates a provider for JuiceFS file system
func NewJfsProvider(mounter *mount.SafeFormatAndMount) (Interface, error) {
	if mounter == nil {
		mounter = &mount.SafeFormatAndMount{
			Interface: mount.New(""),
			Exec:      k8sexec.New(),
		}
	}
	k8sClient, err := k8sclient.NewClient()
	if err != nil {
		klog.V(5).Infof("Can't get k8s client: %v", err)
		return nil, err
	}

	return &juicefs{*mounter, k8sClient}, nil
}

func (j *juicefs) IsNotMountPoint(dir string) (bool, error) {
	return mount.IsNotMountPoint(j, dir)
}

func GetConsole(jfsSecret *config.JfsSetting, secrets map[string]string) (*Console, error) {

	consoleBaseurl, ok := jfsSecret.Envs["BASE_URL"]
	if !ok {
		klog.Errorf(" base url not found")
		return nil, status.Errorf(codes.Internal, "base url not found")
	}
	consolUrl, err := urlpkg.Parse(consoleBaseurl)
	if err != nil {
		klog.Errorf("Parse base url %q error: %v", consoleBaseurl, err)
		return nil, err
	}
	consoleToken, ok := secrets["consoleToken"]
	if !ok {
		klog.Errorf(" consoleToken not found")
		return nil, status.Errorf(codes.Internal, "consoleToken not found")
	}
	return &Console{Addr: consolUrl.Host, Token: consoleToken}, nil
}

// GetUniqueId: get UniqueId from volumeId (volumeHandle of PV)
// When STORAGE_CLASS_SHARE_MOUNT env is set:
//
//	in dynamic provision, UniqueId set as SC name
//	in static provision, UniqueId set as volumeId
//
// When STORAGE_CLASS_SHARE_MOUNT env not set:
//
//	UniqueId set as volumeId
func (j *juicefs) GetUniqueId(volumeId string) (string, error) {
	if os.Getenv("STORAGE_CLASS_SHARE_MOUNT") == "true" {
		pv, err := j.K8sClient.GetPersistentVolume(volumeId)
		// In static provision, volumeId may not be PV name, it is expected that PV cannot be found by volumeId
		if err != nil && !k8serrors.IsNotFound(err) {
			return "", err
		}
		// In dynamic provision, PV.spec.StorageClassName is which SC(StorageClass) it belongs to.
		if err == nil && pv.Spec.StorageClassName != "" {
			return pv.Spec.StorageClassName, nil
		}
	}
	return volumeId, nil
}

// JfsMount auths and mounts JuiceFS
func (j *juicefs) JfsMount(volumeID string, target string, secrets, volCtx map[string]string, options []string, usePod bool) (Jfs, error) {
	volumeID, err := j.GetUniqueId(volumeID)
	if err != nil {
		klog.V(5).Infof("GetUniqueId from volumeId:%s error: %v", volumeID, err)
		return nil, err
	}

	jfsSecret, err := config.ParseSetting(secrets, volCtx, usePod)
	if err != nil {
		klog.V(5).Infof("Parse config error: %v", err)
		return nil, err
	}
	console, err := GetConsole(jfsSecret, secrets)
	if err != nil {
		return nil, err
	}
	source, isCe := secrets["metaurl"]
	var mountPath string
	if !isCe {
		j.Upgrade()
		if secrets["token"] == "" {
			klog.V(5).Infof("token is empty, skip authfs.")
		} else {
			stdoutStderr, err := j.AuthFs(secrets, jfsSecret.Envs)
			klog.V(5).Infof("mosfsMount: authentication output is '%s'\n", stdoutStderr)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "Could not auth juicefs: %v", err)
			}
		}
		jfsSecret.Source = secrets["name"]
	} else {
		noUpdate := false
		if secrets["storage"] == "" || secrets["bucket"] == "" {
			klog.V(5).Infof("mosfsMount: storage or bucket is empty, format --no-update.")
			noUpdate = true
		}
		stdoutStderr, err := j.ceFormat(secrets, noUpdate)
		klog.V(5).Infof("mosfsMount: format output is '%s'\n", stdoutStderr)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "Could not format juicefs: %v", err)
		}
		// Default use redis:// scheme
		if !strings.Contains(source, "://") {
			source = "redis://" + source
		}
		jfsSecret.Source = source
	}
	mountPath, err = j.MountFs(volumeID, target, options, jfsSecret)
	if err != nil {
		return nil, err
	}

	return &jfs{
		Provider:   j,
		Name:       secrets["name"],
		MountPath:  mountPath,
		Options:    options,
		JfsSetting: jfsSecret,
		Console:    console,
	}, nil
}

func (j *juicefs) JfsUnmount(mountPath string) (err error) {
	klog.V(5).Infof("JfsUnmount: umount %s", mountPath)
	if err = j.Unmount(mountPath); err != nil {
		klog.V(5).Infof("JfsUnmount: error umount %s, %v", mountPath, err)
		return err
	}

	// cleanup target path
	if err := j.JfsCleanupMountPoint(mountPath); err != nil {
		klog.V(5).Infof("Clean mount point error: %v", err)
		return err
	}
	return
}

func (j *juicefs) JfsCleanupMountPoint(mountPath string) error {
	klog.V(5).Infof("JfsCleanupMountPoint: clean up mount point: %q", mountPath)
	return mount.CleanupMountPoint(mountPath, j.SafeFormatAndMount.Interface, false)
}

func (j *juicefs) RmrDir(directory string, isCeMount bool) ([]byte, error) {
	klog.V(5).Infof("RmrDir: removing directory recursively: %q", directory)
	if isCeMount {
		return j.Exec.Command(config.CeCliPath, "rmr", directory).CombinedOutput()
	}
	return j.Exec.Command(config.CliPath, "rmr", directory).CombinedOutput()
}

// AuthFs authenticates JuiceFS, enterprise edition only
func (j *juicefs) AuthFs(secrets map[string]string, extraEnvs map[string]string) ([]byte, error) {
	if secrets == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Nil secrets")
	}

	if secrets["name"] == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Empty name")
	}

	args := []string{"auth", secrets["name"]}
	argsStripped := []string{"auth", secrets["name"]}
	keys := []string{
		"accesskey",
		"accesskey2",
		"bucket",
		"bucket2",
	}
	keysStripped := []string{
		"token",
		"secretkey",
		"secretkey2",
		"passphrase"}
	isOptional := map[string]bool{
		"accesskey2": true,
		"secretkey2": true,
		"bucket":     true,
		"bucket2":    true,
		"passphrase": true,
	}
	for _, k := range keys {
		if !isOptional[k] || secrets[k] != "" {
			args = append(args, fmt.Sprintf("--%s=%s", k, secrets[k]))
			argsStripped = append(argsStripped, fmt.Sprintf("--%s=%s", k, secrets[k]))
		}
	}
	for _, k := range keysStripped {
		if !isOptional[k] || secrets[k] != "" {
			args = append(args, fmt.Sprintf("--%s=%s", k, secrets[k]))
			argsStripped = append(argsStripped, fmt.Sprintf("--%s=[secret]", k))
		}
	}
	if v, ok := os.LookupEnv("JFS_NO_UPDATE_CONFIG"); ok && v == "enabled" {
		args = append(args, "--no-update")
		argsStripped = append(argsStripped, "--no-update")

		if secrets["bucket"] == "" {
			return nil, status.Errorf(codes.InvalidArgument,
				"bucket argument is required when --no-update option is provided")
		}
		if secrets["initconfig"] != "" {
			conf := secrets["name"] + ".conf"
			confPath := filepath.Join("/root/.juicefs", conf)
			if _, err := os.Stat(confPath); os.IsNotExist(err) {
				err = ioutil.WriteFile(confPath, []byte(secrets["initconfig"]), 0644)
				if err != nil {
					return nil, status.Errorf(codes.Internal,
						"Create config file %q failed: %v", confPath, err)
				}
				klog.V(5).Infof("Create config file: %q success", confPath)
			}
		}
	}
	klog.V(5).Infof("AuthFs: cmd %q, args %#v", config.CliPath, argsStripped)
	authCmd := j.Exec.Command(config.CliPath, args...)
	envs := syscall.Environ()
	for key, val := range extraEnvs {
		envs = append(envs, fmt.Sprintf("%s=%s", key, val))
	}
	authCmd.SetEnv(envs)
	return authCmd.CombinedOutput()
}

// MountFs mounts JuiceFS with idempotency
func (j *juicefs) MountFs(volumeID, target string, options []string, jfsSetting *config.JfsSetting) (string, error) {
	var mountPath string
	var mnt podmount.Interface
	if jfsSetting.UsePod {
		mountPath = filepath.Join(config.PodMountBase, volumeID)
		mnt = podmount.NewPodMount(jfsSetting, j.K8sClient)
	} else {
		mountPath = filepath.Join(config.MountBase, volumeID)
		mnt = podmount.NewProcessMount(jfsSetting)
	}

	// 此处不能直接通过挂载点判断，因为上一个业务Pod卸载卷时，下一个pod尝试启动，会出现竞态问题。导致后面Pod执行csi流程时，MountPod处于正在删除，但还没删除完毕的状态。
	// 尝试启动mountPod进行挂载
	klog.V(5).Infof("Mount: mounting %q at %q with options %v", jfsSetting.Source, mountPath, options)
	err := mnt.JMount(jfsSetting.Storage, volumeID, mountPath, target, options)
	if err != nil {
		return "", fmt.Errorf("could not mount %q at %q: %v", jfsSetting.Source, mountPath, err)
	}
	return mountPath, nil
}

// Upgrade upgrades binary file in `cliPath` to newest version
func (j *juicefs) Upgrade() {
	if v, ok := os.LookupEnv("JFS_AUTO_UPGRADE"); !ok || v != "enabled" {
		return
	}

	timeout := 10
	if t, ok := os.LookupEnv("JFS_AUTO_UPGRADE_TIMEOUT"); ok {
		if v, err := strconv.Atoi(t); err == nil {
			timeout = v
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	err := exec.CommandContext(ctx, config.CliPath, "version", "-u").Run()
	if ctx.Err() == context.DeadlineExceeded {
		klog.V(5).Infof("Upgrade: did not finish in %v", timeout)
		return
	}

	if err != nil {
		klog.V(5).Infof("Upgrade: err %v", err)
		return
	}

	klog.V(5).Infof("Upgrade: successfully upgraded to newest version")
}

func (j *juicefs) Version() ([]byte, error) {
	return j.Exec.Command(config.CliPath, "version").CombinedOutput()
}

func (j *juicefs) ceFormat(secrets map[string]string, noUpdate bool) ([]byte, error) {
	if secrets == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Nil secrets")
	}

	if secrets["name"] == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Empty name")
	}

	if secrets["metaurl"] == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Empty metaurl")
	}

	args := []string{"format"}
	if noUpdate {
		args = append(args, "--no-update")
	}
	if secrets["storage"] == "ceph" || secrets["storage"] == "gs" {
		os.Setenv("JFS_NO_CHECK_OBJECT_STORAGE", "1")
	}
	argsStripped := []string{"format"}
	keys := []string{
		"storage",
		"bucket",
		"access-key",
		"block-size",
		"compress",
	}
	keysStripped := []string{"secret-key"}
	isOptional := map[string]bool{
		"block-size": true,
		"compress":   true,
	}
	for _, k := range keys {
		if !isOptional[k] || secrets[k] != "" {
			args = append(args, fmt.Sprintf("--%s=%s", k, secrets[k]))
			argsStripped = append(argsStripped, fmt.Sprintf("--%s=%s", k, secrets[k]))
		}
	}
	for _, k := range keysStripped {
		if !isOptional[k] || secrets[k] != "" {
			args = append(args, fmt.Sprintf("--%s=%s", k, secrets[k]))
			argsStripped = append(argsStripped, fmt.Sprintf("--%s=[secret]", k))
		}
	}
	args = append(args, secrets["metaurl"], secrets["name"])
	argsStripped = append(argsStripped, "[metaurl]", secrets["name"])
	klog.V(5).Infof("ceFormat: cmd %q, args %#v", config.CeCliPath, argsStripped)
	return j.Exec.Command(config.CeCliPath, args...).CombinedOutput()
}
