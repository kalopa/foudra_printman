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
package octoprint

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kalopa/print_manager/internal/printer"
)

// TestLive drives a real OctoPrint through a print: upload and print,
// watch it run, see it finish, then upload again and see it idle. Skipped
// unless OCTOPRINT_URL and OCTOPRINT_KEY are set; meant for OctoPrint's
// virtual printer, since it really prints.
//
//	OCTOPRINT_URL=http://127.0.0.1:5000 OCTOPRINT_KEY=… go test -run TestLive -v ./internal/octoprint/
func TestLive(t *testing.T) {
	url, key := os.Getenv("OCTOPRINT_URL"), os.Getenv("OCTOPRINT_KEY")
	if url == "" || key == "" {
		t.Skip("OCTOPRINT_URL and OCTOPRINT_KEY not set")
	}
	c, err := New(url, key)
	if err != nil {
		t.Fatal(err)
	}
	d := Driver{C: c}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// About ten seconds of virtual printing.
	gcode := "G28\n" + strings.Repeat("G1 X10 Y10 F3000\nG4 S2\n", 5) + "M84\n"
	upload := func(print bool) {
		t.Helper()
		if err := d.Upload(ctx, "drv-live.gcode", strings.NewReader(gcode), int64(len(gcode)), print); err != nil {
			t.Fatal(err)
		}
	}
	// waitFor polls until the driver reports want, logging each change.
	waitFor := func(want printer.State) *printer.Status {
		t.Helper()
		var last printer.State
		for ctx.Err() == nil {
			s, err := d.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if s.State != last {
				t.Logf("%-9s %-12q job %+v", s.State, s.Raw, s.Job)
				last = s.State
			}
			if s.State == want {
				return s
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("never reached %s", want)
		return nil
	}

	// Whatever an earlier run left selected is replaced, and not printed.
	upload(false)
	waitFor(printer.Idle)
	upload(true)
	waitFor(printer.Printing)
	done := waitFor(printer.Finished)
	if done.Job == nil || done.Job.Progress != 100 {
		t.Fatalf("finished job = %+v", done.Job)
	}
	if again := waitFor(printer.Finished); again.Job.ID != done.Job.ID {
		t.Errorf("finished id moved: %d then %d", done.Job.ID, again.Job.ID)
	}

	// The bed is cleared and the next file goes up: idle again.
	upload(false)
	if s := waitFor(printer.Idle); s.Job != nil {
		t.Errorf("fresh upload reads as a job: %+v", s.Job)
	}

	// And a cancelled print reads as stopped. Cancelling is not part of
	// the driver, so ask OctoPrint directly.
	upload(true)
	waitFor(printer.Printing)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/job", strings.NewReader(`{"command":"cancel"}`))
	req.Header.Set("Content-Type", "application/json")
	if err := c.send(c.http, req, nil); err != nil {
		t.Fatal(err)
	}
	stopped := waitFor(printer.Stopped)
	if stopped.Job == nil || stopped.Job.ID == done.Job.ID {
		t.Errorf("stopped job = %+v (finished was %d)", stopped.Job, done.Job.ID)
	}
	upload(false) // leave it idle for the next run
}
