# dfs-csi使用说明
## 基础镜像编译
### 依赖文件分布说明
* 工程目录`dfs-csi/bin_dependency`为**外部依赖**存放目录
* 进入安装完毕dfs的任一节点，在如下目录: `/var/lib/dfs/etc/dfs/console/dfs-console-1/jfs-static`具有客户端工具**juicefs**
* 将客户端工具**juicefs**替换工程目录`dfs-csi/bin_dependency`下的juicefs文件，则可达到**升级客户端**的目的
* **librados.so.2**,**libceph-common.so.0**取自MOS产品包，通常无需修改

### 底包编译说明
* 使用工程目录下的`dfs-csi/base.Dockerfile`可进行基础镜像(**dfs-base:v1**)编译
* 基础镜像**dfs-base:v1**用于编译csi镜像的底包，除非需要升级dfs客户端，否则一般无需修改底包内容
* 编译前需要解压`dfs-csi/bin_bependency/so.zip`,编译时依赖其中的**libceph-common.so.0**以及**librados.so.2**文件
* 执行命令编译底包:`docker build -f base.Dockerfile -t  dfs-base:v1 .`

### 工程编译说明
* 编译csi 二进制: `make juicefs-csi-driver`
* 编译csi镜像(**mosfs-csi-driver**): 执行`make mosfs-csi-image`
* 编译csi完整安装包: 执行`make package`， 注意：需要`dfs-csi/hack/build-package.sh`中指定镜像可以顺利被拉取