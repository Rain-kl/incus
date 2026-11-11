//go:build linux

package security

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func TestSecurityModeDurableResumePlan(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "mode"))
	item := ResumeInstance{Project: "project", Name: "running", UUID: "identity", Frozen: true}
	snapshots := 0
	m.SetSnapshot(func(context.Context) ([]ResumeInstance, error) { snapshots++; return []ResumeInstance{item}, nil })
	stops := 0
	m.SetStop(func(context.Context) error {
		state, err := m.Status()
		if err != nil || !state.ResumeCaptured || len(state.ResumeInstances) != 1 || state.ResumeInstances[0] != item {
			t.Fatal("stop preceded durable checkpoint", state, err)
		}
		stops++
		return nil
	})
	if m.Trigger("panel_request") != nil {
		t.Fatal("lock failed")
	}
	m.Lockdown(context.Background())
	if snapshots != 1 || stops != 1 {
		t.Fatal(snapshots, stops)
	}
	if m.Trigger("panel_request") != nil {
		t.Fatal("retry failed")
	}
	m.Lockdown(context.Background())
	if snapshots != 1 || stops != 2 {
		t.Fatal("retry recaptured stopped inventory")
	}
	clone := New(m.dir)
	if clone.Boot() != nil {
		t.Fatal("restart failed")
	}
	state, _ := clone.Status()
	if !state.ResumeCaptured || len(state.ResumeInstances) != 1 {
		t.Fatal("restart lost recovery plan")
	}
	clone.SetStop(func(context.Context) error { return nil })
	clone.Lockdown(context.Background())
	state, _ = clone.Status()
	if clone.AcknowledgeResume(state.Revision, item) == nil {
		t.Fatal("acknowledged while locked")
	}
	if clone.Recover(state.Revision) != nil {
		t.Fatal("remote recovery failed")
	}
	state, _ = clone.Status()
	if len(state.ResumeInstances) != 1 {
		t.Fatal("unlock discarded recovery plan")
	}
	if clone.AcknowledgeResume(state.Revision-1, item) == nil {
		t.Fatal("stale acknowledgement")
	}
	wrong := item
	wrong.UUID = "replacement"
	if clone.AcknowledgeResume(state.Revision, wrong) == nil {
		t.Fatal("acknowledged another identity")
	}
	body, _ := json.Marshal(map[string]any{"revision": state.Revision, "instance": item})
	code, _ := clone.Handle(request("POST", "/resume", string(body), true))
	if code != 200 {
		t.Fatal("cannot acknowledge completed recovery", code)
	}
	state, _ = clone.Status()
	if len(state.ResumeInstances) != 0 {
		t.Fatal("completed identity retained")
	}
}

func TestSecurityModeResumeAuthorityAndSnapshotFailure(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "mode"))
	m.SetSnapshot(func(context.Context) ([]ResumeInstance, error) { return nil, errors.New("inventory unavailable") })
	m.SetStop(func(context.Context) error { return nil })
	_ = m.Trigger("panel_request")
	m.Lockdown(context.Background())
	state, _ := m.Status()
	if !state.Locked || state.ResumeCaptured || len(state.Results) < 2 || !state.Results[0].Success || state.Results[1].Target != "recovery_plan" || state.Results[1].Success || m.Recover(state.Revision) == nil {
		t.Fatal("failed checkpoint reported containment complete")
	}
	code, _ := m.Handle(request("POST", "/resume", `{"revision":1,"instance":{"project":"default","name":"c","uuid":"identity","frozen":false}}`, false))
	if code != 403 {
		t.Fatal("local recovery acknowledgement allowed", code)
	}
	code, _ = m.Handle(request("POST", "/resume", `{}`, true))
	if code != 409 {
		t.Fatal("malformed acknowledgement allowed", code)
	}
}

func TestSecurityModeReportsExcludeTelemetryEvents(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "mode"))
	for i := 0; i < 3; i++ {
		_ = m.Report()
		_ = m.Check()
	}
	events, err := m.Events()
	if err != nil || len(events) != 0 {
		t.Fatal("telemetry became CLI events")
	}
	_, err = m.Audit("info", false, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_ = m.Report()
	}
	events, err = m.Events()
	if err != nil || len(events) != 1 || events[0].Command != "info" {
		t.Fatal("CLI event was lost or duplicated by telemetry")
	}
}
