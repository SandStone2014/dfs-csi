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

package controller

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"os"
	"time"

	"dfs-csi/pkg/juicefs/config"
	"dfs-csi/pkg/juicefs/k8sclient"
	"dfs-csi/pkg/util"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/mount"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type PodDriver struct {
	Client   k8sclient.K8sClient
	handlers map[podStatus]podHandler
	mit      mountInfoTable
	mount.SafeFormatAndMount
}

func NewPodDriver(client k8sclient.K8sClient, mounter mount.SafeFormatAndMount) *PodDriver {
	return newPodDriver(client, mounter)
}

func newPodDriver(client k8sclient.K8sClient, mounter mount.SafeFormatAndMount) *PodDriver {
	driver := &PodDriver{
		Client:             client,
		handlers:           map[podStatus]podHandler{},
		SafeFormatAndMount: mounter,
	}
	driver.handlers[podReady] = driver.podReadyHandler
	driver.handlers[podError] = driver.podErrorHandler
	driver.handlers[podPending] = driver.podPendingHandler
	driver.handlers[podDeleted] = driver.podDeletedHandler
	return driver
}

type podHandler func(ctx context.Context, pod *corev1.Pod) error
type podStatus string

const (
	podReady   podStatus = "podReady"
	podError   podStatus = "podError"
	podDeleted podStatus = "podDeleted"
	podPending podStatus = "podPending"
)

func (p *PodDriver) SetMountInfo(mit mountInfoTable) {
	p.mit = mit
}

func (p *PodDriver) Run(ctx context.Context, current *corev1.Pod) error {
	var existTargets int
	delAnnotations := []string{}
	for k, target := range current.Annotations {
		if k == util.GetReferenceKey(target) {
			_, exists := p.mit.deletedPods[getPodUid(target)]
			if !exists {
				delAnnotations = append(delAnnotations, k)
				// target pod is deleted
				continue
			}
			existTargets++
		}
	}

	if existTargets != 0 && current.Annotations[config.DeleteDelayAtKey] != "" {
		delAnnotations = append(delAnnotations, config.DeleteDelayAtKey)
	}
	if len(delAnnotations) != 0 {
		// check mount pod reference key, if it is not the latest, return conflict
		newPod, err := p.Client.GetPod(current.Name, current.Namespace)
		if err != nil {
			return err
		}
		if len(util.GetAllRefKeys(*newPod)) != len(util.GetAllRefKeys(*current)) {
			return apierrors.NewConflict(schema.GroupResource{
				Group:    current.GroupVersionKind().Group,
				Resource: current.GroupVersionKind().Kind,
			}, current.Name, fmt.Errorf("can not patch pod"))
		}
		if err := util.DelPodAnnotation(ctx, p.Client, current, delAnnotations); err != nil {
			return err
		}
	}

	if existTargets == 0 && current.DeletionTimestamp == nil {
		// 检查是否配置了延迟删除
		var shouldDelay bool
		shouldDelay, err := util.ShouldDelay(ctx, current, &p.Client)
		if err != nil {
			klog.Errorf("check mountPod:[%s] should delay delete, error:%s", current.Name, err)
			return err
		}
		if !shouldDelay {
			// if there are no refs or after delay time, delete it
			klog.V(5).Infof("There are no refs in pod %s annotation, delete it", current.Name)
			if err := p.Client.DeletePod(current); err != nil {
				klog.Errorf("Delete pod %s error: %v", current.Name, err)
				return err
			}
			return nil
		}
	}

	return p.handlers[p.getPodStatus(current)](ctx, current)
}

func (p *PodDriver) getPodStatus(pod *corev1.Pod) podStatus {
	if pod == nil {
		return podError
	}
	if pod.DeletionTimestamp != nil {
		return podDeleted
	}
	if util.IsPodError(pod) {
		return podError
	}
	if util.IsPodReady(pod) {
		return podReady
	}
	return podPending
}

