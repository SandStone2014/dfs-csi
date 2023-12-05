package juicefs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
)

var DefaultTimeout = 15

type VolumeInfo struct {
	Id   int    `josn:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}
type QuotaInfo struct {
	Id    int    `josn:"id"`
	Volid int    `json:"-"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Inode int64  `json:"inodes"`
}

type Console struct {
	Addr  string
	Token string
}

type Acl struct {
	ID         int    `json:"id"`
	Desc       string `json:"desc"`
	Iprange    string `json:"iprange"`
	Apionly    bool   `json:"apionly"`
	Readonly   bool   `json:"readonly"`
	Appendonly bool   `json:"appendonly"`
	Token      string `json:"token"`
	Qos        string `json:"qos"`
	Extend     string `json:"extend"`
}

func runHttpRequestRaw(req *http.Request) ([]byte, error) {
	// create http client
	client := &http.Client{}

	// add req timeout setup
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*time.Duration(DefaultTimeout))

	// if not defer ,go vet  will warning
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := client.Do(req)
	if err != nil {
		klog.Errorf("http request %s timeout, %s", req.URL, err)
		return nil, status.Errorf(codes.Internal, "Reqest console %v failed: %v", req, err)
	}
	err = nil
	if resp.StatusCode >= http.StatusBadRequest {
		klog.Errorf("http request %s error, status %d", req.URL, resp.StatusCode)
		err = status.Errorf(codes.Internal, "Request console %v failed: status %d", req, resp.StatusCode)
	}
	defer resp.Body.Close()
	bodyc, errreadbody := ioutil.ReadAll(resp.Body)
	if errreadbody != nil {
		klog.Errorf("http request %s read body failed %v", req.URL, errreadbody)
		if err == nil {
			err = errreadbody
		}
	}
	return bodyc, err

}

func (cs *Console) GetVolumes() ([]VolumeInfo, error) {
	url := fmt.Sprintf("http://%s/api/v1/volumes?token=%s", cs.Addr, cs.Token)
	req, _ := http.NewRequest("GET", url, nil)
	resp, err := runHttpRequestRaw(req)
	if err != nil {
		return []VolumeInfo{}, err
	}
	klog.V(6).Infof("get vols:%s", string(resp))
	var volumeinfo []VolumeInfo
	err = json.Unmarshal(resp, &volumeinfo)
	return volumeinfo, err
}

func (cs *Console) volume(name string) (*VolumeInfo, error) {
	vols, err := cs.GetVolumes()
	if err != nil {
		return nil, err
	}
	for _, v := range vols {
		if v.Name == name {
			return &v, nil
		}
	}
	return nil, status.Errorf(codes.Internal, "Volume not found %s", name)

}

func (cs *Console) GetQuotas(volid int) ([]QuotaInfo, error) {
	url := fmt.Sprintf("http://%s/api/v1/volumes/%d/quotas?token=%s", cs.Addr, volid, cs.Token)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return []QuotaInfo{}, err
	}
	resp, err := runHttpRequestRaw(req)
	if err != nil {
		return []QuotaInfo{}, err
	}
	klog.V(6).Infof("get quotas:%s", string(resp))
	var quotas []QuotaInfo
	err = json.Unmarshal(resp, &quotas)
	return quotas, err
}

func (cs *Console) CreateQuota(volume_name, path string, inodes, size int64) error {
	vol, err := cs.volume(volume_name)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://%s/api/v1/volumes/%d/quotas?token=%s", cs.Addr, vol.Id, cs.Token)
	type quota struct {
	}
	body, err := json.Marshal(map[string]interface{}{
		"path":   path,
		"inodes": inodes,
		"size":   size,
	})
	klog.V(5).Infof("createquota url %s body %s\n", url, string(body))
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(body))
	req.Header.Add("Content-Type", "application/json")
	resp, err := runHttpRequestRaw(req)
	klog.V(5).Infof("createquota response: %s,err %v", string(resp), err)
	return err
}

