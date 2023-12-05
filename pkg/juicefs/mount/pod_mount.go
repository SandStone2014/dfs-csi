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

package mount

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog"
	k8sexec "k8s.io/utils/exec"
	k8sMount "k8s.io/utils/mount"

	jfsConfig "dfs-csi/pkg/juicefs/config"
	"dfs-csi/pkg/juicefs/k8sclient"
	"dfs-csi/pkg/util"
)

const defaultCheckTimeout = 2 * time.Second

type PodMount struct {
	k8sMount.SafeFormatAndMount
	jfsSetting *jfsConfig.JfsSetting
	K8sClient  k8sclient.K8sClient
}

func NewPodMount(setting *jfsConfig.JfsSetting, client k8sclient.K8sClient) Interface {
	mounter := &k8sMount.SafeFormatAndMount{
		Interface: k8sMount.New(""),
		Exec:      k8sexec.New(),
	}
	return &PodMount{*mounter, setting, client}
}

func (p *PodMount) JMount(storage, volumeId, mountPath string, target string, options []string) error {
	cmd := ""
	if p.jfsSetting.IsCe {
		klog.V(5).Infof("ceMount: mount %v at %v", p.jfsSetting.Source, mountPath)
		mountArgs := []string{jfsConfig.CeMountPath, p.jfsSetting.Source, mountPath}
		options = append(options, "metrics=0.0.0.0:9567")
		mountArgs = append(mountArgs, "-o", strings.Join(options, ","))
		cmd = strings.Join(mountArgs, " ")
	} else {
		klog.V(5).Infof("Mount: mount %v at %v", p.jfsSetting.Source, mountPath)
		mountArgs := []string{jfsConfig.JfsMountPath, p.jfsSetting.Source, mountPath}
		options = append(options, "foreground")
		if len(options) > 0 {
			mountArgs = append(mountArgs, "-o", strings.Join(options, ","))
		}
		cmd = strings.Join(mountArgs, " ")
	}

	return p.waitUntilMount(volumeId, target, mountPath, cmd)
}

func (p *PodMount) JUmount(volumeId, target string) error {
	// check mount pod is need to delete
	klog.V(5).Infof("DeleteRefOfMountPod: Check mount pod is need to delete or not.")

	pod, err := p.K8sClient.GetPod(GeneratePodNameByVolumeId(volumeId), jfsConfig.Namespace)
	if err != nil && !k8serrors.IsNotFound(err) {
		klog.V(5).Infof("DeleteRefOfMountPod: Get pod of volumeId %s err: %v", volumeId, err)
		return err
	}

	// if mount pod not exists.
	if pod == nil {
		klog.V(5).Infof("DeleteRefOfMountPod: Mount pod of volumeId %v not exists.", volumeId)
		return nil
	}

	klog.V(5).Infof("DeleteRefOfMountPod: Delete target ref [%s] in pod [%s].", target, pod.Name)

	key := util.GetReferenceKey(target)
	klog.V(5).Infof("DeleteRefOfMountPod: Target %v hash of target %v", target, key)

loop:
	err = func() error {
		jfsConfig.JLock.Lock()
		defer jfsConfig.JLock.Unlock()

		po, err := p.K8sClient.GetPod(pod.Name, pod.Namespace)
		if err != nil {
			return err
		}
		annotation := po.Annotations
		if _, ok := annotation[key]; !ok {
			klog.V(5).Infof("DeleteRefOfMountPod: Target ref [%s] in pod [%s] already not exists.", target, pod.Name)
			return nil
		}
		klog.V(5).Infof("DeleteRefOfMountPod: Remove ref of volumeId %v, target %v", volumeId, target)
		return util.DelPodAnnotation(context.TODO(), p.K8sClient, po, []string{key})
	}()
	if err != nil && k8serrors.IsConflict(err) {
		// if can't update pod because of conflict, retry
		klog.V(5).Infof("DeleteRefOfMountPod: Update pod conflict, retry.")
		goto loop
	} else if err != nil {
		return err
	}

	deleteMountPod := func(podName, namespace string) error {
		jfsConfig.JLock.Lock()
		defer jfsConfig.JLock.Unlock()

		po, err := p.K8sClient.GetPod(podName, namespace)
		if err != nil {
			return err
		}

		if hasRef(po) {
			klog.V(5).Infof("DeleteRefOfMountPod: pod:[%s] still has mosfs- refs.", po.Name)
			return nil
		}

		var shouldDelay bool
		shouldDelay, err = util.ShouldDelay(context.TODO(), po, &p.K8sClient)
		if err != nil {
			klog.Errorf("check mountPod:[%s] should delay delete, error:%s", po.Name, err)
			return err
		}
		if !shouldDelay {
			klog.V(5).Infof("DeleteRefOfMountPod: Pod of volumeId %v has not refs, delete it.", volumeId)
			if err := p.K8sClient.DeletePod(po); err != nil {
				klog.V(5).Infof("DeleteRefOfMountPod: Delete pod of volumeId %s error: %v", volumeId, err)
				return err
			}
		}
		return nil
	}

	newPod, err := p.K8sClient.GetPod(pod.Name, pod.Namespace)
	if err != nil {
		return err
	}
	if hasRef(newPod) {
		klog.V(5).Infof("DeleteRefOfMountPod: pod:[%s] still has mosfs- refs.", newPod.Name)
		return nil
	}
	klog.V(5).Infof("DeleteRefOfMountPod: pod:[%s] has no mosfs- refs.", newPod.Name)
	// if pod annotations has no "mosfs-" prefix, delete pod
	return deleteMountPod(pod.Name, pod.Namespace)
}

