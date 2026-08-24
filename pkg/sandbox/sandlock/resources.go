package sandlock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

// ResourceSource samples process-level CPU/memory for sandlock's active task
// processes. Docker's resource source queries the daemon, which independently
// holds container identity by label; sandlock has no such external registry,
// so this reads the owning Manager's live record of active task processes
// instead.
type ResourceSource struct {
	manager *Manager
	runID   string
	tasks   map[string]struct{}
}

// NewResourceSource scopes a resource source to one run's task set, mirroring
// pkg/sandbox/docker's ResourceOptions shape.
func (m *Manager) NewResourceSource(runID string, taskIDs []string) (*ResourceSource, error) {
	if err := validateIdentity("run", runID); err != nil {
		return nil, err
	}
	if len(taskIDs) == 0 {
		return nil, errors.New("sandlock resource source requires at least one task ID")
	}
	tasks := make(map[string]struct{}, len(taskIDs))
	for _, taskID := range taskIDs {
		if err := validateIdentity("task", taskID); err != nil {
			return nil, err
		}
		if _, exists := tasks[taskID]; exists {
			return nil, fmt.Errorf("sandlock resource task ID %q is repeated", taskID)
		}
		tasks[taskID] = struct{}{}
	}
	return &ResourceSource{manager: m, runID: runID, tasks: tasks}, nil
}

func (source *ResourceSource) Sample(ctx context.Context) ([]core.ResourceReading, error) {
	active := source.manager.activeProcesses(source.runID)
	readings := make([]core.ResourceReading, 0, len(active))
	for taskID, pid := range active {
		if _, selected := source.tasks[taskID]; !selected {
			continue
		}
		reading, err := sampleProcess(taskID, pid)
		if errors.Is(err, errProcessGone) {
			continue
		}
		if err != nil {
			return nil, err
		}
		readings = append(readings, reading)
	}
	return readings, nil
}

func (source *ResourceSource) Close() error { return nil }

var errProcessGone = errors.New("sandlock task process disappeared during sampling")

// linuxUserHZ is the kernel clock tick rate used to scale /proc/<pid>/stat's
// utime/stime fields. It is 100 on every architecture ARIES currently
// targets; a host with a different value would need this made configurable.
const linuxUserHZ = 100

func sampleProcess(taskID string, pid int) (core.ResourceReading, error) {
	statBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if os.IsNotExist(err) {
		return core.ResourceReading{}, errProcessGone
	}
	if err != nil {
		return core.ResourceReading{}, fmt.Errorf("read sandlock task process stat: %w", err)
	}
	cpuTicks, err := parseUtimeStime(string(statBytes))
	if err != nil {
		return core.ResourceReading{}, fmt.Errorf("parse sandlock task process stat: %w", err)
	}
	statusBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if os.IsNotExist(err) {
		return core.ResourceReading{}, errProcessGone
	}
	if err != nil {
		return core.ResourceReading{}, fmt.Errorf("read sandlock task process status: %w", err)
	}
	rssBytes, err := parseVmRSS(string(statusBytes))
	if err != nil {
		return core.ResourceReading{}, fmt.Errorf("parse sandlock task process status: %w", err)
	}
	nsPerTick := int64(time.Second) / linuxUserHZ
	return core.ResourceReading{
		TaskID: taskID, Component: "sandbox", RuntimeID: strconv.Itoa(pid), RuntimeName: "sandlock-" + taskID,
		ObservedAt: time.Now(), CPUUsageNanoseconds: uint64(cpuTicks * nsPerTick), MemoryUsageBytes: rssBytes,
	}, nil
}

// parseUtimeStime reads fields 14 (utime) and 15 (stime) from a
// /proc/<pid>/stat record. The record's second field (comm) is parenthesized
// and may itself contain spaces or parentheses, so fields are counted from
// the last ')' rather than by naive splitting.
func parseUtimeStime(stat string) (int64, error) {
	closeParen := strings.LastIndexByte(stat, ')')
	if closeParen < 0 || closeParen+2 > len(stat) {
		return 0, errors.New("malformed stat record")
	}
	fields := strings.Fields(stat[closeParen+2:])
	if len(fields) < 13 {
		return 0, errors.New("stat record has too few fields")
	}
	utime, err := strconv.ParseInt(fields[11], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse utime: %w", err)
	}
	stime, err := strconv.ParseInt(fields[12], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse stime: %w", err)
	}
	return utime + stime, nil
}

func parseVmRSS(status string) (uint64, error) {
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, errors.New("malformed VmRSS line")
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse VmRSS: %w", err)
		}
		return kb * 1024, nil
	}
	return 0, errors.New("VmRSS not found")
}
