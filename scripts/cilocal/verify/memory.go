package verify

import (
	"fmt"
	"regexp"
	"strconv"
)

// A run started on a starved machine fails timing-bound steps for reasons
// outside the code, and a failed head is final. So before its steps start,
// make ci-local waits until MemoryPressure is "" (Linux only: it reads
// /proc/meminfo).

// MinAvailableGiB is the default floor of MemAvailable: a full cold
// `make test` peaks around 2 GB (AGENTS.md "Verifying"), with lint and the
// release smoke beside it.
const MinAvailableGiB = 4

// SwapClimbKiB is swap growth between two samples that counts as climbing.
const SwapClimbKiB = 256 * 1024

const kibPerGiB = 1024 * 1024

// Meminfo is the two fields of /proc/meminfo the check reads, in KiB.
type Meminfo struct {
	Available, SwapUsed int64
}

// ReadMeminfo reads /proc/meminfo's text.
func ReadMeminfo(text string) (Meminfo, error) {
	field := func(name string) (int64, error) {
		match := regexp.MustCompile(`(?m)^` + name + `:\s+(\d+) kB$`).FindStringSubmatch(text)
		if match == nil {
			return 0, fmt.Errorf("/proc/meminfo has no %s", name)
		}
		value, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("/proc/meminfo %s: %w", name, err)
		}
		return value, nil
	}
	available, err := field("MemAvailable")
	if err != nil {
		return Meminfo{}, err
	}
	total, err := field("SwapTotal")
	if err != nil {
		return Meminfo{}, err
	}
	free, err := field("SwapFree")
	if err != nil {
		return Meminfo{}, err
	}
	return Meminfo{Available: available, SwapUsed: total - free}, nil
}

// MemoryPressure is why the machine is too starved to start, from two
// samples a few seconds apart, or "".
func MemoryPressure(before, after Meminfo, minAvailableGiB float64) string {
	if float64(after.Available) < minAvailableGiB*kibPerGiB {
		return fmt.Sprintf("MemAvailable is %.1f GiB, under %g GiB", float64(after.Available)/kibPerGiB, minAvailableGiB)
	}
	const kibPerMiB = 1024
	if climb := after.SwapUsed - before.SwapUsed; climb >= SwapClimbKiB {
		return fmt.Sprintf("swap use grew %d MiB while sampling", climb/kibPerMiB)
	}
	return ""
}
