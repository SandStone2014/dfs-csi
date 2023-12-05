
console=20.20.51.211:20101

curl -sS $console/static/Linux/ceph.conf >/dev/null 
if [ $? -ne 0 ];then
 echo Get $console/static/Linux/ceph.conf failed,please check console ip and port.
 exit 1
fi

cat <<EOF |kubectl apply -f -
apiVersion: v1
kind: Secret
metadata:
  name: ceph-conf
  namespace: szsandstone
type: Opaque
data:
  ceph.conf: $(curl -sS $console/static/Linux/ceph.conf|base64 -w 0)
  ceph.client.admin.keyring: ""
EOF
