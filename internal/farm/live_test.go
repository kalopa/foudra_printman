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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kalopa/print_manager/internal/mrp"
	"github.com/kalopa/print_manager/internal/octoprint"
	"github.com/kalopa/print_manager/internal/printer"
)

// uploadCounter wraps a real driver to count what the farm sends it.
type uploadCounter struct {
	printer.Driver
	n int
}

func (u *uploadCounter) Upload(ctx context.Context, name string, body io.Reader, size int64, printNow bool) error {
	u.n++
	return u.Driver.Upload(ctx, name, body, size, printNow)
}

// TestLiveOctoPrintFarm runs the farm loop, against a fake MRP, on a
// real OctoPrint: a two-bed order prints its first bed, waits with the
// parts on the bed however long nobody answers, and prints the second
// only after "Bed clear, parts good". Skipped unless OCTOPRINT_URL and
// OCTOPRINT_KEY are set; meant for OctoPrint's virtual printer.
func TestLiveOctoPrintFarm(t *testing.T) {
	url, key := os.Getenv("OCTOPRINT_URL"), os.Getenv("OCTOPRINT_KEY")
	if url == "" || key == "" {
		t.Skip("OCTOPRINT_URL and OCTOPRINT_KEY not set")
	}
	c, err := octoprint.New(url, key)
	if err != nil {
		t.Fatal(err)
	}
	link := &uploadCounter{Driver: octoprint.Driver{C: c}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	dir := t.TempDir()
	gcode := "G28\n" + strings.Repeat("G1 X10 Y10 F3000\nG4 S2\n", 4) + "M84\n"
	os.WriteFile(filepath.Join(dir, "farm-live.gcode"), []byte(gcode), 0o644)
	os.WriteFile(filepath.Join(dir, "farm-live.json"), []byte(`{"per_bed": 1, "est_seconds": 10, "filament_g": 3}`), 0o644)

	m := &fakeMRP{ws: station(), partCustom: `{"gcode": {"mk3s-0.4": "farm-live.gcode"}}`}
	job := &mrp.Job{StepID: 51, Name: "Print"}
	job.WorkOrder.Number, job.WorkOrder.Quantity = "WO-00051", "2"
	job.Part.ID, job.Part.Name = 920, "Live test part"
	m.next = job
	m.order = mrp.WorkOrder{ID: 51, Number: "WO-00051", Status: "in_progress", Quantity: "2"}
	m.order.Part.ID = 920
	m.step = &mrp.Step{ID: 51, Name: "Print", Status: "pending", QuantityDone: "0"}
	m.custom = Settings{Driver: "octoprint", URL: url, Profile: "mk3s-0.4"}
	p := &Printer{WS: m.ws, Link: link, Settings: m.custom}
	f := newFarm(m)
	f.Cfg.GCodeDir = dir

	tick := func() []mrp.Task {
		t.Helper()
		f.refreshOutcomes(ctx)
		p.Settings = m.custom
		tasks, err := f.Tick(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return tasks
	}
	// untilTask ticks until the farm raises a task, as it does for a bed.
	untilTask := func() mrp.Task {
		t.Helper()
		for ctx.Err() == nil {
			if ts := tick(); len(ts) == 1 {
				return ts[0]
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatal("no task raised")
		return mrp.Task{}
	}

	// Leave OctoPrint idle: whatever the last test left selected is
	// replaced, and not printed.
	if err := link.Driver.Upload(ctx, "farm-live.gcode", strings.NewReader(gcode), int64(len(gcode)), false); err != nil {
		t.Fatal(err)
	}

	tick() // idle: claims the step and sends bed 1
	if link.n != 1 || m.ws.Status != "busy" {
		t.Fatalf("bed 1 not started: uploads=%d status=%s note=%v", link.n, m.ws.Status, deref(m.ws.StatusNote))
	}
	bed := untilTask()
	t.Logf("bed 1 done: task %s, status %s", bed.Key, m.ws.Status)
	if m.ws.Status != "blocked" || !strings.Contains(bed.Message, "clear the bed") {
		t.Fatalf("finished bed: status=%s task=%+v", m.ws.Status, bed)
	}

	// Nobody comes for a while. OctoPrint says "Operational" throughout;
	// nothing may be printed on top of the parts.
	for i := 0; i < 5; i++ {
		tick()
		time.Sleep(200 * time.Millisecond)
	}
	if link.n != 1 || len(m.completed) != 0 {
		t.Fatalf("acted with parts on the bed: uploads=%d completed=%v", link.n, m.completed)
	}

	// Bed clear, parts good: bed 1 completes, then bed 2 starts.
	m.outcomes = map[string]string{bed.Key: "ready"}
	tick()
	if len(m.completed) != 1 || m.completed[0] != "1" || len(m.paused) != 1 {
		t.Fatalf("after ready: completed=%v paused=%v", m.completed, m.paused)
	}
	m.step.QuantityDone = "1" // what the MRP records on completion
	tick()
	if link.n != 2 {
		t.Fatalf("bed 2 not started: uploads=%d status=%s", link.n, m.ws.Status)
	}
	bed2 := untilTask()
	if bed2.Key == bed.Key {
		t.Errorf("bed 2 reused bed 1's task key %s", bed.Key)
	}
	t.Logf("bed 2 done: task %s", bed2.Key)

	// Leave OctoPrint idle for whoever runs next.
	_ = link.Driver.Upload(ctx, "farm-live.gcode", strings.NewReader(gcode), int64(len(gcode)), false)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
