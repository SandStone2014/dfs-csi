/*
 Copyright 2023 Juicedata Inc

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
	"dfs-csi/pkg/driver"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog"
	k8sexec "k8s.io/utils/exec"
	"k8s.io/utils/mount"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"dfs-csi/pkg/juicefs/config"
	"dfs-csi/pkg/juicefs/k8sclient"
	"dfs-csi/pkg/util"
)

type PodController struct {
	k8sclient.K8sClient
}

func NewPodController(client k8sclient.K8sClient) *PodController {
	return &PodController{client}
}

func (m *PodController) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	klog.V(6).Infof("Receive pod %s %s", request.Name, request.Namespace)
	mountPod, err := m.GetPod(request.Name, request.Namespace)
	if err != nil && !k8serrors.IsNotFound(err) {
		klog.Errorf("get pod %s error: %v", request.Name, err)
		return reconcile.Result{}, err
	}
	if mountPod == nil {
		klog.V(6).Infof("pod %s has been deleted.", request.Name)
		return reconcile.Result{}, nil
	}
	if mountPod.Spec.NodeName != config.NodeName && mountPod.Spec.NodeSelector["kubernetes.io/hostname"] != config.NodeName {
		klog.V(6).Infof("pod %s/%s is not on node %s, skipped", mountPod.Namespace, mountPod.Name, config.NodeName)
		return reconcile.Result{}, nil
	}

	// get mount info
	mit := newMountInfoTable()
	if err := mit.parse(); err != nil {
		klog.Errorf("doReconcile ParseMountInfo: %v", err)
		return reconcile.Result{}, err
	}

	// get app pod list
	podList, err := m.ListAppPod(ctx, mountPod)
	if err != nil {
		klog.Errorf("doReconcile ListPod: %v", err)
		return reconcile.Result{}, err
	}

	mounter := mount.SafeFormatAndMount{
		Interface: mount.New(""),
		Exec:      k8sexec.New(),
	}

	podDriver := NewPodDriver(m.K8sClient, mounter)
	podDriver.SetMountInfo(*mit)
	podDriver.mit.setPodsStatus(&corev1.PodList{Items: podList})

	err = podDriver.Run(ctx, mountPod)
	if err != nil {
		klog.Errorf("Driver check pod %s error: %v", mountPod.Name, err)
		return reconcile.Result{}, err
	}
	if mountPod.Annotations[config.DeleteDelayAtKey] != "" {
		// if mount pod set delay deleted, requeue after delay time
		delayAtStr := mountPod.Annotations[config.DeleteDelayAtKey]
		delayAt, err := util.GetTime(delayAtStr)
		if err != nil {
			return reconcile.Result{}, err
		}
		now := time.Now()
		requeueAfter := delayAt.Sub(now)
		if delayAt.Before(now) {
			requeueAfter = 0
		}
		return reconcile.Result{
			Requeue:      true,
			RequeueAfter: requeueAfter,
		}, nil
	}
	return reconcile.Result{}, nil
}

func (m *PodController) ListAppPod(ctx context.Context, mountPod *corev1.Pod) ([]corev1.Pod, error) {
	// 共享mountPod时,会存在多个pv都使用同一个mountPod，因此查询业务Pod，需要查询多个namespace
	storeNamespace := make(map[string]struct{})
	pvs, err := m.K8sClient.ListPersistentVolumes(ctx, nil, nil)
	if err != nil {
		klog.Errorf("ListPV error:%v", err)
		return nil, err
	}
	for _, pv := range pvs {
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != driver.DriverName {
			continue
		}
		if pv.Spec.ClaimRef != nil {
			pvcNamespace := pv.Spec.ClaimRef.Namespace
			_, ok := storeNamespace[pvcNamespace]
			if !ok {
				storeNamespace[pvcNamespace] = struct{}{}
			}
		}
	}
	klog.V(6).Infof("use csi app namespace:%+v", storeNamespace)

	result := make([]corev1.Pod, 0)
	for ns, _ := range storeNamespace {
		podList, err := m.K8sClient.ListPod(ctx, ns, nil, nil)
		if err != nil {
			klog.Errorf("list app pod error: %v", err)
			return nil, err
		}
		result = append(result, podList...)
	}
	return result, nil
}

func (m *PodController) SetupWithManager(mgr ctrl.Manager) error {
	c, err := controller.New("mount", mgr, controller.Options{Reconciler: m})
	if err != nil {
		return err
	}

	return c.Watch(&source.Kind{Type: &corev1.Pod{}}, &handler.EnqueueRequestForObject{}, predicate.Funcs{
		CreateFunc: func(event event.CreateEvent) bool {
			pod := event.Object.(*corev1.Pod)
			klog.V(6).Infof("watch pod %s created", pod.GetName())
			return true
		},
		UpdateFunc: func(updateEvent event.UpdateEvent) bool {
			podNew, ok := updateEvent.ObjectNew.(*corev1.Pod)
			klog.V(6).Infof("watch pod %s updated", podNew.GetName())
			if !ok {
				klog.V(6).Infof("pod.onUpdateFunc Skip object: %v", updateEvent.ObjectNew)
				return false
			}

			podOld, ok := updateEvent.ObjectOld.(*corev1.Pod)
			if !ok {
				klog.V(6).Infof("pod.onUpdateFunc Skip object: %v", updateEvent.ObjectOld)
				return false
			}

			if podNew.GetResourceVersion() == podOld.GetResourceVersion() {
				klog.V(6).Info("pod.onUpdateFunc Skip due to resourceVersion not changed")
				return false
			}
			return true
		},
		DeleteFunc: func(deleteEvent event.DeleteEvent) bool {
			pod := deleteEvent.Object.(*corev1.Pod)
			klog.V(6).Infof("watch pod %s deleted", pod.GetName())
			return true
		},
	})
}
