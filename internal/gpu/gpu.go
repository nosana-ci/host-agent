// Package gpu detects NVIDIA GPUs through nvidia-smi.
package gpu

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/nosana-ci/nosana-host/internal/api"
)

func Detect(ctx context.Context) api.GpuStatus {
	st := api.GpuStatus{Gpus: []api.Gpu{}}
	smi, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return st
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, smi, "--query-gpu=index,name,memory.total,driver_version,uuid", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return st
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
	return st
}
