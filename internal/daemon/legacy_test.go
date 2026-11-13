package daemon

import (
	"errors"
	"testing"

	"github.com/nosana-ci/host-agent/internal/node"
)

func TestNextStep(t *testing.T) {
	answers := func(state string) *node.Info { return &node.Info{State: state} }
	silent := errors.New("answered 503")
	for _, tc := range []struct {
		name     string
		inDocker bool
		drained  bool
		info     *node.Info
		err      error
		want     step
	}{
		{"queued", true, false, answers("QUEUED"), nil, stopOld},
		{"starting", true, false, answers("OTHER"), nil, stopOld},
		{"in a job", true, false, answers("RUNNING_JOB"), nil, waitJob},
		{"benchmarking", true, false, answers("BENCHMARKING"), nil, waitJob},
		{"silent", true, false, nil, silent, waitUnknown},
		{"stopped by itself after its job", true, true, nil, silent, stopOld},
		{"tunnel lingers, or the key runs elsewhere", false, false, answers("QUEUED"), nil, waitElsewhere},
		{"gone", false, false, nil, silent, startNode},
	} {
		if got := nextStep(tc.inDocker, tc.drained, tc.info, tc.err); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}
