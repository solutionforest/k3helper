package tui

import (
	"strconv"
	"strings"
)

// hostSample is one moment's load on a node, as percentages.
type hostSample struct {
	CPU float64
	Mem float64
	// OK is false when the probe could not be read. A failed probe must not
	// enter the history as 0%: an unreadable /proc/meminfo would otherwise
	// draw a flat healthy line for a node under memory pressure.
	OK bool
}

// sampleHost reads load and memory from /proc in one round trip.
//
// /proc rather than `kubectl top`: metrics-server is optional on k3s and
// absent on a plain kubeadm cluster, and the dashboard already holds an SSH
// connection to every node — including nodes the API server cannot see, which
// are the ones worth graphing when something is wrong.
func sampleHost(exec interface {
	Run(cmd string) (string, int, error)
}) hostSample {
	out, code, err := exec.Run(`cat /proc/loadavg; nproc; cat /proc/meminfo`)
	if err != nil || code != 0 {
		return hostSample{}
	}
	return parseHostSample(out)
}

// parseHostSample turns the concatenated probe output into percentages.
func parseHostSample(out string) hostSample {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 3 {
		return hostSample{}
	}
	s := hostSample{}

	// Line 1: "0.52 0.58 0.59 1/523 12345"
	fields := strings.Fields(lines[0])
	if len(fields) == 0 {
		return hostSample{}
	}
	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return hostSample{}
	}
	// Line 2: nproc
	cores, err := strconv.Atoi(strings.TrimSpace(lines[1]))
	if err != nil || cores <= 0 {
		cores = 1
	}
	s.CPU = load1 / float64(cores) * 100

	// Remaining lines: /proc/meminfo. MemAvailable is what the kernel says can
	// be handed out without swapping — MemFree alone counts page cache as used
	// and would report a healthy host at 95%.
	var total, available float64
	var haveTotal, haveAvail bool
	for _, line := range lines[2:] {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		kb := strings.Fields(value)
		if len(kb) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(kb[0], 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			total, haveTotal = v, true
		case "MemAvailable":
			available, haveAvail = v, true
		}
	}
	if !haveTotal || !haveAvail || total <= 0 {
		return hostSample{}
	}
	s.Mem = (total - available) / total * 100
	s.OK = true
	return s
}

// historyLen is how many samples each node keeps: at the 5s refresh interval
// that is the last two minutes, which is enough to see a spike arrive.
const historyLen = 24

// history is a fixed-size ring of samples per node.
type history struct {
	cpu map[string][]float64
	mem map[string][]float64
}

func newHistory() *history {
	return &history{cpu: map[string][]float64{}, mem: map[string][]float64{}}
}

// push records one sample. A failed probe is dropped rather than stored, so
// the graph shows the last known real values instead of a fabricated zero.
func (h *history) push(node string, s hostSample) {
	if !s.OK {
		return
	}
	h.cpu[node] = appendCapped(h.cpu[node], s.CPU)
	h.mem[node] = appendCapped(h.mem[node], s.Mem)
}

func appendCapped(xs []float64, v float64) []float64 {
	xs = append(xs, v)
	if len(xs) > historyLen {
		xs = xs[len(xs)-historyLen:]
	}
	return xs
}

func (h *history) cpuFor(node string) []float64 { return h.cpu[node] }
func (h *history) memFor(node string) []float64 { return h.mem[node] }

// latest returns the most recent value, and whether there is one.
func latest(xs []float64) (float64, bool) {
	if len(xs) == 0 {
		return 0, false
	}
	return xs[len(xs)-1], true
}
