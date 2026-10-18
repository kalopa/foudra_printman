//
// Copyright (c) 2026, Kalopa Robotics Limited.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
package farm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kalopa/print_manager/internal/mrp"
	"github.com/kalopa/print_manager/internal/printer"
)

// fakePrinter answers with whatever state the test sets.
type fakePrinter struct {
	state   printer.State
	jobID   int
	uploads []string
	err     error // when set, Status fails as an unreachable printer does
}

func (f *fakePrinter) Upload(_ context.Context, name string, body io.Reader, _ int64, printNow bool) error {
	b, _ := io.ReadAll(body)
	f.uploads = append(f.uploads, fmt.Sprintf("%s|%v|%d bytes", name, printNow, len(b)))
	f.state, f.jobID = printer.Printing, 200
	return nil
}

// Status reports the state the test set. Raw is the upper-cased word,
// as PrusaLink would send it, since notes and messages quote it.
func (f *fakePrinter) Status(context.Context) (*printer.Status, error) {
	if f.err != nil {
		return nil, f.err
	}
	s := &printer.Status{State: f.state, Raw: strings.ToUpper(string(f.state))}
	if f.jobID != 0 {
		s.Job = &printer.Job{ID: f.jobID, File: "rib_PETG.bgcode", Progress: 50, TimeRemaining: 600}
	}
	return s, nil
}

// fakeMRP records writes and serves one workstation and one running step.
type fakeMRP struct {
	ws         mrp.Workstation
	custom     Settings
	partCustom string // JSON served for any part
	step       *mrp.Step
	order      mrp.WorkOrder
	next       *mrp.Job // what Next returns when the filter matches (or no filter)
	nextOther  *mrp.Job // what unfiltered Next returns when next is nil
	statuses   []string
	completed  []string
	paused     []int64
	consumed   [][]mrp.Consumed
	started    []int64
	scrapped   []string
	material   *int64
	outcomes   map[string]string // what the operator chose, by task key
	tasks      []mrp.Task
}

func (m *fakeMRP) StartStep(_ context.Context, stepID, wsID int64) error {
	m.started = append(m.started, stepID)
	if m.step != nil && m.step.ID == stepID {
		m.step.Status, m.step.WorkstationID = "running", &wsID
	}
	return nil
}
func (m *fakeMRP) ScrapStep(_ context.Context, stepID int64, qty string, c []mrp.Consumed, note string) error {
	m.scrapped = append(m.scrapped, qty+"|"+note)
	m.consumed = append(m.consumed, c)
	return nil
}
func (m *fakeMRP) SetMaterial(_ context.Context, _ int64, partID *int64) error {
	m.material = partID
	return nil
}
func (m *fakeMRP) TaskStates(context.Context, string) ([]mrp.TaskState, error) {
	var out []mrp.TaskState
	for k, o := range m.outcomes {
		o := o
		out = append(out, mrp.TaskState{Key: k, Outcome: &o})
	}
	return out, nil
}

func (m *fakeMRP) Workstations(context.Context) ([]mrp.Workstation, error) {
	return []mrp.Workstation{m.ws}, nil
}
func (m *fakeMRP) Workstation(context.Context, int64) (*mrp.Workstation, error) {
	w := m.ws
	return &w, nil
}
func (m *fakeMRP) SetStatus(_ context.Context, _ int64, status string, note *string) error {
	m.ws.Status, m.ws.StatusNote = status, note
	m.statuses = append(m.statuses, status)
	return nil
}
func (m *fakeMRP) Custom(_ context.Context, kind string, _ int64) (json.RawMessage, error) {
	if kind == "parts" {
		if m.partCustom == "" {
			return json.RawMessage(`{"per_bed": 6}`), nil
		}
		return json.RawMessage(m.partCustom), nil
	}
	b, _ := json.Marshal(m.custom)
	return b, nil
}

// SetCustom replaces the blob wholesale, as the MRP does.
func (m *fakeMRP) SetCustom(_ context.Context, _ string, _ int64, v any) error {
	b, _ := json.Marshal(v)
	m.custom = Settings{}
	return json.Unmarshal(b, &m.custom)
}
func (m *fakeMRP) WorkOrders(context.Context, string) ([]mrp.WorkOrder, error) {
	if m.step == nil {
		return nil, nil
	}
	return []mrp.WorkOrder{m.order}, nil
}
func (m *fakeMRP) WorkOrder(context.Context, int64) (*mrp.WorkOrder, error) {
	o := m.order
	if m.step != nil {
		o.Steps = []mrp.Step{*m.step}
	}
	return &o, nil
}
func (m *fakeMRP) PauseStep(_ context.Context, stepID int64) error {
	m.paused = append(m.paused, stepID)
	if m.step != nil && m.step.ID == stepID {
		m.step.Status = "pending"
	}
	return nil
}

func (m *fakeMRP) CompleteStep(_ context.Context, stepID int64, qty string, c []mrp.Consumed) error {
	m.completed = append(m.completed, qty)
	m.consumed = append(m.consumed, c)
	return nil
}
func (m *fakeMRP) ReplaceTasks(_ context.Context, _ string, t []mrp.Task) error {
	m.tasks = t
	return nil
}
func (m *fakeMRP) Next(_ context.Context, _ int64, material *int64) (*mrp.Job, error) {
	if material == nil {
		if m.next != nil {
			return m.next, nil
		}
		return m.nextOther, nil
	}
	return m.next, nil
}

