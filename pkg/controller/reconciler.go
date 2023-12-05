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
	"k8s.io/client-go/util/flowcontrol"
	k8sexec "k8s.io/utils/exec"
	"k8s.io/utils/mount"
	"strconv"
	"time"

	"dfs-csi/pkg/juicefs/config"
	"dfs-csi/pkg/juicefs/k8sclient"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog"
)

const (
	retryPeriod    = 5 * time.Second
	maxRetryPeriod = 300 * time.Second
)

func StartReconciler() error {
	// gen kubelet client
	port, err := strconv.Atoi(config.KubeletPort)
	if err != nil {
		return err
	}
	kc, err := k8sclient.NewKubeletClient(config.HostIp, port)
	if err != nil {
		return err
	}

	// check if kubelet can be connected
	_, err = kc.GetNodeRunningPods()
	if err != nil {
		return err
	}

	// gen podDriver
	k8sClient, err := k8sclient.NewClient()
	if err != nil {
		klog.V(5).Infof("Could not create k8s client %v", err)
		return err
	}

	go doReconcile(k8sClient, kc)
	klog.V(5).Infof("Start Reconciler of polling kubelet success")
	return nil

	//k8sClient, err := k8sclient.NewClient()
	//if err != nil {
	//	klog.V(5).Infof("Could not create k8s client %v", err)
	//	return fmt.Errorf("new k8s client error:%s", err)
	//}
	//reconciler := &PodReconciler{
	//	K8sClient:    k8sClient,
	//	AllPodStatus: make(map[string]bool),
	//}
	//// TODO 增加超时Ctx
	//go reconciler.Reconcile(context.TODO())
	//
	//return nil
}

type PodReconciler struct {
	k8sclient.K8sClient
	// allPodStatus record exist pod status in cluster status: key-->podId, bool-->isDeleting,judge by DeletionTimestamp
	AllPodStatus map[string]bool
}

func (p *PodReconciler) getClusterPodsStatus() (*corev1.PodList, error) {
	podList, err := p.K8sClient.GetAllPod()
	if err != nil {
		return nil, fmt.Errorf("set cluster pods status error:%s", err)
	}
	p.AllPodStatus = make(map[string]bool)
	for _, pod := range podList.Items {
		if pod.DeletionTimestamp != nil {
			p.AllPodStatus[string(pod.UID)] = true
		} else {
			p.AllPodStatus[string(pod.UID)] = false
		}

	}
	return podList, nil
}

func doReconcile(ks k8sclient.K8sClient, kc *k8sclient.KubeletClient) {
	backOff := flowcontrol.NewBackOff(retryPeriod, maxRetryPeriod)
	for {
		ctx := context.TODO()
		timeoutCtx, cancel := context.WithTimeout(context.Background(), config.ReconcileTimeout)
		g, ctx := errgroup.WithContext(timeoutCtx)

		mit := newMountInfoTable()
		podList, err := kc.GetNodeRunningPods()
		if err != nil {
			klog.Errorf("doReconcile GetNodeRunningPods: %v", err)
			goto finish
		}
		if err := mit.parse(); err != nil {
			klog.Errorf("doReconcile ParseMountInfo: %v", err)
			goto finish
		}

		for i := range podList.Items {
			pod := &podList.Items[i]
			if pod.Namespace != config.Namespace {
				continue
			}
			// check label
			if value, ok := pod.Labels[config.PodTypeKey]; !ok || value != config.PodTypeValue {
				continue
			}

			backOffID := fmt.Sprintf("mountpod/%s", pod.Name)
			g.Go(func() error {
				mounter := mount.SafeFormatAndMount{
					Interface: mount.New(""),
					Exec:      k8sexec.New(),
				}

				podDriver := NewPodDriver(ks, mounter)
				podDriver.SetMountInfo(*mit)
				podDriver.mit.setPodsStatus(podList)

				select {
				case <-ctx.Done():
					klog.Infof("goroutine of pod %s cancel", pod.Name)
					return nil
				default:
					if !backOff.IsInBackOffSinceUpdate(backOffID, backOff.Clock.Now()) {
						err = podDriver.Run(ctx, pod)
						if err != nil {
							klog.Errorf("Driver check pod %s error, will retry: %v", pod.Name, err)
							backOff.Next(backOffID, time.Now())
							return err
						}
						backOff.Reset(backOffID)
					}
				}
				return nil
			})
		}
		backOff.GC()
		g.Wait()
	finish:
		cancel()
		time.Sleep(time.Duration(config.ReconcilerInterval) * time.Second)
	}
}
