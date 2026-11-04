package daemon

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/store"
)

// rebootEvery is how often a wanted reboot asks whether the node may be interrupted now.
const rebootEvery = 30 * time.Second

// systemStatus is what the machine itself says: whether the distro asks for a reboot, and the
// reboot the owner asked for that waits for the node.
func (d *daemon) systemStatus() *api.SystemStatus {
	s := &api.SystemStatus{}
	for _, dir := range []string{"/run", "/var/run"} {
		if _, err := os.Stat(dir + "/reboot-required"); err != nil {
			continue
		}
		s.RebootRequired = true
		if raw, err := os.ReadFile(dir + "/reboot-required.pkgs"); err == nil && len(strings.Fields(string(raw))) > 0 {
			pkgs := strings.Fields(string(raw))
			s.RebootPackages = &pkgs
		}
		break
	}
	if raw, err := os.ReadFile("/proc/uptime"); err == nil {
		if up, err := strconv.ParseFloat(strings.Fields(string(raw))[0], 64); err == nil {
			booted := time.Now().Add(-time.Duration(up * float64(time.Second))).UTC()
			s.BootedAt = &booted
		}
	}
	s.RebootPending = d.st.State().RebootWanted
	return s
}

// rebootLoop carries out reboot-when-idle: once the node may be interrupted (see
// Supervisor.MayInterrupt: not in a job or a benchmark, at most 24 h in the queue), the machine
// reboots. The wish is kept in the state, so an agent restart or update picks it up again; the
// node starts again after the boot as it did before.
func (d *daemon) rebootLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if d.st.State().RebootWanted != nil && d.sup.MayInterrupt() {
			d.reboot(ctx)
		}
		sleep(ctx, rebootEvery)
	}
}

func (d *daemon) reboot(ctx context.Context) {
	slog.Info("rebooting: the node is idle")
	d.st.Info("node", "rebooting the machine now that the node is idle")
	_ = d.st.Update(func(s *store.State) { s.RebootWanted = nil })
	_ = d.sendEvents(ctx) // the console sees why the host goes away
	if out, err := exec.CommandContext(ctx, "systemctl", "reboot").CombinedOutput(); err != nil {
		slog.Error("reboot", "err", err, "out", strings.TrimSpace(string(out)))
		d.st.Error("node", "the reboot failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
}

// wantReboot records the owner's wish; the loop acts on it.
func (d *daemon) wantReboot() (string, error) {
	now := time.Now().UTC()
	if err := d.st.Update(func(s *store.State) { s.RebootWanted = &now }); err != nil {
		return "", err
	}
	if d.sup.MayInterrupt() {
		return "the node is idle: rebooting now", nil
	}
	return "the machine reboots once the node is between jobs; cancel-reboot takes it back", nil
}

func (d *daemon) cancelReboot() (string, error) {
	if d.st.State().RebootWanted == nil {
		return "no reboot was waiting", nil
	}
	return "the reboot is cancelled", d.st.Update(func(s *store.State) { s.RebootWanted = nil })
}
