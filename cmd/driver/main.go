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

package main

import (
	"dfs-csi/cmd/app"
	"flag"
	"fmt"
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
	"time"

	"dfs-csi/pkg/controller"
	"dfs-csi/pkg/driver"
	"dfs-csi/pkg/juicefs/config"
	k8s "dfs-csi/pkg/juicefs/k8sclient"

	"k8s.io/klog"
)

var (
	endpoint      = flag.String("endpoint", "unix://tmp/csi.sock", "CSI Endpoint")
	version       = flag.Bool("version", false, "Print the version and exit.")
	nodeID        = flag.String("nodeid", "", "Node ID")
	enableManager = flag.Bool("enable-manager", false, "Enable manager or not.")
)

func init() {
	klog.InitFlags(nil)
	flag.Parse()
	config.NodeName = os.Getenv("NODE_NAME")
	config.Namespace = os.Getenv("MOSFS_MOUNT_NAMESPACE")
	config.PodName = os.Getenv("POD_NAME")
	config.MountPointPath = os.Getenv("MOSFS_MOUNT_PATH")
	config.JFSConfigPath = os.Getenv("MOSFS_CONFIG_PATH")
	config.HostIp = os.Getenv("HOST_IP")
	config.KubeletPort = os.Getenv("KUBELET_PORT")

	if timeout := os.Getenv("MOSFS_RECONCILE_TIMEOUT"); timeout != "" {
		duration, _ := time.ParseDuration(timeout)
		if duration > config.ReconcileTimeout {
			config.ReconcileTimeout = duration
		}
	}

	if config.PodName == "" || config.Namespace == "" {
		klog.Fatalln("Pod name & namespace can't be null.")
		os.Exit(0)
	}
	k8sclient, err := k8s.NewClient()
	if err != nil {
		klog.V(5).Infof("Can't get k8s client: %v", err)
		os.Exit(0)
	}
	pod, err := k8sclient.GetPod(config.PodName, config.Namespace)
	if err != nil {
		klog.V(5).Infof("Can't get pod %s: %v", config.PodName, err)
		os.Exit(0)
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "mosfs-plugin" {
			config.MountImage = pod.Spec.Containers[i].Image
			return
		}
	}
	klog.V(5).Infof("Can't get container mosfs-plugin in pod %s", config.PodName)
	os.Exit(0)
}

func main() {
	if *version {
		info, err := driver.GetVersionJSON()
		if err != nil {
			klog.Fatalln(err)
		}
		fmt.Println(info)
		os.Exit(0)
	}
	if *nodeID == "" {
		klog.Fatalln("nodeID must be provided")
	}

	if *enableManager {
		needStartPodManager := false
		if config.KubeletPort != "" && config.HostIp != "" {
			//通过kubelet获取mountPod信息，避免频繁访问apiServer
			if err := controller.StartReconciler(); err != nil {
				klog.Errorf("start polling kubelet  error:%s", err)
				klog.V(5).Info("Could not Start Reconciler of polling kubelet and fallback to watch ApiServer.")
				needStartPodManager = true
			}
		} else {
			needStartPodManager = true
		}

		if needStartPodManager {
			go func() {
				ctx := ctrl.SetupSignalHandler()
				mgr, err := app.NewPodManager()
				if err != nil {
					klog.Fatalln(err)
				}

				if err := mgr.Start(ctx); err != nil {
					klog.Fatalln(err)
				}
			}()
		}
		klog.V(5).Infof("Pod Reconciler Started")
	}

	drv, err := driver.NewDriver(*endpoint, *nodeID)
	if err != nil {
		klog.Fatalln(err)
	}
	if err := drv.Run(); err != nil {
		klog.Fatalln(err)
	}
}
