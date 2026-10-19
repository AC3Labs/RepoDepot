//go:build windows

package agent

import (
	"context"
	"fmt"

	"golang.org/x/sys/windows"
)

func isAdmin() bool { return windows.GetCurrentProcessToken().IsElevated() }

// installService registers the app as a Windows service with sc.exe. The
// program has to be written as a service (most server software that ships
// for Windows is); otherwise Windows stops it after 30 seconds.
func installService(ctx context.Context, l *jobLog, name, exe, args, workDir string) error {
	bin := `"` + exe + `"`
	if args != "" {
		bin += " " + args
	}
	runCmd(ctx, l, workDir, nil, "sc.exe", "stop", name)
	runCmd(ctx, l, workDir, nil, "sc.exe", "delete", name)
	if err := runCmd(ctx, l, workDir, nil, "sc.exe", "create", name, "binPath=", bin, "start=", "auto", "DisplayName=", name+" (The Repo Depot)"); err != nil {
		return fmt.Errorf("couldn't create the service: %w", err)
	}
	return runCmd(ctx, l, workDir, nil, "sc.exe", "start", name)
}

func stopService(ctx context.Context, l *jobLog, name string) {
	runCmd(ctx, l, `C:\`, nil, "sc.exe", "stop", name)
}

func removeService(ctx context.Context, l *jobLog, name string) {
	runCmd(ctx, l, `C:\`, nil, "sc.exe", "stop", name)
	runCmd(ctx, l, `C:\`, nil, "sc.exe", "delete", name)
}
