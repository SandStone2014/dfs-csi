/*
Copyright 2022.

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

package controllers

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	log2 "sigs.k8s.io/controller-runtime/pkg/log"

	cachev1alpha1 "dfs-csi/pkg/warmup/api/v1alpha1"
)

// CacheReconciler reconciles a Cache object
type CacheReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	ClientSet *kubernetes.Clientset
	Config    *rest.Config
}

var (
	retryTimeout = 15 * time.Second
	log          = ctrl.Log.WithName("setup")
)

//+kubebuilder:rbac:groups=cache.sandstone.com,resources=caches,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cache.sandstone.com,resources=caches/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cache.sandstone.com,resources=caches/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete;
//+kubebuilder:rbac:groups=core,resources=pods/exec,verbs=get;list;watch;create;update;patch;delete;
//+kubebuilder:rbac:groups=core,resources=pods/attach,verbs=get;list;watch;create;update;patch;delete;
//+kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete;

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// the Cache object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile
func (r *CacheReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log2.FromContext(ctx)

	cache := &cachev1alpha1.Cache{}
	err := r.Get(ctx, req.NamespacedName, cache)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Error(err, fmt.Sprintf("ns:%s, cache:%s, not found, ignore", req.Namespace, req.Name))
			return ctrl.Result{}, nil
		}
		log.Info(fmt.Sprintf("get cache object failed, error:%s", err))
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: retryTimeout,
		}, err
	}
	// handler delete
	if cache.DeletionTimestamp != nil {
		log.Info(fmt.Sprintf("ns:%s cache object:%s will delete", cache.Namespace, cache.Name))
		// delete it
		if err := r.Delete(ctx, cache); err != nil {
			if !apierrors.IsNotFound(err) {
				log.Error(err, fmt.Sprintf("ns:%s, delete cache object:%s failed, error:%s", req.Namespace, req.Name, err))
				return ctrl.Result{
					Requeue:      true,
					RequeueAfter: retryTimeout,
				}, err
			}
		}
		return ctrl.Result{}, nil
	}
	// handle cached
	if cache.Status.Cached {
		log.Info(fmt.Sprintf("ns:%s cache object:%s already cached", cache.Namespace, cache.Name))
		return ctrl.Result{}, nil
	} else {
		// from pvc get cache path
		scName, pvName, err := r.getStorageClassNameAndPVName(ctx, req.Namespace, cache.Spec.PvcName)
		if err != nil {
			log.Error(err, fmt.Sprintf("get sc name and pv name failed,error:%s", err))
			return ctrl.Result{
				Requeue:      true,
				RequeueAfter: retryTimeout,
			}, err
		}

		// parse shareMount
		shareMount := os.Getenv("STORAGE_CLASS_SHARE_MOUNT") == "true"
		// if share mount cachePath is /mosfs/<storageclassName>/<pvName>/<subPath>
		// if not share, cachePath is /mosfs/<pvName>/<subPath>
		cachePath := fmt.Sprintf("/mosfs/%s/%s", scName, pvName)
		if !shareMount {
			cachePath = fmt.Sprintf("/mosfs/%s", pvName)
		}
		// cache pvc
		// check whether contains sub path
		if len(cache.Spec.Path) > 0 {
			cachePath = filepath.Join(cachePath, cache.Spec.Path)
		}
		log.Info(fmt.Sprintf("ns:%s check object:%s will cache path:%s", cache.Namespace, cache.Name, cachePath))

		// get use pvc related pod node name
		nodesName, err := r.fromPVCGetUsedPodsNodeName(ctx, req.Namespace, cache.Spec.PvcName)
		if err != nil {
			log.Error(err, fmt.Sprintf("get pvc related nodes failed, error:%s", err))
			return ctrl.Result{
				Requeue:      true,
				RequeueAfter: retryTimeout,
			}, err
		}
		if len(nodesName) == 0 {
			err = fmt.Errorf("%s", "nodeName is empty")
			log.Error(err, fmt.Sprintf("ns:%s ,pvc:%s related nodes is empty, "+
				"cancel warm up", req.Namespace, cache.Spec.PvcName))
			return ctrl.Result{
				Requeue:      false,
				RequeueAfter: retryTimeout,
			}, err
		}
		// parse namespace
		namespace := os.Getenv("MOSFS_MOUNT_NAMESPACE")
		if len(namespace) == 0 {
			err = fmt.Errorf("get namespace failed, namespace can not empty")
			log.Error(err, err.Error())
			return ctrl.Result{
				Requeue:      false,
				RequeueAfter: retryTimeout,
			}, err
		}

		if err := r.warmUpAll(ctx, scName, pvName, cachePath, namespace, shareMount, cache, nodesName...); err != nil {
			log.Error(err, fmt.Sprintf("warmup cache:%s failed", cache))
			return ctrl.Result{
				Requeue:      true,
				RequeueAfter: retryTimeout,
			}, err
		}
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *CacheReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cachev1alpha1.Cache{}).
		Complete(r)
}

func (r *CacheReconciler) getStorageClassNameAndPVName(ctx context.Context, namespace, pvcName string) (string, string, error) {
	pvc := &v1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: namespace,
		Name:      pvcName,
	}, pvc); err != nil {
		return "", "", fmt.Errorf("get pvc:%s, error:%w", pvcName, err)
	}
	storageClassName := *pvc.Spec.StorageClassName
	// check sc and pv
	pvName := pvc.Spec.VolumeName
	if len(storageClassName) == 0 {
		return "", "", fmt.Errorf("ns:%s, pvc:%s related storageclass is empty,"+
			" cache only support dynamic provision", namespace, pvcName)
	}
	if len(pvName) == 0 {
		return "", "", fmt.Errorf("ns:%s, pvc:%s not bound to pv", namespace, pvcName)
	}
	return storageClassName, pvName, nil
}

func (r *CacheReconciler) fromPVCGetUsedPodsNodeName(ctx context.Context, ns, pvcName string) ([]string, error) {
	podList := &v1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("list pod in namesapce:%s, error:%w", err)
	}
	nodesName := make([]string, 0)
	for _, pod := range podList.Items {
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim == nil {
				break
			} else {
				if volume.PersistentVolumeClaim.ClaimName == pvcName && len(pod.Spec.NodeName) > 0 {
					nodesName = append(nodesName, pod.Spec.NodeName)
				}
			}
		}
	}
	return nodesName, nil
}

func (r *CacheReconciler) warmUp(nodeName, scName, pvName, path, namespace string, shareMount bool) error {
	// if share mount, mount pod: mosfs-<nodeName>-<storageClassName>
	// if not share, mount pod: mosfs-<nodeName>-<pvName>
	mountPodName := fmt.Sprintf("mosfs-%s-%s", nodeName, scName)
	if !shareMount {
		mountPodName = fmt.Sprintf("mosfs-%s-%s", nodeName, pvName)
	}
	// check mount pod
	cmd := []string{"/usr/bin/dfscli", "warmup", path}
	log.Info(fmt.Sprintf("mount pod is:%s, warm up cmd:%v", mountPodName, cmd))
	// call rest client exec
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	req := r.ClientSet.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(mountPodName).
		SubResource("exec")
	option := &v1.PodExecOptions{
		Command: cmd,
		Stdout:  true,
		Stderr:  true,
	}

	req.VersionedParams(
		option,
		scheme.ParameterCodec,
	)
	exec, err := remotecommand.NewSPDYExecutor(r.Config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("new exec error:%s", err)
	}
	err = exec.Stream(remotecommand.StreamOptions{
		Stdout: stdout,
		Stderr: stderr,
	})
	if err != nil {
		log.Error(err, fmt.Sprintf("cmd:%#v, stdout:%s, stderr:%s, err:%s", cmd, stdout.String(), stderr.String(), err.Error()))
		return fmt.Errorf("stdout:%s, stderr:%s, err:%w", stdout.String(), stderr.String(), err)
	}
	log.Info(fmt.Sprintf("warm up mountPod:%s success, msg:%s", mountPodName, stdout.String()))
	return nil
}

func (r *CacheReconciler) warmUpAll(ctx context.Context, scName, pvName, path, namespace string, shareMount bool, cache *cachev1alpha1.Cache, nodesName ...string) error {
	var finalErr error
	for _, nodeName := range nodesName {
		err := r.warmUp(nodeName, scName, pvName, path, namespace, shareMount)
		if err != nil {
			log.Error(err, fmt.Sprintf("warm up node:%s, scName:%s, path:%s failed, error:%s", nodeName, scName, path, err))
			finalErr = err
			break
		}

	}
	// update cache status
	if finalErr != nil {
		cache.Status.Cached = false
		cache.Status.Message = finalErr.Error()
	} else {
		cache.Status.Cached = true
	}

	if err := r.Client.Status().Update(ctx, cache); err != nil {
		log.Error(err, fmt.Sprintf("update cache status failed, error:%s", err))
		return fmt.Errorf("update cache:%s, error:%s", cache.Name, err)
	}
	log.Info(fmt.Sprintf("update ns:%s, cache:%s status success", cache.Namespace, cache.Name))
	if finalErr != nil {
		return fmt.Errorf("warm up cahche:%s, error:%s", cache.Name, finalErr)
	}
	return nil
}
