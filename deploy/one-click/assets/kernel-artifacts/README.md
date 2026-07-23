请把固定 kernel 制品放到本目录，默认文件名如下：

- `vmlinux`
- `vmlinux-pvm`（可选，PVM guest kernel）

guest image 默认在构建 one-click 发布包时基于 `deploy/guest-image/Dockerfile` 本地生成。
也可通过 `ONE_CLICK_GUEST_IMAGE_TAR` 指向已有的 `cube-guest-image-*.tar.gz`
（与 Release / docker 资产同布局）直接复用，跳过本地重建。

如需覆盖 kernel 默认路径，可以通过环境变量指定：

- `ONE_CLICK_CUBE_KERNEL_VMLINUX`
- `ONE_CLICK_CUBE_KERNEL_PVM_VMLINUX`
- `ONE_CLICK_GUEST_IMAGE_TAR`
