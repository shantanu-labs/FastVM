//go:build !linux
// +build !linux

package main

import "github.com/sirupsen/logrus"

func mountInitFilesystems(log *logrus.Logger) {}

func startVsockServer(port int, log *logrus.Logger) {}
