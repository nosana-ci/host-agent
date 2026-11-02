// Package service is the agent's systemd side: the two units and systemctl.
package service

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nosana-ci/host-agent/internal/config"
	"github.com/nosana-ci/host-agent/internal/store"
)

const (
	UnitDir    = "/etc/systemd/system"
	AgentUnit  = "nosana-host.service"
	PodmanUnit = "nosana-podman.service"
)

// Every path in the units is a fixed one from config, so plain formatting is safe.
var units = map[string]string{
	PodmanUnit: fmt.Sprintf(`[Unit]
Description=Nosana podman service
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=0

[Service]
Type=exec
# Containers survive restarts of this service.
KillMode=process
Delegate=yes
EnvironmentFile=-%s
ExecStartPre=/bin/mkdir -p %s
ExecStartPre=/bin/rm -f %s
ExecStart=%s --root %s --runroot %s system service --time=0 unix://%s
SyslogIdentifier=nosana-podman
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`, config.RuntimeEnv, config.PodmanSocketDir, config.PodmanSocket, config.RuntimePodman, config.PodmanRoot, config.PodmanRunRoot, config.PodmanSocket),
	AgentUnit: fmt.Sprintf(`[Unit]
Description=Nosana host agent
Wants=network-online.target nosana-podman.service
After=network-online.target nosana-podman.service
StartLimitIntervalSec=0

[Service]
Type=simple
ExecStart=%s daemon
SyslogIdentifier=nosana-host
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`, config.CurrentBinary),
}

// WriteUnits writes both unit files and reports whether any changed.
func WriteUnits() (bool, error) {
	changed := false
	for name, text := range units {
		path := filepath.Join(UnitDir, name)
		old, err := os.ReadFile(path)
		if err == nil && bytes.Equal(old, []byte(text)) {
			continue
		}
		if err := store.WriteFile(path, []byte(text), 0o644); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

func Systemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