func (cs *Console) GetQuota(volume_name, path string) (*QuotaInfo, error) {
	vol, err := cs.volume(volume_name)
	if err != nil {
		return nil, err
	}
	quotas, err := cs.GetQuotas(vol.Id)
	if err != nil {
		return nil, err
	}
	var quota *QuotaInfo = nil
	for i, q := range quotas {
		if q.Path == path {
			quota = &quotas[i]
			quota.Volid = vol.Id
		}
	}

	return quota, nil
}

func (cs *Console) DeleteQuota(volume_name, path string) error {

	quota, err := cs.GetQuota(volume_name, path)
	if err != nil {
		return err
	}

	if quota != nil {
		url := fmt.Sprintf("http://%s/api/v1/volumes/%d/quotas/%d?token=%s", cs.Addr, quota.Volid, quota.Id, cs.Token)

		klog.V(5).Infof("DeleteQuota url %s ", url)
		req, _ := http.NewRequest("DELETE", url, nil)
		resp, err := runHttpRequestRaw(req)
		klog.V(5).Infof("delete quota response: %s,err %v", string(resp), err)
		return err
	} else {
		return nil
	}
}

func (cs *Console) UpdateQuota(quota *QuotaInfo) error {

	url := fmt.Sprintf("http://%s/api/v1/volumes/%d/quotas/%d?token=%s", cs.Addr, quota.Volid, quota.Id, cs.Token)
	body, err := json.Marshal(quota)
	klog.V(5).Infof("UpdateQuota url %s body %s", url, string(body))
	req, _ := http.NewRequest("PUT", url, bytes.NewBuffer(body))
	req.Header.Add("Content-Type", "application/json")
	resp, err := runHttpRequestRaw(req)
	klog.V(5).Infof("update quota response: %s,err %v", string(resp), err)
	return err
}

func (cs *Console) GetACLList(volumeName string) ([]Acl, error) {
	vol, err := cs.volume(volumeName)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("http://%s/api/v1/volumes/%d/exports?token=%s", cs.Addr, vol.Id, cs.Token)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := runHttpRequestRaw(req)
	if err != nil {
		return nil, err
	}
	acl := make([]Acl, 0)
	if err := json.Unmarshal(resp, &acl); err != nil {
		return nil, err
	}
	return acl, nil
}

func (cs *Console) UpdateQos(qos string, volumeName string, acl *Acl) error {
	vol, err := cs.volume(volumeName)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("http://%s/api/v1/volumes/%d/exports/%d?token=%s", cs.Addr, vol.Id, acl.ID, cs.Token)
	updateBody := struct {
		Desc       string `json:"desc"`
		Iprange    string `json:"iprange"`
		Readonly   bool   `json:"readonly"`
		Appendonly bool   `json:"appendonly"`
		Qos        string `json:"qos"`
		Extend     string `json:"extend"`
	}{}
	updateBody.Desc = acl.Desc
	updateBody.Iprange = acl.Iprange
	updateBody.Readonly = acl.Readonly
	updateBody.Appendonly = acl.Appendonly
	updateBody.Qos = qos
	updateBody.Extend = acl.Extend

	body, err := json.Marshal(updateBody)
	if err != nil {
		return err
	}
	klog.V(5).Infof("set qos url %s body %s", url, string(body))
	req, err := http.NewRequest("PUT", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Add("Content-Type", "application/json")
	resp, err := runHttpRequestRaw(req)
	if err != nil {
		return err
	}
	klog.V(5).Infof("set qos response:%s, err %v", string(resp), err)
	return nil
}

func (cs *Console) SetQos(volumeName string, qos string) error {
	klog.V(5).Infof("set volume:%s, qos:%s", volumeName, qos)
	acls, err := cs.GetACLList(volumeName)
	if err != nil {
		return fmt.Errorf("get volume:%s acl list error:%s", volumeName, err)
	}
	var targetACL *Acl
	for i := range acls {
		if acls[i].Desc == "default" {
			targetACL = &acls[i]
			break
		}
	}

	if targetACL == nil {
		return fmt.Errorf("volume:%s not contains default acl", volumeName)
	}

	if err := cs.UpdateQos(qos, volumeName, targetACL); err != nil {
		return fmt.Errorf("update volume:%s qos, error:%s", volumeName, err)
	}
	return nil
}
