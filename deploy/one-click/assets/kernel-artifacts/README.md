请把固定 kernel 制品放到本目录，默认文件名如下：

- `vmlinux`
- `vmlinux-pvm`（可选，PVM guest kernel）

guest image 会在构建 one-click 发布包时，基于 `deploy/guest-image/Dockerfile` 在本地动态构建，不再依赖预制 zip。

如需覆盖 kernel 默认路径，可以通过环境变量指定：

- `ONE_CLICK_CUBE_KERNEL_VMLINUX`
- `ONE_CLICK_CUBE_KERNEL_PVM_VMLINUX`
