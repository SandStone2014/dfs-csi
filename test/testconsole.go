package main

import (
	"flag"
	"fmt"
	"os"

	"dfs-csi/pkg/juicefs"

	"k8s.io/klog"
)

var (
	path string

	action  string
	volname string
	size    int64
)

func main() {
	klog.InitFlags(nil)
	flag.StringVar(&path, "path", "", "path")
	flag.StringVar(&volname, "volume", "", "vol name")

	flag.StringVar(&action, "action", "ls", "ls:list vol, lsq:list quota, cq:create quota, dq:delete quota, uq:update quota")
	flag.Int64Var(&size, "size", 0, "quota size")

	if err := flag.Set("logtostderr", "true"); err != nil {
		klog.Exitf("failed to set logtostderr flag: %v", err)
	}
	flag.Parse()

	cs := juicefs.Console{
		Addr:  "20.20.51.122:9400",
		Token: "6b175b78b974e4121bb4dbf9e7ae3568ab2ce901",
	}
	if action == "ls" {
		vols, err := cs.GetVolumes()
		fmt.Printf("vols:\n%v\nerr %v\n", vols, err)
		if err != nil {
			fmt.Printf("err:%v \n", err)
		}
	} else if action == "lsq" {
		vols, err := cs.GetVolumes()
		if err != nil {
			fmt.Printf("get volume failed")
			os.Exit(1)
		}
		var volid int = -1
		for _, v := range vols {
			if v.Name == volname {
				volid = v.Id
			}
		}
		if volid == -1 {
			fmt.Print("vol not found")
			os.Exit(1)
		}
		quotas, err := cs.GetQuotas(volid)
		fmt.Printf("GetQuotas %v err %v\n", quotas, err)
	} else if action == "cq" {
		fmt.Printf("CreateQuota %v\n", cs.CreateQuota(volname, path, 0, size))
	} else if action == "dq" {
		fmt.Printf("DeleteQuota %v\n", cs.DeleteQuota(volname, path))
	} else if action == "uq" {
		q, err := cs.GetQuota(volname, path)
		if err != nil {
			fmt.Printf("getquota failed %v", err)
			os.Exit(1)
		}
		if q != nil {
			q.Size = size
			fmt.Printf("UpdateQuota %v\n", cs.UpdateQuota(q))
		} else {
			fmt.Println("quota not found")
		}
	}

}