func newFarm(m *fakeMRP) *Farm {
	return &Farm{MRP: m, Log: slog.New(slog.NewTextHandler(discard{}, nil))}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func station() mrp.Workstation {
	return mrp.Workstation{ID: 12, Code: "CORE_ONEP_1", Name: "Core One+ Printer #1", Kind: "automatic", IsActive: true, Status: "idle"}
}

func TestPrintingIsMirroredAsBusy(t *testing.T) {
	m := &fakeMRP{ws: station()}
	pr := &fakePrinter{state: printer.Printing, jobID: 141}
	p := &Printer{WS: m.ws, Link: pr}
	if _, err := newFarm(m).Tick(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if m.ws.Status != "busy" || m.ws.StatusNote == nil || !strings.Contains(*m.ws.StatusNote, "rib_PETG") {
		t.Errorf("status=%s note=%v", m.ws.Status, m.ws.StatusNote)
	}
}

func TestOperatorOfflineIsNotOverwrittenByIdlePrinter(t *testing.T) {
	m := &fakeMRP{ws: station()}
	off := "gauge away for calibration" // no [auto] tag: an operator wrote it
	m.ws.Status, m.ws.StatusNote = "offline", &off
	p := &Printer{WS: m.ws, Link: &fakePrinter{state: printer.Idle}}
	if _, err := newFarm(m).Tick(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if m.ws.Status != "offline" {
		t.Errorf("daemon overwrote an operator's offline: %s", m.ws.Status)
	}
}

func TestFinishedPrintBlocksThenCompletesOnReady(t *testing.T) {
	ws12 := int64(12)
	m := &fakeMRP{ws: station()}
	m.ws.Status = "busy"
	m.order = mrp.WorkOrder{ID: 6, Number: "WO-00001", Status: "in_progress", Quantity: "12"}
	m.order.Part.ID = 917
	m.step = &mrp.Step{ID: 41, Name: "Print", Status: "running", WorkstationID: &ws12, QuantityDone: "0"}
	pr := &fakePrinter{state: printer.Finished, jobID: 141}
	p := &Printer{WS: m.ws, Link: pr}
	f := newFarm(m)

	// Tick 1: finished → blocked, task raised, job remembered in custom JSON.
	tasks, err := f.Tick(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if m.ws.Status != "blocked" || len(tasks) != 1 || !strings.Contains(tasks[0].Message, "clear the bed") || tasks[0].Key != "CORE_ONEP_1-bed-141" {
		t.Fatalf("after finish: status=%s tasks=%v", m.ws.Status, tasks)
	}
	if len(tasks[0].Actions) != 1 || tasks[0].Actions[0].Key != "ready" {
		t.Errorf("a run the daemon did not start offers only Ready: %v", tasks[0].Actions)
	}
	if m.custom.AwaitingClearJob != 141 {
		t.Errorf("awaiting_clear_job = %d", m.custom.AwaitingClearJob)
	}

	// Tick 2: still finished, operator hasn't come: same task, no new writes.
	before := len(m.statuses)
	tasks, _ = f.Tick(context.Background(), p)
	if len(tasks) != 1 || len(m.statuses) != before {
		t.Errorf("second finished tick changed things: tasks=%d statuses=%d->%d", len(tasks), before, len(m.statuses))
	}

	// Tick 3: the operator pressed "Bed clear" on the task. Complete one
	// bed (per_bed 6 of 12) and hand the station back.
	m.outcomes = map[string]string{"CORE_ONEP_1-bed-141": "ready"}
	f.refreshOutcomes(context.Background())
	tasks, err = f.Tick(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 || len(m.completed) != 1 || m.completed[0] != "6" || m.ws.Status != "idle" {
		t.Errorf("after ready: tasks=%v completed=%v status=%s", tasks, m.completed, m.ws.Status)
	}
	if m.custom.AwaitingClearJob != 0 || m.custom.LastCompletedJob != 141 {
		t.Errorf("bookkeeping = %+v", m.custom)
	}

	// Tick 4: printer still says FINISHED for job 141 — must not complete again.
	f.Tick(context.Background(), p)
	if len(m.completed) != 1 {
		t.Errorf("completed twice for one job: %v", m.completed)
	}
}

func TestKeyEnvFor(t *testing.T) {
	for in, want := range map[string]string{"CORE_ONEP_1": "PRINTER_KEY_CORE_ONEP_1", "print-2": "PRINTER_KEY_PRINT_2"} {
		if got := KeyEnvFor(in); got != want {
			t.Errorf("KeyEnvFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// Discover picks each station's driver from its custom JSON, finds the
// key under its new name or the old one, and skips what it can't drive.
func TestDiscoverChoosesTheDriver(t *testing.T) {
	for _, tc := range []struct {
		name     string
		custom   string
		env      string
		wantKind string // "" means skipped
	}{
		{"existing prusalink station, old key name", `{"prusalink_url":"http://192.0.2.7"}`, "PRUSALINK_KEY_CORE_ONEP_1", "prusalink.Driver"},
		{"octoprint", `{"driver":"octoprint","url":"http://192.0.2.8"}`, "PRINTER_KEY_CORE_ONEP_1", "octoprint.Driver"},
		{"unknown driver", `{"driver":"bambu","url":"http://192.0.2.9"}`, "PRINTER_KEY_CORE_ONEP_1", ""},
		{"no key", `{"driver":"octoprint","url":"http://192.0.2.8"}`, "", ""},
		{"no address", `{"driver":"octoprint"}`, "PRINTER_KEY_CORE_ONEP_1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv(tc.env, "k")
			}
			m := &fakeMRP{ws: station()}
			if err := json.Unmarshal([]byte(tc.custom), &m.custom); err != nil {
				t.Fatal(err)
			}
			fleet, err := Discover(context.Background(), m, slog.New(slog.NewTextHandler(discard{}, nil)))
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if len(fleet) == 1 {
				got = fmt.Sprintf("%T", fleet[0].Link)
				got = got[strings.LastIndex(got, "/")+1:]
			}
			if got != tc.wantKind {
				t.Errorf("driver = %q, want %q", got, tc.wantKind)
			}
		})
	}
}

// A ready job with G-code on disk: the daemon claims the step, sends
// the file with print-after-upload, records the run, and goes busy.
// Then, after finish and Ready, it completes with the sidecar's
// filament as the actual.
func TestIdlePrinterStartsTheNextJobAndReportsActuals(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "case.bgcode"), []byte("GCODE"), 0o644)
	os.WriteFile(filepath.Join(dir, "case.json"), []byte(`{"per_bed": 1, "est_seconds": 3600, "filament_g": 42.5, "material": "PETG"}`), 0o644)

	petg := int64(934)
	m := &fakeMRP{ws: station(), partCustom: `{"gcode": {"core-one-0.4": "case.bgcode"}}`}
	m.ws.CurrentMaterial = &mrp.MaterialRef{PartID: petg, Name: ptr("PETG black")}
	job := &mrp.Job{StepID: 41, Name: "Print"}
	job.WorkOrder.Number, job.WorkOrder.Quantity = "WO-00020", "1"
	job.Part.ID, job.Part.Name = 917, "Bow node case"
	job.Materials = []mrp.Material{{PartID: petg, PartNumber: "500-000007-00", Name: "PETG black", UOM: "kg", QtyPer: "0.27"}}
	m.next = job
	ws12 := int64(12)
	m.order = mrp.WorkOrder{ID: 20, Number: "WO-00020", Status: "in_progress", Quantity: "1"}
	m.order.Part.ID = 917
	// Pending until the daemon claims it; the fake's StartStep flips it.
	m.step = &mrp.Step{ID: 41, Name: "Print", Status: "pending", QuantityDone: "0"}
	_ = ws12

	pr := &fakePrinter{state: printer.Idle}
	p := &Printer{WS: m.ws, Link: pr, Settings: Settings{Profile: "core-one-0.4"}}
	f := newFarm(m)
	f.Cfg.GCodeDir = dir
	f.now = func() time.Time { return time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC) }

	tasks, err := f.Tick(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 || len(m.started) != 1 || m.started[0] != 41 {
		t.Fatalf("start: tasks=%v started=%v", tasks, m.started)
	}
	if len(pr.uploads) != 1 || pr.uploads[0] != "case.bgcode|true|5 bytes" {
		t.Errorf("uploads = %v", pr.uploads)
	}
	if m.ws.Status != "busy" || m.custom.Run == nil || m.custom.Run.StepID != 41 || m.custom.Run.FilamentG != 42.5 || m.custom.Run.FilamentPartID != petg {
		t.Errorf("after start: status=%s run=%+v", m.ws.Status, m.custom.Run)
	}

	// The printer prints (the fake flipped to PRINTING, job 200), then finishes.
	pr.state = printer.Finished
	p.Settings = m.custom
	if _, err := f.Tick(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if m.ws.Status != "blocked" {
		t.Fatalf("after finish: %s", m.ws.Status)
	}
	// Operator clears the bed and taps Ready.
	m.ws.Status = "idle"
	p.Settings = m.custom
	if _, err := f.Tick(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(m.completed) != 1 || m.completed[0] != "1" {
		t.Fatalf("completed = %v", m.completed)
	}
	if len(m.consumed[0]) != 1 || m.consumed[0][0].PartID != petg || m.consumed[0][0].Quantity != "0.0425" {
		t.Errorf("actuals = %+v, want 0.0425 kg of PETG", m.consumed[0])
	}
	if m.custom.Run != nil {
		t.Error("run not cleared after completion")
	}

	// The printer still says FINISHED (nobody tapped its screen) but the
	// bed is clear and the MRP has another job: start it anyway.
	m.next = job
	m.step = &mrp.Step{ID: 41, Name: "Print", Status: "pending", QuantityDone: "0"}
	m.started = nil
	p.Settings = m.custom
	if _, err := f.Tick(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(m.started) != 1 || len(pr.uploads) != 2 {
		t.Errorf("after FINISHED with a clear bed: started=%v uploads=%v, want the next job sent", m.started, pr.uploads)
	}
}

func TestWindowAndMissingGCode(t *testing.T) {
	m := &fakeMRP{ws: station(), partCustom: `{}`}
	job := &mrp.Job{StepID: 41, Name: "Print"}
	job.WorkOrder.Number = "WO-00020"
	job.Part.ID, job.Part.Name = 917, "Bow node case"
	m.next = job
	pr := &fakePrinter{state: printer.Idle}
	p := &Printer{WS: m.ws, Link: pr, Settings: Settings{Profile: "core-one-0.4"}}
	f := newFarm(m)

	// No G-code on the part: a task pointing at the part's custom data, no upload.
	tasks, err := f.Tick(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || !strings.Contains(tasks[0].Message, "no G-code") || len(pr.uploads) != 0 {
		t.Errorf("missing gcode: tasks=%v uploads=%v", tasks, pr.uploads)
	}

	// Window: a 4-hour job at 20:00 with a 22:00 finish-by does not start.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "case.bgcode"), []byte("GCODE"), 0o644)
	os.WriteFile(filepath.Join(dir, "case.json"), []byte(`{"est_seconds": 14400}`), 0o644)
	m.partCustom = `{"gcode": {"core-one-0.4": "case.bgcode"}}`
	f.Cfg.GCodeDir = dir
	p.Settings.Window.Start, p.Settings.Window.End, p.Settings.Window.Timezone, p.Settings.Window.MustFinishInWindow = "08:00", "22:00", "UTC", true
	f.now = func() time.Time { return time.Date(2026, 9, 28, 20, 0, 0, 0, time.UTC) }
	if _, err := f.Tick(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(pr.uploads) != 0 {
		t.Errorf("started outside the window: %v", pr.uploads)
	}
	// At 10:00 it starts.
	f.now = func() time.Time { return time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC) }
	if _, err := f.Tick(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(pr.uploads) != 1 {
		t.Errorf("did not start inside the window: %v", pr.uploads)
	}
}

// Loaded PETG, only a PP job ready: no print, a spool-change task.
func TestWrongMaterialAsksForAChange(t *testing.T) {
	petg, ppcf := int64(934), int64(935)
	m := &fakeMRP{ws: station()}
	m.ws.CurrentMaterial = &mrp.MaterialRef{PartID: petg, Name: ptr("PETG black")}
	other := &mrp.Job{StepID: 50, Name: "Print", RequiresMaterialChange: true}
	other.WorkOrder.Number, other.Part.Name = "WO-00031", "Wing rib 3"
	other.Materials = []mrp.Material{{PartID: ppcf, PartNumber: "500-000009-00", Name: "Tectonic PP CF", UOM: "kg"}}
	m.nextOther = other
	pr := &fakePrinter{state: printer.Idle}
	p := &Printer{WS: m.ws, Link: pr}
	tasks, err := newFarm(m).Tick(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || !strings.Contains(tasks[0].Message, "change filament from PETG black to 500-000009-00 Tectonic PP CF") || len(pr.uploads) != 0 {
		t.Errorf("tasks=%v uploads=%v", tasks, pr.uploads)
	}
}

func ptr(s string) *string { return &s }

// "Print failed": the run's filament is scrapped at the slicer's figure,
// nothing is produced, the station is handed back — and because the
// step is still running, the next idle tick resumes it without asking
// next-job (which only offers pending steps).
func TestPrintFailedScrapsAndRetries(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "case.bgcode"), []byte("GCODE"), 0o644)
	os.WriteFile(filepath.Join(dir, "case.json"), []byte(`{"per_bed": 1, "est_seconds": 600, "filament_g": 42.5}`), 0o644)
	petg := int64(934)
	ws12 := int64(12)
	m := &fakeMRP{ws: station(), partCustom: `{"gcode": {"core-one-0.4": "case.bgcode"}}`}
	m.ws.CurrentMaterial = &mrp.MaterialRef{PartID: petg, Name: ptr("PETG black")}
	m.order = mrp.WorkOrder{ID: 20, Number: "WO-00020", Status: "in_progress", Quantity: "1"}
	m.order.Part.ID, m.order.Part.Name = 917, "Bow node case"
	m.step = &mrp.Step{ID: 41, Name: "Print", Status: "running", WorkstationID: &ws12, QuantityDone: "0",
		Materials: []mrp.Material{{PartID: petg, UOM: "kg"}}}
	pr := &fakePrinter{state: printer.Finished, jobID: 300}
	p := &Printer{WS: m.ws, Link: pr, Settings: Settings{Profile: "core-one-0.4",
		AwaitingClearJob: 300, Run: &Run{StepID: 41, PerBed: "1", FilamentPartID: petg, FilamentG: 42.5}}}
	m.ws.Status = "blocked"
	f := newFarm(m)
	f.Cfg.GCodeDir = dir

	m.outcomes = map[string]string{"CORE_ONEP_1-bed-300": "failed"}
	f.refreshOutcomes(context.Background())
	if _, err := f.Tick(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(m.scrapped) != 1 || m.scrapped[0] != "1|Print failed (reported at the printer)" || len(m.completed) != 0 {
		t.Fatalf("scrapped=%v completed=%v", m.scrapped, m.completed)
	}
	if m.consumed[0][0].Quantity != "0.0425" || m.ws.Status != "idle" || m.custom.Run != nil {
		t.Errorf("consumed=%v status=%s run=%v", m.consumed[0], m.ws.Status, m.custom.Run)
	}

	// The printer is back to IDLE (dismissed on the screen): the same
	// step is resumed — no StartStep, file sent again.
	pr.state, pr.jobID = printer.Idle, 0
	p.Settings = m.custom
	p.Settings.Profile = "core-one-0.4"
	if _, err := f.Tick(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(pr.uploads) != 1 || len(m.started) != 0 {
		t.Errorf("retry: uploads=%v started=%v (a held step is not started again)", pr.uploads, m.started)
	}
}

// The spool-change task carries a button; pressing it records the
// material, and the next tick prints in it.
func TestLoadedButtonSetsTheMaterial(t *testing.T) {
	petg, ppcf := int64(934), int64(935)
	m := &fakeMRP{ws: station()}
	m.ws.CurrentMaterial = &mrp.MaterialRef{PartID: petg, Name: ptr("PETG black")}
	other := &mrp.Job{StepID: 50, Name: "Print", RequiresMaterialChange: true}
	other.WorkOrder.Number, other.Part.Name = "WO-00031", "Wing rib 3"
	other.Materials = []mrp.Material{{PartID: ppcf, PartNumber: "500-000009-00", Name: "Tectonic PP CF", UOM: "kg"}}
	m.nextOther = other
	p := &Printer{WS: m.ws, Link: &fakePrinter{state: printer.Idle}}
	f := newFarm(m)
	tasks, _ := f.Tick(context.Background(), p)
	if len(tasks) != 1 || tasks[0].Key != "CORE_ONEP_1-material-935" || len(tasks[0].Actions) != 1 || tasks[0].Actions[0].Key != "loaded" {
		t.Fatalf("tasks = %+v", tasks)
	}
	m.outcomes = map[string]string{"CORE_ONEP_1-material-935": "loaded"}
	f.refreshOutcomes(context.Background())
	tasks, _ = f.Tick(context.Background(), p)
	if len(tasks) != 0 || m.material == nil || *m.material != ppcf {
		t.Errorf("after loaded: tasks=%v material=%v", tasks, m.material)
	}
}

// The daemon revoked its key mid-print (or was simply away): by the
// time it looks again the printer says FINISHED and no longer reports
// the job. The run it started must still be brought to a close — with
// the job id learned while printing when there is one, and a stand-in
// derived from the step when there isn't — never left "busy" forever.
func TestFinishedPrinterThatForgotItsJobStillCloses(t *testing.T) {
	for _, sawItRunning := range []bool{true, false} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "case.bgcode"), []byte("GCODE"), 0o644)
		os.WriteFile(filepath.Join(dir, "case.json"), []byte(`{"per_bed": 1, "est_seconds": 600, "filament_g": 10}`), 0o644)
		m := &fakeMRP{ws: station(), partCustom: `{"gcode": {"core-one-0.4": "case.bgcode"}}`}
		job := &mrp.Job{StepID: 41, Name: "Print"}
		job.WorkOrder.Number, job.WorkOrder.Quantity = "WO-00020", "1"
		job.Part.ID, job.Part.Name = 917, "Bow node case"
		m.next = job
		m.order = mrp.WorkOrder{ID: 20, Number: "WO-00020", Status: "in_progress", Quantity: "1"}
		m.order.Part.ID = 917
		m.step = &mrp.Step{ID: 41, Name: "Print", Status: "pending", QuantityDone: "0"}
		pr := &fakePrinter{state: printer.Idle}
		p := &Printer{WS: m.ws, Link: pr, Settings: Settings{Profile: "core-one-0.4"}}
		f := newFarm(m)
		f.Cfg.GCodeDir = dir
		f.now = func() time.Time { return time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC) }
		if _, err := f.Tick(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		wantKey := "CORE_ONEP_1-bed--41"
		if sawItRunning {
			// One tick while printing: the job id is learned (the fake starts job 200).
			p.Settings = m.custom
			if _, err := f.Tick(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			if m.custom.Run == nil || m.custom.Run.JobID != 200 {
				t.Fatalf("job id not learned: %+v", m.custom.Run)
			}
			wantKey = "CORE_ONEP_1-bed-200"
		}
		// The printer finished and has forgotten the job.
		pr.state, pr.jobID = printer.Finished, 0
		p.Settings = m.custom
		tasks, err := f.Tick(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		if m.ws.Status != "blocked" || len(tasks) != 1 || tasks[0].Key != wantKey {
			t.Fatalf("saw=%v: status=%s tasks=%+v", sawItRunning, m.ws.Status, tasks)
		}
		// Bed cleared via Ready on the workstation page.
		m.ws.Status = "idle"
		p.Settings = m.custom
		if _, err := f.Tick(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		if len(m.completed) != 1 || m.custom.Run != nil {
			t.Errorf("saw=%v: completed=%v run=%+v", sawItRunning, m.completed, m.custom.Run)
		}
	}
}

// A five-bed order: after each bed the step is handed back to the queue
// with its progress, so the MRP chooses the next bed afresh. On the last
// bed there is nothing left to pause.
func TestStepIsPausedBetweenBeds(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "seg.bgcode"), []byte("GCODE"), 0o644)
	os.WriteFile(filepath.Join(dir, "seg.json"), []byte(`{"per_bed": 1, "est_seconds": 600, "filament_g": 10}`), 0o644)
	m := &fakeMRP{ws: station(), partCustom: `{"gcode": {"core-one-0.4": "seg.bgcode"}}`}
	job := &mrp.Job{StepID: 41, Name: "Print"}
	job.WorkOrder.Number, job.WorkOrder.Quantity = "WO-00030", "5"
	job.Part.ID, job.Part.Name = 917, "Wing segment 0F"
	m.next = job
	m.order = mrp.WorkOrder{ID: 30, Number: "WO-00030", Status: "in_progress", Quantity: "5"}
	m.order.Part.ID = 917
	m.step = &mrp.Step{ID: 41, Name: "Print", Status: "pending", QuantityDone: "0"}
	pr := &fakePrinter{state: printer.Idle}
	p := &Printer{WS: m.ws, Link: pr, Settings: Settings{Profile: "core-one-0.4"}}
	f := newFarm(m)
	f.Cfg.GCodeDir = dir
	f.now = func() time.Time { return time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC) }
	if _, err := f.Tick(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	// First bed done and cleared.
	pr.state = printer.Finished
	p.Settings = m.custom
	if _, err := f.Tick(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	m.ws.Status = "idle"
	m.step.QuantityDone = "0" // the fake's CompleteStep does not count; the daemon's arithmetic uses the bed size
	p.Settings = m.custom
	if _, err := f.Tick(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(m.completed) != 1 || m.completed[0] != "1" {
		t.Fatalf("completed = %v, want one bed", m.completed)
	}
	if len(m.paused) != 1 || m.paused[0] != 41 {
		t.Errorf("paused = %v, want step 41 handed back with four beds left", m.paused)
	}
	// Last bed: 4 of 5 already done, one made now — nothing to pause.
	m.paused = nil
	m.step = &mrp.Step{ID: 41, Name: "Print", Status: "pending", QuantityDone: "4"}
	pr.state, pr.jobID = printer.Idle, 0
	p.Settings = m.custom
	if _, err := f.Tick(context.Background(), p); err != nil { // starts again
		t.Fatal(err)
	}
	pr.state = printer.Finished
	p.Settings = m.custom
	if _, err := f.Tick(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	m.ws.Status = "idle"
	p.Settings = m.custom
	if _, err := f.Tick(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(m.paused) != 0 {
		t.Errorf("paused on the last bed: %v", m.paused)
	}
}

// A print the operator stopped (or the printer errored) is a bed with a
// question on it, like a finished one: the task appears, and Print
// failed scraps the run's filament and leaves the step to try again.
func TestStoppedPrintAsksLikeAFinishedOne(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "seg.bgcode"), []byte("GCODE"), 0o644)
	os.WriteFile(filepath.Join(dir, "seg.json"), []byte(`{"per_bed": 1, "est_seconds": 600, "filament_g": 137.7}`), 0o644)
	petg := int64(171)
	m := &fakeMRP{ws: station(), partCustom: `{"gcode": {"core-one-0.4": "seg.bgcode"}}`}
	m.ws.CurrentMaterial = &mrp.MaterialRef{PartID: petg, Name: ptr("PETG")}
	job := &mrp.Job{StepID: 41, Name: "Print"}
	job.WorkOrder.Number, job.WorkOrder.Quantity = "WO-00023", "1"
	job.Part.ID, job.Part.Name = 917, "Wing segment 7A"
	job.Materials = []mrp.Material{{PartID: petg, PartNumber: "802-00000002-00", Name: "PETG", UOM: "kg", QtyPer: "0.14"}}
	m.next = job
	m.order = mrp.WorkOrder{ID: 23, Number: "WO-00023", Status: "in_progress", Quantity: "1"}
	m.order.Part.ID = 917
	m.step = &mrp.Step{ID: 41, Name: "Print", Status: "pending", QuantityDone: "0"}
	pr := &fakePrinter{state: printer.Idle}
	p := &Printer{WS: m.ws, Link: pr, Settings: Settings{Profile: "core-one-0.4"}}
	f := newFarm(m)
	f.Cfg.GCodeDir = dir
	f.now = func() time.Time { return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC) }
	if _, err := f.Tick(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	// The operator hits STOP on the printer.
	pr.state = printer.Stopped
	p.Settings = m.custom
	tasks, err := f.Tick(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if m.ws.Status != "blocked" || len(tasks) != 1 || !strings.Contains(tasks[0].Message, "print stopped") || tasks[0].Priority != 1 {
		t.Fatalf("after stop: status=%s tasks=%+v", m.ws.Status, tasks)
	}
	if len(tasks[0].Actions) != 2 || tasks[0].Actions[1].Key != "failed" {
		t.Fatalf("actions = %+v", tasks[0].Actions)
	}
	// Print failed: the filament is scrapped at the sidecar's figure, the step stays open.
	f.outcomes = map[string]string{tasks[0].Key: "failed"}
	p.Settings = m.custom
	if _, err := f.Tick(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(m.scrapped) != 1 || len(m.completed) != 0 {
		t.Errorf("scrapped=%v completed=%v, want one scrap and no completion", m.scrapped, m.completed)
	}
}

// Someone dismissed FINISHED on the printer's screen before answering
// the bed task, so the printer says IDLE while the daemon still awaits
// an answer for the bed. Seen in the field: the station sat idle for a
// week, the task gone and nothing started. The bed must still be
// asked about — and only the task's buttons may answer it, because
// the station's idle here is the daemon's own mirror, not an operator's
// Ready. Once answered, the next tick prints again.
func TestDismissedFinishedPrintStillAsks(t *testing.T) {
	for _, tc := range []struct {
		outcome       string
		wantCompleted int
		wantScrapped  int
	}{
		{"ready", 1, 0},
		{"failed", 0, 1},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "seg.bgcode"), []byte("GCODE"), 0o644)
			os.WriteFile(filepath.Join(dir, "seg.json"), []byte(`{"per_bed": 1, "est_seconds": 600, "filament_g": 10}`), 0o644)
			ws12 := int64(12)
			m := &fakeMRP{ws: station(), partCustom: `{"gcode": {"core-one-0.4": "seg.bgcode"}}`}
			mirrored := autoTag + "IDLE"
			m.ws.StatusNote = &mirrored
			m.order = mrp.WorkOrder{ID: 10, Number: "WO-00010", Status: "in_progress", Quantity: "2"}
			m.order.Part.ID = 917
			m.step = &mrp.Step{ID: 29, Name: "Print", Status: "running", WorkstationID: &ws12, QuantityDone: "0"}
			job := &mrp.Job{StepID: 29, Name: "Print"}
			job.WorkOrder.Number, job.WorkOrder.Quantity = "WO-00010", "2"
			job.Part.ID, job.Part.Name = 917, "Wing segment 1F"
			m.next = job
			m.custom = Settings{Profile: "core-one-0.4", AwaitingClearJob: 149, LastCompletedJob: 148,
				Run: &Run{StepID: 29, WorkOrder: "WO-00010", PerBed: "1", JobID: 149, FilamentPartID: 171, FilamentG: 251.37}}
			pr := &fakePrinter{state: printer.Idle}
			p := &Printer{WS: m.ws, Link: pr, Settings: m.custom}
			f := newFarm(m)
			f.Cfg.GCodeDir = dir
			f.now = func() time.Time { return time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC) }

			// Unanswered: ask, block, start nothing, settle nothing.
			for i := 0; i < 2; i++ {
				tasks, err := f.Tick(context.Background(), p)
				if err != nil {
					t.Fatal(err)
				}
				if len(tasks) != 1 || tasks[0].Key != "CORE_ONEP_1-bed-149" || len(tasks[0].Actions) != 2 {
					t.Fatalf("tick %d: tasks=%+v", i, tasks)
				}
				if m.ws.Status != "blocked" || len(pr.uploads) != 0 || len(m.completed)+len(m.scrapped) != 0 {
					t.Fatalf("tick %d: status=%s uploads=%v completed=%v scrapped=%v", i, m.ws.Status, pr.uploads, m.completed, m.scrapped)
				}
			}

			// The operator answers on the task.
			m.outcomes = map[string]string{"CORE_ONEP_1-bed-149": tc.outcome}
			f.refreshOutcomes(context.Background())
			p.Settings = m.custom
			if _, err := f.Tick(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			if len(m.completed) != tc.wantCompleted || len(m.scrapped) != tc.wantScrapped {
				t.Fatalf("completed=%v scrapped=%v", m.completed, m.scrapped)
			}
			if len(m.consumed) != 1 || len(m.consumed[0]) != 1 || m.consumed[0][0] != (mrp.Consumed{PartID: 171, Quantity: "0.2514"}) {
				t.Errorf("actuals = %+v", m.consumed)
			}
			if m.custom.AwaitingClearJob != 0 || m.custom.Run != nil || m.ws.Status != "idle" || len(pr.uploads) != 0 {
				t.Errorf("after answer: custom=%+v status=%s uploads=%v", m.custom, m.ws.Status, pr.uploads)
			}

			// And the printer is working again.
			p.Settings = m.custom
			if _, err := f.Tick(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			if len(pr.uploads) != 1 {
				t.Errorf("did not start the next bed: uploads=%v", pr.uploads)
			}
		})
	}
}

// A printer gone from the network must show in the journal even when
// the station is already offline and so nothing is written to the MRP —
// once, not every tick, and again when the reason changes.
func TestUnreachablePrinterIsLoggedOnce(t *testing.T) {
	m := &fakeMRP{ws: station()}
	off := autoTag + "unreachable: earlier"
	m.ws.Status, m.ws.StatusNote = "offline", &off
	pr := &fakePrinter{err: errors.New("dial tcp 192.0.2.7:80: connect: no route to host")}
	p := &Printer{WS: m.ws, Link: pr}
	var log strings.Builder
	f := newFarm(m)
	f.Log = slog.New(slog.NewTextHandler(&log, nil))

	for i := 0; i < 3; i++ {
		if _, err := f.Tick(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(log.String(), "printer unreachable"); n != 1 {
		t.Errorf("logged %d times, want 1:\n%s", n, log.String())
	}
	if len(m.statuses) != 0 {
		t.Errorf("wrote statuses over an offline station: %v", m.statuses)
	}

	// Back, but asking for an operator at the screen: a new reason, logged.
	pr.err, pr.state = nil, printer.Attention
	if _, err := f.Tick(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "printer reports ATTENTION") {
		t.Errorf("attention not logged:\n%s", log.String())
	}
}

// lockedMRP makes the fake safe to share between printer goroutines:
// one lock around every call, as the real MRP serialises nothing for us.
type lockedMRP struct {
	mu sync.Mutex
	m  *fakeMRP
}

func (l *lockedMRP) Workstations(ctx context.Context) ([]mrp.Workstation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.Workstations(ctx)
}
func (l *lockedMRP) Workstation(ctx context.Context, id int64) (*mrp.Workstation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.Workstation(ctx, id)
}
func (l *lockedMRP) SetStatus(ctx context.Context, id int64, status string, note *string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.SetStatus(ctx, id, status, note)
}
func (l *lockedMRP) Custom(ctx context.Context, kind string, id int64) (json.RawMessage, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.Custom(ctx, kind, id)
}
func (l *lockedMRP) SetCustom(ctx context.Context, kind string, id int64, v any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.SetCustom(ctx, kind, id, v)
}
func (l *lockedMRP) WorkOrders(ctx context.Context, status string) ([]mrp.WorkOrder, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.WorkOrders(ctx, status)
}
func (l *lockedMRP) WorkOrder(ctx context.Context, id int64) (*mrp.WorkOrder, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.WorkOrder(ctx, id)
}
func (l *lockedMRP) CompleteStep(ctx context.Context, stepID int64, qty string, c []mrp.Consumed) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.CompleteStep(ctx, stepID, qty, c)
}
func (l *lockedMRP) PauseStep(ctx context.Context, stepID int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.PauseStep(ctx, stepID)
}
func (l *lockedMRP) StartStep(ctx context.Context, stepID, wsID int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.StartStep(ctx, stepID, wsID)
}
func (l *lockedMRP) ScrapStep(ctx context.Context, stepID int64, qty string, c []mrp.Consumed, note string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.ScrapStep(ctx, stepID, qty, c, note)
}
func (l *lockedMRP) SetMaterial(ctx context.Context, id int64, partID *int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.SetMaterial(ctx, id, partID)
}
func (l *lockedMRP) ReplaceTasks(ctx context.Context, source string, t []mrp.Task) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.ReplaceTasks(ctx, source, t)
}
func (l *lockedMRP) TaskStates(ctx context.Context, source string) ([]mrp.TaskState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.TaskStates(ctx, source)
}
func (l *lockedMRP) Next(ctx context.Context, wsID int64, material *int64) (*mrp.Job, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.Next(ctx, wsID, material)
}

// stuckPrinter never answers until the daemon shuts down: a fifteen-
// minute upload, or a printer that times out on every call.
type stuckPrinter struct{}

func (stuckPrinter) Status(ctx context.Context) (*printer.Status, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (stuckPrinter) Upload(ctx context.Context, _ string, _ io.Reader, _ int64, _ bool) error {
	<-ctx.Done()
	return ctx.Err()
}

// countingPrinter is a fakePrinter that counts how often it is looked at.
type countingPrinter struct {
	*fakePrinter
	looks atomic.Int32
}

func (c *countingPrinter) Status(ctx context.Context) (*printer.Status, error) {
	c.looks.Add(1)
	return c.fakePrinter.Status(ctx)
}

// One printer stuck must not stop another being driven, and shutdown
// must wait for both goroutines rather than abandon them.
func TestStuckPrinterDoesNotHoldUpTheFarm(t *testing.T) {
	m := &lockedMRP{m: &fakeMRP{ws: station()}}
	quick := &countingPrinter{fakePrinter: &fakePrinter{state: printer.Printing, jobID: 7}}
	slow := station()
	slow.ID, slow.Code = 13, "CORE_ONEP_2"
	fleet := []*Printer{
		{WS: slow, Link: stuckPrinter{}},
		{WS: station(), Link: quick},
	}
	f := &Farm{MRP: m, Log: slog.New(slog.NewTextHandler(discard{}, nil)), Poll: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.runFleet(ctx, fleet)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for quick.looks.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := quick.looks.Load(); n < 3 {
		t.Errorf("the free printer was looked at %d times while the other was stuck", n)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runFleet did not return after shutdown")
	}
}

// Each printer publishes its own tasks; the MRP gets the union, in a
// stable order, and one printer's publish leaves the other's alone.
func TestTasksArePublishedAsTheUnion(t *testing.T) {
	m := &fakeMRP{ws: station()}
	f := newFarm(m)
	ctx := context.Background()
	f.publishTasks(ctx, "P2", []mrp.Task{{Key: "P2-bed-1"}})
	f.publishTasks(ctx, "P1", []mrp.Task{{Key: "P1-material-9"}})
	keys := func() (out []string) {
		for _, t := range m.tasks {
			out = append(out, t.Key)
		}
		return out
	}
	if got := strings.Join(keys(), ","); got != "P1-material-9,P2-bed-1" {
		t.Errorf("union = %s", got)
	}
	f.publishTasks(ctx, "P2", nil) // P2's bed answered
	if got := strings.Join(keys(), ","); got != "P1-material-9" {
		t.Errorf("after P2 cleared: %s", got)
	}
}
