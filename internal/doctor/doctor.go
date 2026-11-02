// Package doctor runs the host checks the console shows, and the three fixes the agent can apply
// itself. It also reads the disk the state directory lives on.
package doctor

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/bundle"
	"github.com/nosana-ci/host-agent/internal/config"
	"github.com/nosana-ci/host-agent/internal/podman"
	"github.com/nosana-ci/host-agent/internal/service"
)

// MinDriver is the oldest NVIDIA driver major version the node supports.
const MinDriver = 550

var netClient = &http.Client{Timeout: 5 * time.Second}

// Run executes every check. g is the GPU detection result, gpuErr why it failed if it did.
func Run(ctx context.Context, controlURL string, pc *podman.Client, g api.GpuStatus, gpuErr error) []api.Check {
	checks := []api.Check{
		checkOS(),
		checkCgroup(),
		checkDisk(),
		checkPodman(ctx, pc),
		checkFirewall(),
		checkReach(ctx, "control", "host-manager reachable", strings.TrimRight(controlURL, "/")+"/health", http.StatusOK),
		// The registry answers 401 to anonymous clients, which is what a working one does.
		checkReach(ctx, "registry", "Container registry reachable", "https://registry-1.docker.io/v2/", http.StatusUnauthorized),
	}
	checks = append(checks, gpuChecks(g, gpuErr)...)
	for i := range checks {
		if _, ok := fixes[checks[i].Id]; ok && checks[i].Status != api.Ok {
			fixable := true
			checks[i].Fixable = &fixable
		}
	}
	return checks
}

var fixes = map[string]func(ctx context.Context) error{
	"podman":       func(context.Context) error { return service.Systemctl("restart", service.PodmanUnit) },
	"firewall":     bundle.EnsureFirewallTool,
	"persistenced": func(context.Context) error { return service.Systemctl("enable", "--now", "nvidia-persistenced") },
}

// Fix applies the fix of one check.
func Fix(ctx context.Context, id string) error {
	fix, ok := fixes[id]
	if !ok {
		return fmt.Errorf("no automatic fix for check %q", id)
	}
	return fix(ctx)
}

func check(id, title string, status api.CheckStatus, detail string) api.Check {
	c := api.Check{Id: id, Title: title, Status: status}
	if detail != "" {
		c.Detail = &detail
	}
	return c
}

func withFix(c api.Check, fix string) api.Check {
	c.Fix = &fix
	return c
}

var supportedOS = map[string]bool{"ubuntu 22.04": true, "ubuntu 24.04": true, "ubuntu 26.04": true, "debian 12": true, "debian 13": true}

func checkOS() api.Check {
	id, ver, pretty := osRelease()
	if runtime.GOARCH != "amd64" {
		return withFix(check("os", "Operating system", api.Fail, pretty+" on "+runtime.GOARCH), "Only x86-64 hosts are supported.")
	}
	if supportedOS[id+" "+ver] {
		return check("os", "Operating system", api.Ok, pretty)
	}
	return withFix(check("os", "Operating system", api.Warn, pretty), "Untested; Ubuntu 22.04, 24.04, 26.04 and Debian 12, 13 are supported.")
}

func osRelease() (id, ver, pretty string) {
	raw, _ := os.ReadFile("/etc/os-release")
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, _ := strings.Cut(line, "=")
		v = strings.Trim(v, `"`)
		switch k {
		case "ID":
			id = v
		case "VERSION_ID":
			ver = v
		case "PRETTY_NAME":
			pretty = v
		}
	}
	return id, ver, pretty
}

func checkCgroup() api.Check {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return withFix(check("cgroup", "cgroup v2", api.Fail, "cgroup v1 or hybrid mode"), "Boot with systemd.unified_cgroup_hierarchy=1.")
	}
	return check("cgroup", "cgroup v2", api.Ok, "")
}

func checkDisk() api.Check {
	d := Disk()
	detail := fmt.Sprintf("%.0f GB free on %s", d.FreeGb, d.Mount)
	switch {
	case d.FreeGb < 20:
		return withFix(check("disk", "Free disk space", api.Fail, detail), "At least 20 GB is needed; 100 GB or more is recommended for model images.")
	case d.FreeGb < 100:
		return withFix(check("disk", "Free disk space", api.Warn, detail), "100 GB or more is recommended for model images.")
	}
	return check("disk", "Free disk space", api.Ok, detail)
}

