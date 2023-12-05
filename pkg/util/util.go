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

package util

import (
	"context"
	"crypto/sha256"
	k8s "dfs-csi/pkg/juicefs/k8sclient"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"dfs-csi/pkg/juicefs/config"

	"k8s.io/apimachinery/pkg/util/sets"
)

const defaultLoopTimeout = 60

func ParseQosParams(params map[string]string) string {
	qosParams := make([]string, 0, 2)
	uploadLimit, ok := params["upload-limit"]
	if ok {
		qosParams = append(qosParams, fmt.Sprintf("--upload-limit=%s", uploadLimit))
	}
	downloadLimit, ok := params["download-limit"]
	if ok {
		qosParams = append(qosParams, fmt.Sprintf("--download-limit=%s", downloadLimit))
	}

	if len(uploadLimit) == 0 && len(downloadLimit) == 0 {
		return fmt.Sprintf("%d:%d", 0, 0)
	}
	// qos like 1m:1m
	return fmt.Sprintf("%s:%s", uploadLimit, downloadLimit)
}

func ParseEndpoint(endpoint string) (string, string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", "", fmt.Errorf("could not parse endpoint: %v", err)
	}

	addr := path.Join(u.Host, filepath.FromSlash(u.Path))

	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "tcp":
	case "unix":
		addr = path.Join("/", addr)
		if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
			return "", "", fmt.Errorf("could not remove unix domain socket %q: %v", addr, err)
		}
	default:
		return "", "", fmt.Errorf("unsupported protocol: %s", scheme)
	}

	return scheme, addr, nil
}

func ContainsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func GetReferenceKey(target string) string {
	h := sha256.New()
	h.Write([]byte(target))
	return fmt.Sprintf("mosfs-%x", h.Sum(nil))[:63]
}

func GetLoopTimeout() int {
	res := defaultLoopTimeout
	timeout := os.Getenv("LOOP_TIMEOUT")
	if len(timeout) == 0 {
		return res
	}
	r, err := strconv.Atoi(timeout)
	if err != nil {
		klog.Errorf("parse LOOP_TIMEOUT:%s, error:%s", timeout, err)
		return res
	}
	res = r
	return res
}

// ParseMntPath return mntPath, volumeId (/jfs/volumeId, volumeId err)
func ParseMntPath(cmd string) (string, string, error) {
	args := strings.Fields(cmd)
	if len(args) < 3 || !strings.HasPrefix(args[2], config.PodMountBase) {
		return "", "", fmt.Errorf("err cmd:%s", cmd)
	}
	argSlice := strings.Split(args[2], "/")
	if len(argSlice) < 3 {
		return "", "", fmt.Errorf("err mntPath:%s", args[2])
	}
	return args[2], argSlice[2], nil
}

const (
	// VolumeOperationAlreadyExistsFmt string format to return for concerrent operation
	VolumeOperationAlreadyExistsFmt = "an operation with the given Volume %s already exists"

	// SnapshotOperationAlreadyExistsFmt string format to return for concerrent operation
	SnapshotOperationAlreadyExistsFmt = "an operation with the given Snapshot %s already exists"
)

// ObjLock implements a map with atomic operations. It stores a set of all volume IDs
// with an ongoing operation.
type ObjLocks struct {
	locks sets.String
	mux   sync.Mutex
}

// NewObjLock returns new  ObjLock
func NewObjLocks() *ObjLocks {
	return &ObjLocks{
		locks: sets.NewString(),
	}
}

// TryAcquire tries to acquire the lock for operating on id and returns true if successful.
// If another operation is already using id, returns false.
func (ols *ObjLocks) TryAcquire(id string) bool {
	ols.mux.Lock()
	defer ols.mux.Unlock()
	if ols.locks.Has(id) {
		return false
	}
	ols.locks.Insert(id)
	return true
}

func (ols *ObjLocks) Release(id string) {
	ols.mux.Lock()
	defer ols.mux.Unlock()
	ols.locks.Delete(id)
}

func DoWithTimeout(parent context.Context, timeout time.Duration, f func() error) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return DoWithContext(ctx, f)
}

func DoWithContext(ctx context.Context, f func() error) error {
	doneCh := make(chan error)
	go func() {
		doneCh <- f()
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-doneCh:
		return err
	}
}

func UmountPath(ctx context.Context, sourcePath string) {
	cmd := exec.CommandContext(ctx, "umount", sourcePath)
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		out := string(outBytes)
		if !strings.Contains(out, "not mounted") &&
			!strings.Contains(out, "mountpoint not found") &&
			!strings.Contains(out, "no mount point specified") {
			klog.V(5).Infof("Unmount %s failed: %q, try to lazy unmount", sourcePath, err)
			output, err := exec.CommandContext(ctx, "umount", "-l", sourcePath).CombinedOutput()
			if err != nil {
				klog.Errorf("could not lazy unmount %q: %v, output: %s", sourcePath, err, string(output))
			}
		}
	}
}

func ShouldDelay(ctx context.Context, pod *corev1.Pod, Client *k8s.K8sClient) (shouldDelay bool, err error) {
	delayStr, delayExist := pod.Annotations[config.DeleteDelayTimeKey]
	if !delayExist {
		// not set delete delay
		return false, nil
	}
	delayAtStr, delayAtExist := pod.Annotations[config.DeleteDelayAtKey]
	if !delayAtExist {
		// need to add delayAt annotation
		d, err := GetTimeAfterDelay(delayStr)
		if err != nil {
			klog.Errorf("delayDelete: can't parse delay time %s: %v", d, err)
			return false, nil
		}
		addAnnotation := map[string]string{config.DeleteDelayAtKey: d}
		klog.Infof("delayDelete: add annotation %v to pod %s", addAnnotation, pod.Name)
		if err := AddPodAnnotation(ctx, *Client, pod, addAnnotation); err != nil {
			klog.Errorf("delayDelete: Update pod %s error: %v", pod.Name, err)
			return true, err
		}
		return true, nil
	}
	delayAt, err := GetTime(delayAtStr)
	if err != nil {
		klog.Errorf("delayDelete: can't parse delayAt %s: %v", delayAtStr, err)
		return false, nil
	}
	return time.Now().Before(delayAt), nil
}

// GetTimeAfterDelay get time which after delay
func GetTimeAfterDelay(delayStr string) (string, error) {
	delay, err := time.ParseDuration(delayStr)
	if err != nil {
		return "", err
	}
	delayAt := time.Now().Add(delay)
	return delayAt.Format("2006-01-02 15:04:05"), nil
}

func GetTime(str string) (time.Time, error) {
	return time.Parse("2006-01-02 15:04:05", str)
}
