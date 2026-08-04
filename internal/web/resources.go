package web

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Choosing how much an app may use, by dragging rather than typing.
//
// "500m" is Kubernetes' grammar, not anybody's idea of an amount. A text field
// asking for it makes somebody learn that grammar before they can express a
// judgement they already have — half a core, a couple of gigabytes — and gives
// no sense of whether what they typed is a lot.
//
// A track with a real ceiling turns it into a comparison instead. The ceiling
// is the largest node's capacity, because that is what a single pod can
// actually land on: a limit above it is a workload that stays Pending forever,
// and offering it would be offering to break the app.

// BranchData is what the branch picker swaps in.
type BranchData struct {
	Branches []string

	// Error is why the remote could not be read, already reduced to something
	// short enough to sit under a field. A private repository, a typo and an
	// outage all land here and none of them is worth a red field.
	Error string

	// Searched distinguishes "nothing matched what you typed" from "we have
	// not looked yet", which render as different things and would otherwise
	// both be an empty list.
	Searched bool
}

// repoLabel is the owner and name, which is how people say a repository.
//
// https://github.com/codeblocktz/yacht.git is the machine's version of a name
// everybody writes as codeblocktz/yacht, and a card is the wrong place to make
// somebody read a URL to find two words.
func repoLabel(url string) string {
	trimmed := strings.TrimSuffix(strings.TrimSpace(url), ".git")
	trimmed = strings.TrimSuffix(trimmed, "/")

	parts := strings.Split(trimmed, "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	return trimmed
}

// sliderSteps is how many positions a track has.
//
// Coarse on purpose. Nobody wants 1/1000th of a core, and a slider that can
// express one is a slider nobody can land on a round number with.
const cpuStepMillis = 250

// memStepMiB keeps memory on quarter-gigabyte stops for the same reason.
const memStepMiB = 256

// ResourceSlider is one track: what it is set to now, and how far it can go.
type ResourceSlider struct {
	// Name is the form field, in the unit the slider works in — millicores or
	// mebibytes — rather than in Kubernetes' string grammar. The handler turns
	// the number back into a string, so nothing here has to parse one.
	Name string

	Label string

	// Value and Max are in the slider's own unit.
	Value int64
	Max   int64
	Step  int64

	// Display and MaxDisplay are the same numbers as people say them.
	Display    string
	MaxDisplay string

	// Unset means no limit is configured, which is a different answer from a
	// small one and is drawn differently.
	Unset bool
}

// cpuSlider builds the CPU track from a stored limit and the cluster's shape.
func cpuSlider(name, label, current string, capacityMillis int64) ResourceSlider {
	max := roundUp(capacityMillis, cpuStepMillis)
	if max <= 0 {
		// No cluster to ask, so offer something usable rather than a track
		// with one position on it. Four cores covers most single-box installs.
		max = 4000
	}
	value := parseCPUMillis(current)
	if value > max {
		// A limit set before a node was removed. Shown at the ceiling rather
		// than off the end of the track, where the handle would be invisible.
		max = roundUp(value, cpuStepMillis)
	}
	return ResourceSlider{
		Name: name, Label: label,
		Value: value, Max: max, Step: cpuStepMillis,
		Display: cpuDisplay(value), MaxDisplay: cpuDisplay(max),
		Unset: value == 0,
	}
}

// memSlider builds the memory track.
func memSlider(name, label, current string, capacityBytes int64) ResourceSlider {
	max := roundUp(capacityBytes/(1<<20), memStepMiB)
	if max <= 0 {
		max = 8192
	}
	value := parseMemMiB(current)
	if value > max {
		max = roundUp(value, memStepMiB)
	}
	return ResourceSlider{
		Name: name, Label: label,
		Value: value, Max: max, Step: memStepMiB,
		Display: memDisplay(value), MaxDisplay: memDisplay(max),
		Unset: value == 0,
	}
}

// schedulableCPU is the largest single node's CPU, which is the real ceiling.
//
// Not the cluster total: a pod runs on one node, so the sum of every node is a
// number no workload can ever reach. Offering it would be offering a limit
// that guarantees the app never schedules.
func schedulableCPU(nodes []orchestrator.NodeInfo) int64 {
	var max int64
	for _, n := range nodes {
		if n.CPUCapacityMillis > max {
			max = n.CPUCapacityMillis
		}
	}
	return max
}

// schedulableMemory is the same question for memory.
func schedulableMemory(nodes []orchestrator.NodeInfo) int64 {
	var max int64
	for _, n := range nodes {
		if n.MemCapacityBytes > max {
			max = n.MemCapacityBytes
		}
	}
	return max
}

// parseCPUMillis reads Kubernetes' CPU grammar into millicores.
//
// Two forms: "500m" is millicores, a bare number is whole cores. Anything else
// is treated as unset rather than guessed at — a value nothing here understands
// is one this control should not claim to represent.
func parseCPUMillis(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if rest, ok := strings.CutSuffix(s, "m"); ok {
		n, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	cores, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(cores * 1000)
}

// parseMemMiB reads Kubernetes' memory grammar into mebibytes.
func parseMemMiB(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	for _, u := range []struct {
		suffix string
		mib    int64
	}{
		{"Gi", 1024}, {"G", 1000}, {"Mi", 1}, {"M", 1},
	} {
		if rest, ok := strings.CutSuffix(s, u.suffix); ok {
			n, err := strconv.ParseFloat(rest, 64)
			if err != nil {
				return 0
			}
			return int64(n * float64(u.mib))
		}
	}
	// A bare number is bytes, which is legal and which nobody types.
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n / (1 << 20)
}

// FormatCPU turns millicores back into what the cluster is given.
//
// Whole cores where it divides cleanly, because "2" is what somebody meant and
// "2000m" is the same thing said in a way that invites a second look.
func FormatCPU(millis int64) string {
	switch {
	case millis <= 0:
		return ""
	case millis%1000 == 0:
		return strconv.FormatInt(millis/1000, 10)
	}
	return strconv.FormatInt(millis, 10) + "m"
}

// FormatMemory turns mebibytes back into what the cluster is given.
func FormatMemory(mib int64) string {
	switch {
	case mib <= 0:
		return ""
	case mib%1024 == 0:
		return strconv.FormatInt(mib/1024, 10) + "Gi"
	}
	return strconv.FormatInt(mib, 10) + "Mi"
}

// cpuDisplay is how much CPU that is, in the words people use.
func cpuDisplay(millis int64) string {
	switch {
	case millis <= 0:
		return "No limit"
	case millis < 1000:
		return fmt.Sprintf("%.2g vCPU", float64(millis)/1000)
	case millis%1000 == 0:
		return fmt.Sprintf("%d vCPU", millis/1000)
	}
	return fmt.Sprintf("%.1f vCPU", float64(millis)/1000)
}

func memDisplay(mib int64) string {
	switch {
	case mib <= 0:
		return "No limit"
	case mib < 1024:
		return fmt.Sprintf("%d MB", mib)
	case mib%1024 == 0:
		return fmt.Sprintf("%d GB", mib/1024)
	}
	return fmt.Sprintf("%.1f GB", float64(mib)/1024)
}

// roundUp lifts a capacity to the next whole step, so the ceiling is a number
// the handle can actually reach.
func roundUp(v, step int64) int64 {
	if v <= 0 || step <= 0 {
		return 0
	}
	if v%step == 0 {
		return v
	}
	return (v/step + 1) * step
}

// sliderStyle paints the filled part of the track.
//
// A gradient on the input itself rather than a second element behind it: two
// elements would need to agree about where the handle is, and they disagree
// the moment one of them is a pixel out.
func sliderStyle(s ResourceSlider) string {
	pct := 0
	if s.Max > 0 {
		pct = int(s.Value * 100 / s.Max)
	}
	return fmt.Sprintf(
		"background:linear-gradient(to right,var(--foreground) 0%%,var(--foreground) %d%%,var(--muted) %d%%,var(--muted) 100%%)",
		pct, pct)
}
