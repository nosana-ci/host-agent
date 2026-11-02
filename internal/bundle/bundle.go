// Package bundle installs the container runtime bundle: a relocatable podman with its helpers,
// so hosts never install podman themselves.
//
//	/var/lib/nosana/runtime/<version>/bin, lib/podman, etc, env   one unpacked bundle per version
//	/var/lib/nosana/runtime/current -> <version>                  the nosana-podman unit runs current/bin/podman
package bundle

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nosana-ci/host-agent/internal/config"
	"github.com/nosana-ci/host-agent/internal/manifest"
	"github.com/nosana-ci/host-agent/internal/store"
)

// Platform is this machine's key in a manifest's artifacts.
const Platform = "linux-amd64"

// Current is the active bundle version, or "".
func Current() string {
	target, err := os.Readlink(config.RuntimeCurrent)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// Install downloads the manifest's runtime bundle, unpacks it next to the other versions and
// writes its configuration. It does not activate it. Returns the version.
func Install(ctx context.Context, m *manifest.Manifest) (string, error) {
	art := m.Runtime.Artifacts.LinuxAmd64
	if art.Url == "" {
		return "", errors.New("the release has no runtime bundle for " + Platform)
	}
	ver := m.Runtime.Version
	dir := filepath.Join(config.RuntimeDir, ver)
	if _, err := os.Stat(filepath.Join(dir, "bin", "podman")); err == nil {
		return ver, configure(dir)
	}
	tmp := filepath.Join(config.RuntimeDir, "."+ver+".tar.gz")
	if err := art.Download(ctx, tmp, 0o600); err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp) }()
	stage := dir + ".unpack"
	_ = os.RemoveAll(stage)
	if err := untar(tmp, stage); err != nil {
		_ = os.RemoveAll(stage)
		return "", fmt.Errorf("unpack runtime: %w", err)
	}
	if err := smokeTest(ctx, stage); err != nil {
		_ = os.RemoveAll(stage)
		return "", err
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(stage, dir); err != nil {
		return "", err
	}
	return ver, configure(dir)
}

// Activate points runtime/current at version.
func Activate(version string) error { return store.Relink(config.RuntimeCurrent, version) }

// Prune removes every bundle but the active one and keep.
func Prune(keep string) {
	cur := Current()
	entries, _ := os.ReadDir(config.RuntimeDir)
	for _, e := range entries {
		if n := e.Name(); e.IsDir() && n != cur && n != keep {
			_ = os.RemoveAll(filepath.Join(config.RuntimeDir, n))
		}
	}
}

// FirewallDriver is the netavark firewall driver this host can run: the bundle cannot ship
// nft or iptables.
func FirewallDriver() string {
	if _, err := exec.LookPath("nft"); err == nil {
		return "nftables"
	}
	if _, err := exec.LookPath("iptables"); err == nil {
		return "iptables"
	}
	return ""
}

// EnsureFirewallTool installs nftables when neither nft nor iptables is present.
func EnsureFirewallTool(ctx context.Context) error {
	if FirewallDriver() != "" {
		return nil
	}
	if _, err := exec.LookPath("apt-get"); err != nil {
		return errors.New("container networking needs nftables or iptables; install one of them")
	}
	for _, args := range [][]string{{"update", "-qq"}, {"install", "-y", "-qq", "nftables"}} {
		cmd := exec.CommandContext(ctx, "apt-get", args...)
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("apt-get %s: %v: %s", args[0], err, lastLine(out))
		}
	}
	return nil
}

// EnsurePolicy writes podman's image policy where it looks for it (there is no setting for the
// path) when the host has none.
func EnsurePolicy() error {
	const path = "/etc/containers/policy.json"
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(config.RuntimeCurrent, "etc", "policy.json"))
	if err != nil {
		raw = []byte(`{"default":[{"type":"insecureAcceptAnything"}]}` + "\n")
	}
	return store.WriteFile(path, raw, 0o644)
}

// jobLogSizeMax caps every container log the node creates through this podman.
const jobLogSizeMax = 50 << 20

// configure writes the bundle's containers.conf, registries.conf and the env file the podman
// unit loads, with absolute paths, so the bundle never relies on /etc/containers.
func configure(dir string) error {
	final := filepath.Join(config.RuntimeDir, strings.TrimSuffix(filepath.Base(dir), ".unpack"))
	etc := filepath.Join(dir, "etc")
	firewall := ""
	if fw := FirewallDriver(); fw != "" {
		firewall = fmt.Sprintf("firewall_driver = %q\n", fw)
	}
	conf := fmt.Sprintf(`# Written by nosana-host for this runtime bundle.
[containers]
seccomp_profile = %q
# Job containers are created by the node without a log cap of their own.
log_driver = "k8s-file"
log_size_max = %d

[engine]
helper_binaries_dir = [%q, %q]
conmon_path = [%q]
cgroup_manager = "cgroupfs"
events_logger = "file"
# Own locks and tmp dir: podman's default lock table is shared by every podman on the host.
lock_type = "file"
tmp_dir = %q

[engine.runtimes]
crun = [%q]
runc = [%q]

[network]
network_backend = "netavark"
%s`, filepath.Join(final, "etc", "seccomp.json"), jobLogSizeMax,
		filepath.Join(final, "lib", "podman"), filepath.Join(final, "bin"),
		filepath.Join(final, "lib", "podman", "conmon"), config.PodmanTmpDir,
		filepath.Join(final, "bin", "crun"), filepath.Join(final, "bin", "runc"), firewall)
	if err := store.WriteFile(filepath.Join(etc, "containers.conf"), []byte(conf), 0o644); err != nil {
		return err
	}
	reg := "# Written by nosana-host.\nunqualified-search-registries = [\"docker.io\"]\nshort-name-mode = \"permissive\"\n"
	if err := store.WriteFile(filepath.Join(etc, "registries.conf"), []byte(reg), 0o644); err != nil {
		return err
	}
	env := fmt.Sprintf("CONTAINERS_CONF=%s\nCONTAINERS_REGISTRIES_CONF=%s\nPATH=%s:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n",
		filepath.Join(final, "etc", "containers.conf"), filepath.Join(final, "etc", "registries.conf"), filepath.Join(final, "bin"))
	return store.WriteFile(filepath.Join(dir, "env"), []byte(env), 0o644)
}

func smokeTest(ctx context.Context, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, filepath.Join(dir, "bin", "podman"), "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("the runtime bundle does not start: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func untar(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	root, _ := filepath.Abs(dst)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		p := filepath.Join(root, h.Name)
		if p != root && !strings.HasPrefix(p, root+string(os.PathSeparator)) {
			return fmt.Errorf("bad path in bundle: %s", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(h.Mode)&0o755|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(h.Linkname) || strings.Contains(h.Linkname, "..") {
				return fmt.Errorf("bad symlink in bundle: %s -> %s", h.Name, h.Linkname)
			}
			_ = os.Symlink(h.Linkname, p)
		}
	}
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}