func (p *PodMount) waitUntilMount(volumeId, target, mountPath, cmd string) error {
	podName := GeneratePodNameByVolumeId(volumeId)
	klog.V(5).Infof("waitUtilMount: Mount pod cmd: %v", cmd)
	podResource := corev1.ResourceRequirements{}
	configMap := make(map[string]string)
	env := make(map[string]string)
	deletedDelay := p.jfsSetting.DeletedDelay
	if p.jfsSetting != nil {
		podResource = parsePodResources(
			p.jfsSetting.MountPodCpuLimit,
			p.jfsSetting.MountPodMemLimit,
			p.jfsSetting.MountPodCpuRequest,
			p.jfsSetting.MountPodMemRequest,
		)
		configMap = p.jfsSetting.Configs
		env = p.jfsSetting.Envs
	}

	lock := jfsConfig.GetPodLock(podName)
	lock.Lock()
	defer lock.Unlock()

	key := util.GetReferenceKey(target)
	loopTimeout := util.GetLoopTimeout()
	for i := 0; i < loopTimeout; i++ {
		po, err := p.K8sClient.GetPod(podName, jfsConfig.Namespace)
		if err != nil {
			if k8serrors.IsNotFound(err) {
				// need create
				klog.V(5).Infof("waitUtilMount: Need to create mountPod %s.", podName)
				newPod := NewMountPod(podName, cmd, mountPath, podResource, configMap, env)
				if newPod.Annotations == nil {
					newPod.Annotations = make(map[string]string)
				}
				newPod.Annotations[key] = target
				if len(deletedDelay) > 0 {
					newPod.Annotations[jfsConfig.DeleteDelayTimeKey] = deletedDelay
				}
				if _, e := p.K8sClient.CreatePod(newPod); e != nil {
					klog.Errorf("create mountPod:%s, error:%s", newPod.Name, err)
					return e
				}
				klog.V(5).Infof("create mountPod:%s success", newPod.Name)
				continue
			} else {
				klog.Errorf("get mountPod:%s, error:%s", podName, err)
				return err
			}
		}
		if po.DeletionTimestamp != nil {
			klog.V(5).Infof("wait old mountPod:%s deleted", po.Name)
			// 等待mountPod删除后重新创建
			time.Sleep(time.Millisecond * 500)
			continue
		}
		// Wait until the mount pod is ready
		klog.V(5).Infof("waitUtilMount: add mount ref in pod of volumeId %q", volumeId)
		if err := p.AddRefOfMount(target, podName); err != nil {
			return fmt.Errorf("add target:[%s] ref in mountPod:%s, error:%s", target, podName, err)
		}
		return p.waitUtilMountReady(podName, mountPath)
	}
	return fmt.Errorf("wait mountPod:[%s] ready failed", podName)
}

