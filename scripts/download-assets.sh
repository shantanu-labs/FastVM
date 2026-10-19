#!/usr/bin/env bash
set -euo pipefail

DEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/assets"
mkdir -p "${DEST_DIR}"

echo "==> Downloading test kernel (vmlinux) and rootfs into ${DEST_DIR}..."

ARCH="$(uname -m)"
if [ "${ARCH}" != "x86_64" ] && [ "${ARCH}" != "aarch64" ]; then
    ARCH="x86_64"
fi

# Download Firecracker CI pre-built vmlinux 5.10 kernel
KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.7/${ARCH}/vmlinux-5.10.209"
echo "Fetching kernel: ${KERNEL_URL}..."
curl -fsSL "${KERNEL_URL}" -o "${DEST_DIR}/vmlinux"

# Download minimal Ubuntu/Alpine rootfs
ROOTFS_URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.7/${ARCH}/ubuntu-22.04.ext4"
echo "Fetching rootfs: ${ROOTFS_URL}..."
curl -fsSL "${ROOTFS_URL}" -o "${DEST_DIR}/rootfs.ext4"

echo "✓ Assets downloaded successfully to ${DEST_DIR}"
ls -lh "${DEST_DIR}"