func (p *PodDriver) podErrorHandler(ctx context.Context, pod *corev1.Pod) error {
	if pod == nil {
		return nil
	}

	lock := config.GetPodLock(pod.Name)
	lock.Lock()
	defer lock.Unlock()

	// check resource err
	if util.IsPodResourceError(pod) {
		klog.V(5).Infof("waitUtilMount: Pod is failed because of resource.")
		if util.IsPodHasResource(*pod) {
			// if pod is failed because of resource, delete resource and deploy pod again.
			if err := util.RemoveFinalizer(ctx, p.Client, pod, config.Finalizer); err != nil {
				klog.Errorf("remove pod finalizer err:%v", err)
				return err
			}
			klog.V(5).Infof("Delete it and deploy again with no resource.")
			if err := p.Client.DeletePod(pod); err != nil {
				klog.V(5).Infof("delete po:%s err:%v", pod.Name, err)
				return err
			}
			// wait pod delete
			for i := 0; i < 30; i++ {
				oldPod, err := p.Client.GetPod(pod.Name, pod.Namespace)
				if err == nil {
					if pod.Finalizers != nil || len(oldPod.Finalizers) == 0 {
						if err := util.RemoveFinalizer(ctx, p.Client, oldPod, config.Finalizer); err != nil {
							klog.Errorf("remove pod finalizer err:%v", err)
						}
					}
					klog.V(5).Infof("pod still exists wait.")
					time.Sleep(time.Second * 5)
					continue
				}
				if apierrors.IsNotFound(err) {
					break
				}
				klog.V(5).Infof("get mountPod err:%v", err)
			}
			var newPod = &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        pod.Name,
					Namespace:   pod.Namespace,
					Labels:      pod.Labels,
					Annotations: pod.Annotations,
				},
				Spec: pod.Spec,
			}
			controllerutil.AddFinalizer(newPod, config.Finalizer)
			util.DeleteResourceOfPod(newPod)
			klog.V(5).Infof("Deploy again with no resource.")
			_, err := p.Client.CreatePod(newPod)
			if err != nil {
				klog.Errorf("create pod:%s err:%v", pod.Name, err)
			}
		} else {
			klog.V(5).Infof("mountPod PodResourceError, but pod no resource, do nothing.")
		}
		return nil
	}

	// check mount point is broken
	needDeleted := false
	sourcePath, _, err := util.GetMountPathOfPod(*pod)
	if err != nil {
		klog.Error(err)
		return err
	}
	exists, err := mount.PathExists(sourcePath)
	if err != nil || !exists {
		klog.V(5).Infof("%s is a corrupted mountpoint", sourcePath)
		needDeleted = true
	} else if notMnt, err := p.IsLikelyNotMountPoint(sourcePath); err != nil || notMnt {
		needDeleted = true
	}

	if needDeleted {
		klog.V(5).Infof("Get pod %s in namespace %s is err status, deleting thd pod.", pod.Name, pod.Namespace)
		if err := p.Client.DeletePod(pod); err != nil {
			klog.V(5).Infof("delete po:%s err:%v", pod.Name, err)
			return err
		}
	}
	return nil
}

func (p *PodDriver) podDeletedHandler(ctx context.Context, pod *corev1.Pod) error {
	if pod == nil {
		klog.Errorf("get nil pod")
		return nil
	}
	klog.V(5).Infof("Get pod %s in namespace %s is to be deleted.", pod.Name, pod.Namespace)

	// pod with no finalizer
	if !util.ContainsString(pod.GetFinalizers(), config.Finalizer) {
		// do nothing
		return nil
	}

	// pod with resource error
	if util.IsPodResourceError(pod) {
		klog.V(5).Infof("The pod is PodResourceError, podDeletedHandler skip delete the pod:%s", pod.Name)
		return nil
	}

	// remove finalizer of pod
	klog.V(5).Infof("Remove finalizer of pod %s namespace %s", pod.Name, pod.Namespace)
	if err := util.RemoveFinalizer(ctx, p.Client, pod, config.Finalizer); err != nil {
		klog.Errorf("remove pod finalizer err:%v", err)
		return err
	}

	// check if need to do recovery
	klog.V(6).Infof("Annotations:%v", pod.Annotations)
	if pod.Annotations == nil {
		return nil
	}
	var targets = make([]string, 0)
	for k, v := range pod.Annotations {
		if k == util.GetReferenceKey(v) {
			targets = append(targets, v)
		}
	}
	if len(targets) == 0 {
		// do not need recovery
		return nil
	}

	// get mount point
	sourcePath, _, err := util.GetMountPathOfPod(*pod)
	if err != nil {
		klog.Error(err)
		return nil
	}

	lock := config.GetPodLock(pod.Name)
	lock.Lock()
	defer lock.Unlock()

	// create the pod even if get err
	defer func() {
		// check pod delete
		for i := 0; i < 30; i++ {
			if _, err := p.Client.GetPod(pod.Name, pod.Namespace); err == nil {
				time.Sleep(time.Second * 5)
			} else {
				if apierrors.IsNotFound(err) {
					break
				}
				klog.Errorf("get pod err:%v", err) // create pod even if get err
				break
			}
		}
		// create pod
		var newPod = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        pod.Name,
				Namespace:   pod.Namespace,
				Labels:      pod.Labels,
				Annotations: pod.Annotations,
			},
			Spec: pod.Spec,
		}
		controllerutil.AddFinalizer(newPod, config.Finalizer)
		_, err := p.Client.CreatePod(newPod)
		if err != nil {
			klog.Errorf("create pod:%s err:%v", pod.Name, err)
		}
	}()

	// umount mount point before recreate mount pod
	klog.Infof("start umount :%s", sourcePath)
	util.UmountPath(ctx, sourcePath)
	// create
	klog.V(5).Infof("pod targetPath not empty, need create pod:%s", pod.Name)
	return nil
}

