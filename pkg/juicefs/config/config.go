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

package config

import (
	"hash/fnv"
	"sync"
	"time"
)

var JLock = sync.RWMutex{}

var (
	NodeName    = ""
	Namespace   = ""
	PodName     = ""
	MountImage  = ""
	HostIp      = ""
	KubeletPort = ""

	MountPointPath       = "/var/lib/mosfs/volume"
	JFSConfigPath        = "/var/lib/mosfs/config"
	JFSMountPriorityName = "system-node-critical"

	PodMountBase = "/mosfs"
	MountBase    = "/var/lib/mosfs"
	FsType       = "juicefs" //需修改dfscli和client二进制程序才能彻底改这个类型。
	CliPath      = "/usr/bin/dfscli"
	CeCliPath    = CliPath               //社区版路径，不会用到
	CeMountPath  = JfsMountPath          //社区版路径，不会用到
	JfsMountPath = "/sbin/mount.juicefs" //dfscli auth时会建此路径建链接到CliPath

	ReconcilerInterval = 5
	ReconcileTimeout   = 5 * time.Minute

	// DeleteDelayTimeKey mount pod annotation
	DeleteDelayTimeKey = "mosfs-delete-delay"
	DeleteDelayAtKey   = "mosfs-delete-at"
)

const (
	PodTypeKey   = "app.kubernetes.io/name"
	PodTypeValue = "mosfs-mount"
	Finalizer    = "szsandstone.com/mosfsfinalizer"
)

var PodLocks [1024]sync.Mutex

func GetPodLock(podName string) *sync.Mutex {
	h := fnv.New32a()
	h.Write([]byte(podName))
	index := int(h.Sum32())
	return &PodLocks[index%1024]
}