// Disk is the filesystem holding the state directory (podman's storage lives there).
func Disk() api.DiskStatus {
	d := api.DiskStatus{Mount: config.StateDir}
	var s syscall.Statfs_t
	if err := syscall.Statfs(config.StateDir, &s); err == nil {
		d.FreeGb = float32(s.Bavail) * float32(s.Bsize) / 1e9
		d.TotalGb = float32(s.Blocks) * float32(s.Bsize) / 1e9
	}
	return d
}

func checkPodman(ctx context.Context, pc *podman.Client) api.Check {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	info, err := pc.Info(ctx)
	if err != nil {
		return withFix(check("podman", "Podman service", api.Fail, err.Error()), "Restart the nosana-podman service (the agent can do this).")
	}
	return check("podman", "Podman service", api.Ok, "podman "+info.Version.Version+" ("+bundle.Current()+")")
}

func checkFirewall() api.Check {
	if fw := bundle.FirewallDriver(); fw != "" {
		return check("firewall", "Container networking (nftables or iptables)", api.Ok, fw)
	}
	return withFix(check("firewall", "Container networking (nftables or iptables)", api.Fail, "neither nft nor iptables is installed"), "Install nftables (the agent can do this).")
}

func checkReach(ctx context.Context, id, title, url string, expect int) api.Check {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	var resp *http.Response
	if err == nil {
		resp, err = netClient.Do(req)
	}
	if err != nil {
		return withFix(check(id, title, api.Fail, err.Error()), "Check DNS, firewall and outbound HTTPS.")
	}
	resp.Body.Close()
	if resp.StatusCode != expect {
		return withFix(check(id, title, api.Fail, fmt.Sprintf("%s answered %s", url, resp.Status)), "Something answers at this address, but not the expected service.")
	}
	return check(id, title, api.Ok, url)
}

var cdiSpecs = []string{"/etc/cdi/nvidia.yaml", "/var/run/cdi/nvidia.yaml", "/etc/cdi/nvidia.json"}

func gpuChecks(g api.GpuStatus, gpuErr error) []api.Check {
	if gpuErr != nil {
		return []api.Check{withFix(check("gpu", "NVIDIA GPU", api.Fail, gpuErr.Error()), "The driver did not answer. Reboot after a driver update, or reinstall the NVIDIA driver.")}
	}
	if !g.Detected {
		return []api.Check{withFix(check("gpu", "NVIDIA GPU", api.Warn, "no GPU detected"), "The node runs, but it will not pass benchmarks or join a market without an NVIDIA GPU.")}
	}
	names := make([]string, 0, len(g.Gpus))
	for _, x := range g.Gpus {
		names = append(names, fmt.Sprintf("%s (%d MB)", x.Name, x.MemoryMb))
	}
	driver := ""
	if g.Driver != nil {
		driver = *g.Driver
	}
	checks := []api.Check{check("gpu", "NVIDIA GPU", api.Ok, strings.Join(names, ", "))}
	title := fmt.Sprintf("NVIDIA driver %d or newer", MinDriver)
	if major, _ := strconv.Atoi(regexp.MustCompile(`^\d+`).FindString(driver)); major >= MinDriver {
		checks = append(checks, check("driver", title, api.Ok, driver))
	} else {
		checks = append(checks, withFix(check("driver", title, api.Fail, driver), fmt.Sprintf("Upgrade the NVIDIA driver to R%d or newer, then reboot.", MinDriver)))
	}
	if _, err := exec.LookPath("nvidia-ctk"); err == nil {
		checks = append(checks, check("ctk", "NVIDIA Container Toolkit", api.Ok, ""))
	} else {
		checks = append(checks, withFix(check("ctk", "NVIDIA Container Toolkit", api.Fail, "nvidia-ctk is missing"), "Install nvidia-container-toolkit from NVIDIA's apt repository."))
	}
	cdi := withFix(check("cdi", "CDI spec for GPUs", api.Fail, "no CDI spec found"), "Run: sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml")
	for _, p := range cdiSpecs {
		if _, err := os.Stat(p); err == nil {
			cdi = check("cdi", "CDI spec for GPUs", api.Ok, p)
			break
		}
	}
	checks = append(checks, cdi)
	if _, err := os.Stat("/run/nvidia-persistenced"); err == nil {
		checks = append(checks, check("persistenced", "nvidia-persistenced", api.Ok, ""))
	} else {
		checks = append(checks, withFix(check("persistenced", "nvidia-persistenced", api.Warn, "not running"), "Enable it: sudo systemctl enable --now nvidia-persistenced (the agent can do this)."))
	}
	return checks
}