func (p *PodDriver) podReadyHandler(ctx context.Context, pod *corev1.Pod) error {
	if pod == nil {
		klog.Errorf("[podReadyHandler] get nil pod")
		return nil
	}
	if pod.Annotations == nil {
		return nil
	}
	// get mount point
	mntPath, volumeId, err := util.GetMountPathOfPod(*pod)
	if err != nil {
		klog.Error(err)
		return nil
	}

	// staticPv has no subPath, check sourcePath
	sourcePath := fmt.Sprintf("%s/%s", mntPath, volumeId)
	_, err = os.Stat(sourcePath)
	if err != nil {
		if !os.IsNotExist(err) {
			klog.Errorf("stat volPath:%s err:%v, don't do recovery", sourcePath, err)
			return nil
		}
		sourcePath = mntPath
		if _, err2 := os.Stat(sourcePath); err2 != nil {
			klog.Errorf("stat volPath:%s err:%v, don't do recovery", sourcePath, err2)
			return nil
		}
	}

	// recovery for each target
	mountOption := []string{"bind"}
	for k, v := range pod.Annotations {
		if k == util.GetReferenceKey(v) {
			cmd2 := fmt.Sprintf("start exec cmd: mount -o bind %s %s \n", sourcePath, v)
			// check target should do recover
			_, err := os.Stat(v)
			if err == nil {
				klog.V(5).Infof("target path %s is normal, don't need do recover", v)
				continue
			} else if os.IsNotExist(err) {
				klog.V(5).Infof("target %s not exists,  don't do recover", v)
				continue
			}
			klog.V(5).Infof("Get pod %s in namespace %s is ready, %s", pod.Name, pod.Namespace, cmd2)
			if err := p.Mount(sourcePath, v, "none", mountOption); err != nil {
				klog.Errorf("exec cmd: mount -o bind %s %s err:%v", sourcePath, v, err)
			}
		}
	}
	return nil
}

func (p *PodDriver) podPendingHandler(ctx context.Context, pod *corev1.Pod) error {
	if pod == nil {
		return nil
	}
	// 如果MountPod由于意外情况，导致客户端退出，其状态可能是complete,因此不移除注解，直接删除处于complete状态的MountPod，触发重建
	if pod.Status.Phase == corev1.PodSucceeded {
		if err := p.Client.DeletePod(pod); err != nil {
			klog.Errorf("mountPod status is complete, but delete mountPod:%s failed, error:%s", pod.Name, err)
			return err
		}
		klog.V(5).Infof("delete complete mountPod:%s success", pod.Name)
	}
	// requeue
	return nil
}

// getPodStatus get pod status
func getPodStatus(pod *corev1.Pod) podStatus {
	if pod == nil {
		return podError
	}
	if pod.DeletionTimestamp != nil {
		return podDeleted
	}
	if util.IsPodError(pod) {
		return podError
	}
	if util.IsPodReady(pod) {
		return podReady
	}
	return podPending
}
