package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shantanuroy/airbox/pkg/api"
	"github.com/shantanuroy/airbox/pkg/sandbox"
	"github.com/sirupsen/logrus"
)

func main() {
	var (
		port        string
		kernelPath  string
		rootfsPath  string
		overlayDir  string
		tapPoolSize int
		logLevel    string
	)

	flag.StringVar(&port, "port", "8080", "Port for the Airbox API daemon to listen on")
	flag.StringVar(&kernelPath, "kernel", "assets/vmlinux", "Path to uncompressed vmlinux kernel")
	flag.StringVar(&rootfsPath, "rootfs", "assets/rootfs.ext4", "Path to base rootfs ext4 image")
	flag.StringVar(&overlayDir, "overlay-dir", "", "Directory for ephemeral tmpfs/ext4 overlays")
	flag.IntVar(&tapPoolSize, "tap-pool-size", 100, "Number of pre-warmed TAP network devices")
	flag.StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	flag.Parse()

	log := logrus.New()
	log.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05.000",
	})

	lvl, err := logrus.ParseLevel(logLevel)
	if err == nil {
		log.SetLevel(lvl)
	}

	log.Infof("Initializing Airbox Sub-100ms Sandbox Daemon (Kernel: %s, Rootfs: %s)", kernelPath, rootfsPath)

	// Initialize Sandbox Manager (Hypervisor driver, TAP pool, Overlay storage)
	cfg := sandbox.Config{
		KernelPath: kernelPath,
		RootfsPath: rootfsPath,
		OverlayDir: overlayDir,
		PoolSize:   tapPoolSize,
	}

	mgr, err := sandbox.NewManager(cfg, log)
	if err != nil {
		log.Fatalf("Failed to initialize sandbox manager: %v", err)
	}
	defer mgr.Close()

	log.Infof("Sandbox Manager ready using driver [%s], KVM ready: %v, TAP pool: %d",
		mgr.HypervisorName(), mgr.KvmAvailable(), mgr.AvailableTaps())

	// Initialize API Server
	apiServer := api.NewServer(mgr, log)
	router := apiServer.Routes()

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: router,
	}

	// Start HTTP Server in background goroutine
	go func() {
		log.Infof("Airbox daemon listening on http://0.0.0.0:%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server failure: %v", err)
		}
	}()

	// Graceful shutdown on OS signals (Ctrl+C, SIGTERM)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("Shutting down Airbox daemon gracefully...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Errorf("Server forced shutdown: %v", err)
	}

	log.Info("Airbox daemon exited successfully")
}
