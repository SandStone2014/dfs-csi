#!/bin/bash
# must execute build-package.sh on project root
set -e
set -x

VERSION=$1
if [ ! -n "$VERSION" ]; then
  echo "please input csi plugin version"
  exit 1
fi

dir_name="sandstone-mosfsplugin"
output_dir="build_package"
dir_with_version=$output_dir/$dir_name-$VERSION
sandstone_mosfsplugin_image="sandstone/mosfs-csi-driver:$VERSION sandstone/cache-controller:$VERSION"
sandstone_mosfsplugin_image_tar="$dir_with_version/sandstone-mosfsplugin.tar"
sidecar_v1_17_image_tar="$dir_with_version/k8s1.17+/sidecar.tar"
#sidecar_v1_16_image_tar="$dir_with_version/v1.16/sidecar.tar"


sandstone_external_registry="10.10.3.12:8000"
#sandstone_helm_download="10.10.3.12:8081/repository/sdshosts/amd64/el7/helm/"
#helm_v1_17_version="v3.6.3"
# sidecar_new_images for k8s 1.17+
sidecar_new_images="k8s.gcr.io/sig-storage/csi-node-driver-registrar:v2.4.0 registry.k8s.io/sig-storage/csi-provisioner:v3.0.0 k8s.gcr.io/sig-storage/csi-resizer:v1.4.0 registry.k8s.io/sig-storage/csi-snapshotter:v4.1.1 quay.io/k8scsi/livenessprobe:v1.1.0 registry.k8s.io/sig-storage/snapshot-controller:v4.1.1"
sidecar_v1_16_images="quay.io/k8scsi/csi-node-driver-registrar:v1.3.0 quay.io/k8scsi/csi-provisioner:v1.4.0 quay.io/k8scsi/csi-resizer:v1.0.0 quay.io/k8scsi/csi-snapshotter:v1.2.1 quay.io/k8scsi/livenessprobe:v1.1.0"


function save_image_to_tar() {
  # save image
  images=$1
  sidecar_tar=$2
  sandstone_mosfsplugin_tar=$3


  arr=(${images// / })
  for i in "${arr[@]}"; do
    docker pull $sandstone_external_registry/$i
    docker tag $sandstone_external_registry/$i $i
  done
  echo "$images" | xargs docker save -o "$sidecar_tar"
  echo "$sandstone_mosfsplugin_image" | xargs docker save -o "$sandstone_mosfsplugin_tar"
}

# build package
if [ -d $output_dir ]; then
  rm -rf $output_dir
fi
mkdir -p "$dir_with_version"

# copy k8s resource yaml
cp -r charts/k8s1.17+ "$dir_with_version"

#cp -r deploy/kubernetes/v1.16 "$dir_with_version"


# save k8s 1.17-1.21 tar
save_image_to_tar "$sidecar_new_images" "$sidecar_v1_17_image_tar" "$sandstone_mosfsplugin_image_tar"
# save k8s 1.16 tar
#save_image_to_tar "$sidecar_v1_16_images" "$sidecar_v1_16_image_tar" "$sandstone_mosfsplugin_image_tar"

# save doc and example
cp -r example "$dir_with_version"
cp docs/SandStone\ MOSFS\ CSI\ Plugin\ 操作手册.docx "$dir_with_version"

# wget download helm binary
#wget -P $dir_with_version/k8s1.17+ $sandstone_helm_download/$helm_v1_17_version/helm
#chmod +x $dir_with_version/k8s1.17+/helm

# zip package
cd $output_dir
zip -r $dir_name-"$VERSION".zip $dir_name-"$VERSION"

# generate checksum file
md5sum $dir_name-"$VERSION".zip > MD5.txt

tar -cf sandstone-mosfsplugin.tar $dir_name-"$VERSION".zip MD5.txt

echo "############ build package success ,remember update package version #########"