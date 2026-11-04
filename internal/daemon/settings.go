package daemon

import (
	"fmt"
	"strings"

	"github.com/nosana-ci/host-agent/internal/config"
	"github.com/nosana-ci/host-agent/internal/local"
)

// The settings the owner may change while the agent runs, by their name on the command line and
// in the console's set-config command.
const settingAutoUpdate = "auto-update"

func (d *daemon) autoUpdate() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.auto
}

func (d *daemon) configView() local.Config {
	return local.Config{ControlURL: d.cfg.ControlURL, ReleaseURL: d.cfg.ReleaseURL, Network: d.cfg.Network, AutoUpdate: d.autoUpdate()}
}

// setConfig changes one setting, in the config file and in this process, and says what it means.
func (d *daemon) setConfig(key, value string) (string, error) {
	if key != settingAutoUpdate {
		return "", fmt.Errorf("unknown setting %q; the settings are: %s", key, settingAutoUpdate)
	}
	on, err := parseBool(value)
	if err != nil {
		return "", err
	}
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	cfg.AutoUpdate = &on
	if err := config.Save(cfg); err != nil {
		return "", err
	}
	d.mu.Lock()
	d.auto = on
	d.mu.Unlock()
	if on {
		return "auto-update on: new releases are installed as they come", nil
	}
	return "auto-update off: new releases are reported, and installed only with nosana-host update or from the console", nil
}

func parseBool(value string) (bool, error) {
	switch strings.ToLower(value) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("%q is not on or off", value)
}
