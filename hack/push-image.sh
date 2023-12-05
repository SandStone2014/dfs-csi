set -e
set -x


images="k8s.gcr.io/sig-storage/csi-node-driver-registrar:v2.4.0 k8s.gcr.io/sig-storage/csi-provisioner:v2.1.2 k8s.gcr.io/sig-storage/csi-resizer:v1.4.0 k8s.gcr.io/sig-storage/csi-snapshotter:v3.0.3 quay.io/k8scsi/livenessprobe:v1.1.0 k8s.gcr.io/sig-storage/snapshot-controller:v3.0.3"
sandstone_external_registry="10.10.3.12:8010"
arr=(${images// / })
for i in "${arr[@]}"; do
  docker pull $i --platform amd64
  docker tag $i $sandstone_external_registry/$i
#  docker push $sandstone_external_registry/$i
done