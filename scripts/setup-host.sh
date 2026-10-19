#!/usr/bin/env bash
set -euo pipefail

echo "==> Setting up Linux host for Airbox Firecracker MicroVMs..."

# 1. Check KVM hardware virtualization
if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
    echo "✓ KVM hardware virtualization is accessible at /dev/kvm"
else
    echo "✗ ERROR: /dev/kvm is not accessible. Please ensure hardware virtualization is enabled"
    echo "  Run: sudo usermod -aG kvm \$USER and reload session"
    exit 1
fi

# 2. Enable IPv4 forwarding
echo "==> Enabling IPv4 forwarding..."
sudo sysctl -w net.ipv4.ip_forward=1

# 3. Download and install Firecracker binary
FIRECRACKER_VERSION="v1.7.0"
ARCH="$(uname -m)"
echo "==> Installing Firecracker ${FIRECRACKER_VERSION} (${ARCH})..."

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

cd "${TMP_DIR}"
wget -q "https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/firecracker-${FIRECRACKER_VERSION}-${ARCH}.tgz"
tar -xzf "firecracker-${FIRECRACKER_VERSION}-${ARCH}.tgz"

sudo mv "release-${FIRECRACKER_VERSION}-${ARCH}/firecracker-${FIRECRACKER_VERSION}-${ARCH}" /usr/local/bin/firecracker
sudo chmod +x /usr/local/bin/firecracker

echo "✓ Firecracker installed successfully: $(/usr/local/bin/firecracker --version | head -n 1)"

# 4. Create directories
echo "==> Creating local asset and overlay directories..."
mkdir -p assets
sudo mkdir -p /var/lib/airbox/overlays

echo "==> Host setup complete. You can now run './scripts/download-assets.sh' to fetch kernel & rootfs."
