//go:build !windows

package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
)

func isAdmin() bool { return os.Geteuid() == 0 }

func unitPath(name string) string { return "/etc/systemd/system/" + name + ".service" }

// installService writes a systemd unit for the app and (re)starts it.
func installService(ctx context.Context, l *jobLog, name, exe, args, workDir string) error {
	if !hasCmd("systemctl") {
		return fmt.Errorf("running as a service needs systemd, which this machine doesn't have")
	}
	unit := fmt.Sprintf(`[Unit]
Description=%s (installed by The Repo Depot)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%s
ExecStart=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`, name, workDir, strings.TrimSpace(quoteArg(exe)+" "+args))
	if err := os.WriteFile(unitPath(name), []byte(unit), 0o644); err != nil {
		return fmt.Errorf("couldn't write the service file: %w", err)
	}
	l.Printf("Wrote %s", unitPath(name))
	if err := runCmd(ctx, l, "/", nil, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := runCmd(ctx, l, "/", nil, "systemctl", "enable", name); err != nil {
		return err
	}
	return runCmd(ctx, l, "/", nil, "systemctl", "restart", name)
}

func quoteArg(s string) string {
	if strings.ContainsAny(s, " \t\"") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

func stopService(ctx context.Context, l *jobLog, name string) {
	runCmd(ctx, l, "/", nil, "systemctl", "stop", name)
}

func removeService(ctx context.Context, l *jobLog, name string) {
	runCmd(ctx, l, "/", nil, "systemctl", "disable", "--now", name)
	if os.Remove(unitPath(name)) == nil {
		l.Printf("Removed %s", unitPath(name))
	}
	runCmd(ctx, l, "/", nil, "systemctl", "daemon-reload")
}
