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

package k8sclient

import (
	"bytes"
	"context"
	"io"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"os"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"
)

type PatchMapValue struct {
	Op    string            `json:"op"`
	Path  string            `json:"path"`
	Value map[string]string `json:"value"`
}

type PatchStringValue struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value"`
}

type PatchDelValue struct {
	Op   string `json:"op"`
	Path string `json:"path"`
}

type PatchListValue struct {
	Op    string   `json:"op"`
	Path  string   `json:"path"`
	Value []string `json:"value"`
}

type K8sClient interface {
	CreatePod(pod *corev1.Pod) (*corev1.Pod, error)
	GetPod(podName, namespace string) (*corev1.Pod, error)
	GetAllPod() (*corev1.PodList, error)
	PatchPod(ctx context.Context, pod *corev1.Pod, data []byte, pt types.PatchType) error
	UpdatePod(pod *corev1.Pod) error
	DeletePod(pod *corev1.Pod) error
	GetPersistentVolume(pvName string) (*corev1.PersistentVolume, error)
	GetPodLog(ctx context.Context, podName, namespace, containerName string) (string, error)
	ListPersistentVolumes(ctx context.Context, labelSelector *metav1.LabelSelector, filedSelector *fields.Set) ([]corev1.PersistentVolume, error)
	ListPod(ctx context.Context, namespace string, labelSelector *metav1.LabelSelector, filedSelector *fields.Set) ([]corev1.Pod, error)
}

type k8sClient struct {
	enableAPIServerListCache bool
	*kubernetes.Clientset
}

func NewClient() (K8sClient, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	var enableAPIServerListCache bool
	if os.Getenv("ENABLE_APISERVER_LIST_CACHE") == "true" {
		enableAPIServerListCache = true
	}
	return &k8sClient{enableAPIServerListCache, client}, nil
}

func (k *k8sClient) CreatePod(pod *corev1.Pod) (*corev1.Pod, error) {
	klog.V(6).Infof("Create pod %s", pod.Name)
	mntPod, err := k.CoreV1().Pods(pod.Namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		klog.V(5).Infof("Can't create pod %s: %v", pod.Name, err)
		return nil, err
	}
	return mntPod, nil
}

func (k *k8sClient) GetAllPod() (*corev1.PodList, error) {
	klog.V(6).Infof("begin get all pod")
	podList, err := k.Clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		klog.V(5).Infof("get all pod in cluster error:%s", err)
		return nil, err
	}
	return podList, nil
}

func (k *k8sClient) GetPod(podName, namespace string) (*corev1.Pod, error) {
	klog.V(6).Infof("Get pod %s", podName)
	mntPod, err := k.CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		klog.V(5).Infof("Can't get pod %s namespace %s: %v", podName, namespace, err)
		return nil, err
	}
	return mntPod, nil
}

func (k *k8sClient) UpdatePod(pod *corev1.Pod) error {
	klog.V(5).Infof("Update pod %v", pod.Name)
	_, err := k.CoreV1().Pods(pod.Namespace).Update(context.TODO(), pod, metav1.UpdateOptions{})
	return err
}

func (k *k8sClient) PatchPod(ctx context.Context, pod *corev1.Pod, data []byte, pt types.PatchType) error {
	if pod == nil {
		klog.V(5).Info("Patch pod: pod is nil")
		return nil
	}
	klog.V(6).Infof("Patch pod %v", pod.Name)
	_, err := k.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, pt, data, metav1.PatchOptions{})
	return err
}

func (k *k8sClient) DeletePod(pod *corev1.Pod) error {
	klog.V(5).Infof("Delete pod %v", pod.Name)
	return k.CoreV1().Pods(pod.Namespace).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})
}

func (k *k8sClient) GetPersistentVolume(pvName string) (*corev1.PersistentVolume, error) {
	klog.V(6).Infof("Get pv %s", pvName)
	mntPod, err := k.CoreV1().PersistentVolumes().Get(context.TODO(), pvName, metav1.GetOptions{})
	if err != nil {
		klog.V(6).Infof("Can't get pv %s : %v", pvName, err)
		return nil, err
	}
	return mntPod, nil
}

func (k *k8sClient) GetPodLog(ctx context.Context, podName, namespace, containerName string) (string, error) {
	klog.V(6).Infof("Get pod %s log", podName)
	tailLines := int64(20)
	req := k.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: containerName,
		TailLines: &tailLines,
	})
	podLogs, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer podLogs.Close()

	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, podLogs)
	if err != nil {
		return "", err
	}
	str := buf.String()
	return str, nil
}

func (k *k8sClient) ListPersistentVolumes(ctx context.Context, labelSelector *metav1.LabelSelector, filedSelector *fields.Set) ([]corev1.PersistentVolume, error) {
	klog.V(6).Infof("List pvs by labelSelector %v, fieldSelector %v", labelSelector, filedSelector)
	listOptions := metav1.ListOptions{}
	if labelSelector != nil {
		labelMap, err := metav1.LabelSelectorAsMap(labelSelector)
		if err != nil {
			return nil, err
		}
		listOptions.LabelSelector = labels.SelectorFromSet(labelMap).String()
	}
	if filedSelector != nil {
		listOptions.FieldSelector = fields.SelectorFromSet(*filedSelector).String()
	}
	pvList, err := k.CoreV1().PersistentVolumes().List(ctx, listOptions)
	if err != nil {
		klog.V(6).Infof("Can't list pv: %v", err)
		return nil, err
	}
	return pvList.Items, nil
}

func (k *k8sClient) ListPod(ctx context.Context, namespace string, labelSelector *metav1.LabelSelector, filedSelector *fields.Set) ([]corev1.Pod, error) {
	klog.V(6).Infof("List pod by labelSelector %v, fieldSelector %v", labelSelector, filedSelector)
	listOptions := metav1.ListOptions{}
	if k.enableAPIServerListCache {
		// set ResourceVersion="0" means the list response is returned from apiserver cache instead of etcd
		listOptions.ResourceVersion = "0"
	}
	if labelSelector != nil {
		labelMap, err := metav1.LabelSelectorAsSelector(labelSelector)
		if err != nil {
			return nil, err
		}
		listOptions.LabelSelector = labelMap.String()
	}
	if filedSelector != nil {
		listOptions.FieldSelector = fields.SelectorFromSet(*filedSelector).String()
	}

	podList, err := k.CoreV1().Pods(namespace).List(ctx, listOptions)
	if err != nil {
		klog.V(6).Infof("Can't list pod in namespace %s by labelSelector %v: %v", namespace, labelSelector, err)
		return nil, err
	}
	return podList.Items, nil
}
