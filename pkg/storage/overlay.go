package storage

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// OverlayManager manages ephemeral copy-on-write root filesystem overlays.
type OverlayManager struct {
	baseRootfsPath string
	overlayDir     string
	mu             sync.Mutex
	activeOverlays map[string]string // sandboxID -> overlayFilePath
}

// NewOverlayManager creates a new OverlayManager.
func NewOverlayManager(baseRootfsPath string, overlayDir string) (*OverlayManager, error) {
	if overlayDir == "" {
		if st, err := os.Stat("/dev/shm"); err == nil && st.IsDir() {
			overlayDir = "/dev/shm/airbox-overlays"
		} else {
			overlayDir = filepath.Join(os.TempDir(), "airbox-overlays")
		}
	}

	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create overlay directory %s: %w", overlayDir, err)
	}

	return &OverlayManager{
		baseRootfsPath: baseRootfsPath,
		overlayDir:     overlayDir,
		activeOverlays: make(map[string]string),
	}, nil
}

// CreateOverlay registers a fast ephemeral overlay path for a sandbox instance.
func (m *OverlayManager) CreateOverlay(sandboxID string) (string, error) {
	m.mu.Lock()
	overlayPath := filepath.Join(m.overlayDir, fmt.Sprintf("rootfs-%s.ext4", sandboxID))
	m.activeOverlays[sandboxID] = overlayPath
	m.mu.Unlock()
	return overlayPath, nil
}

// MaterializeOverlay copies the base rootfs using sparse/reflink copy when a cold VM boots.
func MaterializeOverlay(baseRootfsPath, overlayPath string) error {
	if err := os.MkdirAll(filepath.Dir(overlayPath), 0755); err != nil {
		return err
	}
	cmd := exec.Command("cp", "--sparse=always", "--reflink=auto", baseRootfsPath, overlayPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to clone rootfs %s -> %s: %v (%s)", baseRootfsPath, overlayPath, err, string(out))
	}
	return nil
}

// RemoveOverlay deletes the temporary overlay file for the given sandbox.
func (m *OverlayManager) RemoveOverlay(sandboxID string) error {
	m.mu.Lock()
	overlayPath, exists := m.activeOverlays[sandboxID]
	if !exists {
		overlayPath = filepath.Join(m.overlayDir, fmt.Sprintf("rootfs-%s.ext4", sandboxID))
	}
	delete(m.activeOverlays, sandboxID)
	m.mu.Unlock()

	if err := os.Remove(overlayPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove overlay %s: %w", overlayPath, err)
	}
	return nil
}

// CleanupAll removes all tracked overlay files.
func (m *OverlayManager) CleanupAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, path := range m.activeOverlays {
		_ = os.Remove(path)
	}
	m.activeOverlays = make(map[string]string)
}
