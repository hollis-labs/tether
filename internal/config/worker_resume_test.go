package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDefaultsWorkerResume(t *testing.T) {
	var d Defaults
	if err := yaml.Unmarshal([]byte("launch_host: direct\n"), &d); err != nil || d.WorkerResume {
		t.Fatalf("default: WorkerResume = %v, err = %v; want off", d.WorkerResume, err)
	}
	if err := yaml.Unmarshal([]byte("worker_resume: true\n"), &d); err != nil || !d.WorkerResume {
		t.Fatalf("worker_resume: true -> %v, err = %v", d.WorkerResume, err)
	}
}
