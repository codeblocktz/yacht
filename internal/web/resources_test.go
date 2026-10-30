package web

import "testing"

// A track with nothing set says what the app is given, and stops at what one
// replica may have. It said "No limit" over a pod capped at 0.1 vCPU and
// 128 MiB, and offered 4 vCPU and 9.8 GB that every deploy then refused above
// 2 vCPU and 4 GiB.
func TestASliderTellsTheTruthAboutTheDefaultAndTheCeiling(t *testing.T) {
	const node = 4000          // millicores on the largest node
	const nodeMem = 9984 << 20 // bytes on the largest node, "9.8 GB"

	cpu := cpuSlider("cpu_limit", "CPU", "", "500m", node)
	if !cpu.Unset || cpu.Display != "Default, 0.5 vCPU" {
		t.Errorf("unset CPU reads %q (unset %v), want the default the pod gets", cpu.Display, cpu.Unset)
	}
	if cpu.Max != 2000 || cpu.MaxDisplay != "2 vCPU" {
		t.Errorf("CPU ceiling = %d (%s), want 2 vCPU: what the namespace lets one container have", cpu.Max, cpu.MaxDisplay)
	}

	mem := memSlider("memory_limit", "Memory", "", "512Mi", nodeMem)
	if mem.Display != "Default, 512 MB" || mem.DefaultDisplay != "Default, 512 MB" {
		t.Errorf("unset memory reads %q / %q, want the default the pod gets", mem.Display, mem.DefaultDisplay)
	}
	if mem.Max != 4096 || mem.MaxDisplay != "4 GB" {
		t.Errorf("memory ceiling = %d (%s), want 4 GB", mem.Max, mem.MaxDisplay)
	}

	// A node smaller than that is the ceiling instead: the pod has to fit.
	if small := memSlider("memory_limit", "Memory", "", "512Mi", 2<<30); small.Max != 2048 {
		t.Errorf("on a 2 GiB node the ceiling is %d MiB, want 2048", small.Max)
	}
	// And a value somebody chose reads as itself.
	if set := memSlider("memory_limit", "Memory", "1Gi", "512Mi", nodeMem); set.Unset || set.Display != "1 GB" {
		t.Errorf("1Gi reads %q (unset %v), want 1 GB", set.Display, set.Unset)
	}
}
