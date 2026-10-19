//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"repodepot/internal/agent"
)

const serviceName = "RepoDepotAgent"

func serviceCommand(cmd, configPath string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("couldn't reach the service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()
	if cmd == "uninstall-service" {
		s, err := m.OpenService(serviceName)
		if err != nil {
			return err
		}
		defer s.Close()
		s.Control(svc.Stop)
		return s.Delete()
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.Abs(exe)
	if s, err := m.OpenService(serviceName); err == nil {
		s.Control(svc.Stop)
		s.Delete()
		s.Close()
		time.Sleep(2 * time.Second)
	}
	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName: "The Repo Depot Agent", StartType: mgr.StartAutomatic,
		Description: "Installs and updates software when The Repo Depot asks.",
	}, "-config", configPath)
	if err != nil {
		return err
	}
	defer s.Close()
	s.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}}, 86400)
	return s.Start()
}

type service struct{ configPath string }

func (s service) Execute(args []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	cfg, err := agent.LoadConfig(s.configPath)
	if err != nil {
		return true, 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.New(cfg).Run(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case <-done:
			cancel()
			return true, 2
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				return false, 0
			}
		}
	}
}

func runAsService(configPath string) bool {
	if ok, _ := svc.IsWindowsService(); !ok {
		return false
	}
	svc.Run(serviceName, service{configPath})
	return true
}