func (p *PodMount) waitUtilMountReady(podName, mountPath string) error {
	waitCtx, cancel := context.WithTimeout(context.TODO(), 30*time.Second)
	defer cancel()
	// Wait until the mount point is ready
	for {
		var finfo os.FileInfo
		if err := util.DoWithTimeout(waitCtx, defaultCheckTimeout, func() (err error) {
			finfo, err = os.Stat(mountPath)
			return err
		}); err != nil {
			if err == context.Canceled || err == context.DeadlineExceeded {
				break
			}
			klog.V(5).Infof("mount path %v not ready: %v", mountPath, err)
			time.Sleep(time.Millisecond * 500)
			continue
		}
		if st, ok := finfo.Sys().(*syscall.Stat_t); ok {
			if st.Ino == 1 {
				klog.V(5).Infof("Mount point %v is ready", mountPath)
				return nil
			}
			klog.V(5).Infof("Mount point %v is not ready", mountPath)
		} else {
			klog.V(5).Info("Cannot reach here")
		}
		time.Sleep(time.Millisecond * 500)
	}
	// mountpoint not ready, get mount pod log for detail
	log, err := p.getErrContainerLog(context.TODO(), podName)
	if err != nil {
		klog.Errorf("Get pod %s log error %v", podName, err)
		return fmt.Errorf("mount %v failed: mount isn't ready in 30 seconds", mountPath)
	}
	return fmt.Errorf("mount %v failed: %v", mountPath, log)
}

func (p *PodMount) getErrContainerLog(ctx context.Context, podName string) (log string, err error) {
	pod, err := p.K8sClient.GetPod(podName, jfsConfig.Namespace)
	if err != nil {
		return
	}
	for _, cn := range pod.Status.InitContainerStatuses {
		if !cn.Ready {
			log, err = p.K8sClient.GetPodLog(ctx, pod.Name, pod.Namespace, cn.Name)
			return
		}
	}
	for _, cn := range pod.Status.ContainerStatuses {
		if !cn.Ready {
			log, err = p.K8sClient.GetPodLog(ctx, pod.Name, pod.Namespace, cn.Name)
			return
		}
	}
	return
}

func (p *PodMount) AddRefOfMount(target string, podName string) error {
	// add volumeId ref in pod annotation
	key := util.GetReferenceKey(target)

loop:
	err := func() error {
		jfsConfig.JLock.Lock()
		defer jfsConfig.JLock.Unlock()

		exist, err := p.K8sClient.GetPod(podName, jfsConfig.Namespace)
		if err != nil {
			return err
		}
		annotation := exist.Annotations
		if _, ok := annotation[key]; ok {
			klog.V(5).Infof("addRefOfMount: Target ref [%s] in pod [%s] already exists.", target, podName)
			return nil
		}
		if annotation == nil {
			annotation = make(map[string]string)
		}
		annotation[key] = target
		// delete deleteDelayAt when there ars refs
		delete(annotation, jfsConfig.DeleteDelayAtKey)
		klog.V(5).Infof("addRefOfMount: Add target ref in mount pod. mount pod: [%s], target: [%s]", podName, target)
		if err := util.ReplacePodAnnotation(context.TODO(), p.K8sClient, exist, annotation); err != nil && k8serrors.IsConflict(err) {
			klog.V(5).Infof("addRefOfMount: Patch pod %s error: %v", podName, err)
			return err
		}
		return nil
	}()
	if err != nil && k8serrors.IsConflict(err) {
		// if can't update pod because of conflict, retry
		klog.V(5).Infof("AddRefOfMountPod: Update pod conflict, retry.")
		time.Sleep(10 * time.Millisecond)
		goto loop
	} else if err != nil {
		return err
	}
	return nil
}
