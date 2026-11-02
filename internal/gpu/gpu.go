// Package gpu detects NVIDIA GPUs through nvidia-smi.
package gpu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
)

// Detect lists the GPUs. The error is set when nvidia-smi exists but fails (a driver that did
// not load, a library mismatch after an upgrade): that is not "no GPU".
func Detect(ctx context.Context) (api.GpuStatus, error) {
	st := api.GpuStatus{Gpus: []api.Gpu{}}
	smi, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return st, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, smi, "--query-gpu=index,name,memory.total,driver_version,uuid", "--format=csv,noheader,nounits").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(bytes.TrimSpace(exit.Stderr)) > 0 {
			return st, fmt.Errorf("nvidia-smi: %s", bytes.TrimSpace(exit.Stderr))
		}
		return st, fmt.Errorf("nvidia-smi: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 5 {
			continue
		}
		index, _ := strconv.Atoi(strings.TrimSpace(f[0]))
		mem, _ := strconv.Atoi(strings.TrimSpace(f[2]))
		driver, uuid := strings.TrimSpace(f[3]), strings.TrimSpace(f[4])
		st.Gpus = append(st.Gpus, api.Gpu{Index: index, Name: strings.TrimSpace(f[1]), MemoryMb: mem, Uuid: &uuid})
		st.Driver = &driver
	}
	st.Detected = len(st.Gpus) > 0
	return st, nil
}
