FROM golang:1.17-buster as builder
WORKDIR /workspace
COPY . /workspace
RUN make juicefs-csi-driver




FROM dfs-base:v1
ARG JFS_AUTO_UPGRADE
WORKDIR /app
ENV JUICEFS_CLI=/usr/bin/dfscli
#控制mount前是否从juicefs官网升级cli
#ENV JFS_AUTO_UPGRADE=${JFS_AUTO_UPGRADE:-enabled}
ENV JFS_MOUNT_PATH=/usr/local/dfsmount
# 如果JFS_MOUNT_PATH不存在，执行dfscli auth、mount时会尝试从console下载juciefs client 二进制程序到此

COPY --from=builder /workspace/bin/juicefs-csi-driver /bin/mosfs-csi-driver
ENTRYPOINT ["/bin/mosfs-csi-driver"]
