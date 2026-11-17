# Prebuilt guest image assets

Place a pinned `cube-guest-image-<arch>.tar.gz` here (same layout as the
`guest-image-*` Release asset: `cube-guest-image-cpu.img` + `version`).

Download it with the tag pinned in `deploy/release-assets.yaml`:

```bash
tag="$(./scripts/read-release-assets.sh | sed -n 's/^GUEST_IMAGE_RELEASE_TAG=//p')"
curl -fL -o "deploy/one-click/assets/guest-image/cube-guest-image-amd64.tar.gz" \
  "https://cnb.cool/CubeSandbox/CubeSandbox/-/releases/download/${tag}/cube-guest-image-amd64.tar.gz"
```

The one-click build only reuses this archive when pointed at it, which skips the
local Docker/mkfs rebuild:

```bash
export ONE_CLICK_GUEST_IMAGE_TAR="$PWD/deploy/one-click/assets/guest-image/cube-guest-image-amd64.tar.gz"
```

Otherwise `build-guest-image.sh` regenerates the image from
`deploy/guest-image/Dockerfile`.
