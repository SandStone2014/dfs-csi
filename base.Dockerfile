FROM python:3.8-slim-buster
ARG JFS_AUTO_UPGRADE
WORKDIR /app
ENV JUICEFS_CLI=/usr/bin/dfscli
#控制mount前是否从juicefs官网升级cli
#ENV JFS_AUTO_UPGRADE=${JFS_AUTO_UPGRADE:-enabled}
ENV JFS_MOUNT_PATH=/usr/local/dfsmount
COPY THIRD-PARTY /
COPY bin_dependency/juicefs /usr/bin/dfscli
#librados相关so
COPY bin_dependency/*.so.* /lib/x86_64-linux-gnu/
RUN apt-get update && apt-get install -y netcat libnss3 curl fuse && \
    rm -rf /var/cache/apt/* && \
    mkdir -p /root/.juicefs && \
    ln -s /usr/local/bin/python /usr/bin/python && \
    chmod +x ${JUICEFS_CLI} && \
    /usr/bin/dfscli version

