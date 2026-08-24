package sandlock

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestParseUtimeStime(t *testing.T) {
	// A realistic /proc/<pid>/stat line; comm contains a space and
	// parentheses to exercise the "split after the last ')'" parsing.
	stat := "123 (my (weird) proc) S 1 123 123 0 -1 4194304 100 0 0 0 55 20 0 0 20 0 1 0 12345 0 0 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0"
	ticks, err := parseUtimeStime(stat)
	if err != nil {
		t.Fatalf("parseUtimeStime: %v", err)
	}
	if ticks != 55+20 {
		t.Errorf("ticks = %d, want %d", ticks, 55+20)
	}
}

func TestParseUtimeStimeMalformed(t *testing.T) {
	if _, err := parseUtimeStime("no closing paren"); err == nil {
		t.Error("expected an error for a stat record with no comm field")
	}
	if _, err := parseUtimeStime("1 (comm) S"); err == nil {
		t.Error("expected an error for a stat record with too few fields")
	}
}

func TestParseVmRSS(t *testing.T) {
	status := "Name:\tsh\nVmPeak:\t   8000 kB\nVmRSS:\t   4096 kB\nThreads:\t1\n"
	bytesValue, err := parseVmRSS(status)
	if err != nil {
		t.Fatalf("parseVmRSS: %v", err)
	}
	if bytesValue != 4096*1024 {
		t.Errorf("bytesValue = %d, want %d", bytesValue, 4096*1024)
	}
}

func TestParseVmRSSMissing(t *testing.T) {
	if _, err := parseVmRSS("Name:\tsh\n"); err == nil {
		t.Error("expected an error when VmRSS is absent")
	}
}

func TestSampleProcessAgainstRealProcess(t *testing.T) {
	reading, err := sampleProcess("task-1", os.Getpid())
	if err != nil {
		t.Fatalf("sampleProcess: %v", err)
	}
	if reading.TaskID != "task-1" || reading.Component != "sandbox" {
		t.Errorf("reading identity = %+v", reading)
	}
	if reading.MemoryUsageBytes == 0 {
		t.Error("expected a nonzero MemoryUsageBytes for the running test process")
	}
}

func TestSampleProcessGone(t *testing.T) {
	// PID 1 always exists; an implausibly large PID should not.
	_, err := sampleProcess("task-1", 1<<30)
	if !errors.Is(err, errProcessGone) {
		t.Errorf("sampleProcess for a nonexistent PID = %v, want errProcessGone", err)
	}
}

func TestResourceSourceSamplesOnlySelectedTasks(t *testing.T) {
	manager := &Manager{sandboxes: map[string]*Sandbox{}}
	present := &Sandbox{owner: manager, runID: "run-1", taskID: "task-1", activePGID: os.Getpid()}
	other := &Sandbox{owner: manager, runID: "run-1", taskID: "task-2", activePGID: os.Getpid()}
	otherRun := &Sandbox{owner: manager, runID: "run-2", taskID: "task-3", activePGID: os.Getpid()}
	manager.sandboxes["task-1"] = present
	manager.sandboxes["task-2"] = other
	manager.sandboxes["task-3"] = otherRun

	source, err := manager.NewResourceSource("run-1", []string{"task-1"})
	if err != nil {
		t.Fatalf("NewResourceSource: %v", err)
	}
	readings, err := source.Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(readings) != 1 || readings[0].TaskID != "task-1" {
		t.Errorf("readings = %+v, want exactly task-1", readings)
	}
}
