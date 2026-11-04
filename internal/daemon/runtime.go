package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/node"
	"github.com/nosana-ci/host-agent/internal/podman"
	"github.com/nosana-ci/host-agent/internal/service"
)

const (
	storageEvery = time.Minute
	// storageListMax caps the images and containers the heartbeat lists.
	storageListMax = 20
	gb             = 1 << 30
)

// storage samples what podman's storage holds every minute and after a change.
type storage struct {
	pc   *podman.Client
	poke chan struct{}

	mu   sync.Mutex
	last *api.RuntimeStorage
}

func newStorage(pc *podman.Client) *storage {
	return &storage{pc: pc, poke: make(chan struct{}, 1)}
}

func (s *storage) run(ctx context.Context) {
	for ctx.Err() == nil {
		s.refresh(ctx)
		select {
		case <-ctx.Done():
		case <-s.poke:
		case <-time.After(storageEvery):
		}
	}
}

// resample reads the storage again soon, after a prune, a reset or a runtime restart.
func (s *storage) resample() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

func (s *storage) status() *api.RuntimeStorage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *storage) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	df, err := s.pc.DiskUsage(ctx)
	if err != nil {
		return
	}
	st := api.RuntimeStorage{ReadAt: time.Now().UTC(), Images: []api.RuntimeImage{}, Containers: []api.RuntimeContainer{}, Volumes: len(df.Volumes)}
	var used, reclaimable int64
	for _, i := range df.Images {
		used += i.UniqueSize
		if i.Containers == 0 {
			reclaimable += i.UniqueSize
		}
		if len(st.Images) < storageListMax {
			ref := strings.TrimPrefix(i.Repository, "docker.io/") + ":" + i.Tag
			if i.Repository == "" || i.Tag == "" || i.Tag == "<none>" {
				ref = podman.ShortID(i.ImageID)
			}
			st.Images = append(st.Images, api.RuntimeImage{Ref: ref, SizeGb: float64(i.UniqueSize) / gb, InUse: i.Containers > 0})
		}
	}
	for _, c := range df.Containers {
		used += c.RWSize
		if c.Names != node.ContainerName && !strings.HasPrefix(c.Status, "running") {
			reclaimable += c.RWSize
		}
		if len(st.Containers) < storageListMax {
			st.Containers = append(st.Containers, api.RuntimeContainer{Name: c.Names, Image: strings.TrimPrefix(c.Image, "docker.io/"), State: strings.Fields(c.Status + " ")[0]})
		}
	}
	for _, v := range df.Volumes {
		used += v.Size
	}
	st.UsedGb, st.ReclaimableGb = float64(used)/gb, float64(reclaimable)/gb
	s.mu.Lock()
	s.last = &st
	s.mu.Unlock()
}

// prune removes the containers that are not running (the node's excepted) and the images nothing
// uses; the node image the release names stays for the next start.
func (d *daemon) prune(ctx context.Context) (string, error) {
	ctrs, err := d.pc.Containers(ctx)
	if err != nil {
		return "", err
	}
	var errs []string
	removedCtrs, removedImgs := 0, 0
	for _, c := range ctrs {
		if c.Name() == node.ContainerName || c.State == "running" {
			continue
		}
		if err := d.pc.RemoveContainer(ctx, c.ID); err != nil {
			errs = append(errs, err.Error())
		} else {
			removedCtrs++
		}
	}
	images, err := d.pc.Images(ctx)
	if err != nil {
		return "", err
	}
	keep := "docker.io/" + strings.TrimPrefix(d.sup.Image(), "docker.io/")
	for _, i := range images {
		if i.Containers > 0 || i.Ref() == strings.TrimPrefix(keep, "docker.io/") {
			continue
		}
		if err := d.pc.RemoveImage(ctx, i.ID); err != nil {
			errs = append(errs, err.Error())
		} else {
			removedImgs++
		}
	}
	d.storage.resample()
	msg := fmt.Sprintf("removed %d container%s and %d image%s", removedCtrs, plural(removedCtrs), removedImgs, plural(removedImgs))
	if len(errs) > 0 {
		return "", errors.New(msg + "; not everything could go: " + strings.Join(errs, "; "))
	}
	d.st.Info("runtime", "pruned the container storage: %s", msg)
	return msg, nil
}

// resetStorage stops the node, wipes every container, image and volume, and starts the node again,
// which pulls its image anew. The node's wallet and state live outside podman's storage.
func (d *daemon) resetStorage(ctx context.Context) (string, error) {
	if err := d.sup.Stop(); err != nil {
		return "", err
	}
	deadline := time.Now().Add(3 * time.Minute)
	for d.sup.Status().Container == "running" {
		if time.Now().After(deadline) {
			return "", errors.New("the node did not stop in time; nothing was removed")
		}
		sleep(ctx, 2*time.Second)
	}
	wipe := func() error {
		ctrs, err := d.pc.Containers(ctx)
		if err != nil {
			return err
		}
		for _, c := range ctrs {
			if err := d.pc.RemoveContainer(ctx, c.ID); err != nil {
				return fmt.Errorf("remove %s: %w", c.Name(), err)
			}
		}
		images, err := d.pc.Images(ctx)
		if err != nil {
			return err
		}
		for _, i := range images {
			if err := d.pc.RemoveImage(ctx, i.ID); err != nil {
				return fmt.Errorf("remove image %s: %w", i.Ref(), err)
			}
		}
		vols, err := d.pc.Volumes(ctx)
		if err != nil {
			return err
		}
		for _, v := range vols {
			if err := d.pc.RemoveVolume(ctx, v.Name); err != nil {
				return fmt.Errorf("remove volume %s: %w", v.Name, err)
			}
		}
		return nil
	}
	err := wipe()
	d.storage.resample()
	if startErr := d.sup.Start(); startErr != nil && err == nil {
		err = startErr
	}
	if err != nil {
		return "", err
	}
	d.st.Warn("runtime", "the container storage was reset; the node pulls its image again")
	return "the container storage is empty; the node pulls its image again and starts", nil
}

// restartRuntime restarts the podman service; the containers survive it (KillMode=process) and
// the node is not touched.
func (d *daemon) restartRuntime(ctx context.Context) (string, error) {
	if err := service.Systemctl("restart", service.PodmanUnit); err != nil {
		return "", err
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := d.pc.Info(ctx); err == nil {
			d.storage.resample()
			d.st.Info("runtime", "podman restarted")
			return "podman restarted", nil
		}
		sleep(ctx, 2*time.Second)
	}
	return "", errors.New("podman did not answer within 45 s of its restart")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
