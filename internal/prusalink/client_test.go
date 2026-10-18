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
package prusalink

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kalopa/print_manager/internal/printer"
)

// fake serves the JSON recorded from a real Core One (testdata/, taken
// 2026-09-27 on firmware 6.8.1) and records the writes it receives, so
// the client is tested against the printer's real shapes without a
// printer — and without ever stopping a real job.
type fake struct {
	t       *testing.T
	key     string
	idle    bool
	uploads []string
	stops   []string
}

func (f *fake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != f.key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		serve := func(name string) {
			b, err := os.ReadFile("testdata/" + name)
			if err != nil {
				f.t.Fatalf("fixture %s: %v", name, err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/version":
			serve("version.json")
		case r.Method == "GET" && r.URL.Path == "/api/v1/info":
			serve("v1_info.json")
		case r.Method == "GET" && r.URL.Path == "/api/v1/status":
			serve("v1_status.json")
		case r.Method == "GET" && r.URL.Path == "/api/v1/job":
			if f.idle {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			serve("v1_job.json")
		case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/api/v1/files/usb/"):
			body, _ := io.ReadAll(r.Body)
			if r.ContentLength < 0 || len(r.TransferEncoding) > 0 {
				// What the real printer does with a chunked body.
				http.Error(w, "411: Length Required", http.StatusLengthRequired)
				return
			}
			f.uploads = append(f.uploads, r.URL.Path+"|"+r.Header.Get("Print-After-Upload")+"|"+string(body))
			w.WriteHeader(http.StatusCreated)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/stop"):
			f.stops = append(f.stops, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "no such endpoint", http.StatusNotFound)
		}
	})
}

func newTestClient(t *testing.T, f *fake) *Client {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, f.key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestReadsDecodeTheRecordedPrinter(t *testing.T) {
	f := &fake{t: t, key: "k"}
	c := newTestClient(t, f)
	ctx := context.Background()

	v, err := c.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v.Firmware != "6.8.1+16182" || !v.Capabilities.UploadByPut || v.NozzleDiameter != 0.4 {
		t.Errorf("version = %+v", v)
	}

	i, err := c.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if i.Hostname != "prusa-core-one" || i.MMU {
		t.Errorf("info = %+v", i)
	}

	s, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture is a snapshot of a running print: assert what is
	// stable about it (state, job id, that progress and times decoded
	// as numbers), not the percentage it happened to be at.
	if s.Printer.State != StatePrinting || s.Job == nil || s.Job.ID != 141 ||
		s.Job.Progress <= 0 || s.Job.Progress > 100 || s.Job.TimeRemaining <= 0 || s.Job.TimePrinting <= 0 {
		t.Errorf("status = %+v job=%+v", s.Printer, s.Job)
	}
	if s.Printer.TargetNozzle != 250 || s.Printer.TargetBed != 85 {
		t.Errorf("temps = %+v", s.Printer)
	}

	j, err := c.Job(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil || j.ID != 141 || j.File.DisplayName != "seg0F_0.4n_0.28mm_PETG_COREONE_5h43m.bgcode" || j.File.Name != "SEG0F_~2.BGC" {
		t.Errorf("job = %+v", j)
	}
}

func TestIdlePrinterHasNoJob(t *testing.T) {
	f := &fake{t: t, key: "k", idle: true}
	c := newTestClient(t, f)
	j, err := c.Job(context.Background())
	if err != nil || j != nil {
		t.Errorf("idle job = %+v, %v; want nil, nil", j, err)
	}
}

func TestWrongKeyIsUnauthorized(t *testing.T) {
	f := &fake{t: t, key: "right"}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c, _ := New(srv.URL, "wrong")
	_, err := c.Status(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("got %v, want ErrUnauthorized", err)
	}
}

func TestOtherErrorsCarryTheStatus(t *testing.T) {
	f := &fake{t: t, key: "k"}
	c := newTestClient(t, f)
	_, err := c.do(context.Background(), "GET", "/api/v1/nope", nil, nil, nil)
	var perr *Error
	if !errors.As(err, &perr) || perr.Status != 404 {
		t.Errorf("got %v, want *Error with 404", err)
	}
}

// The two writes, against the fake only. A real printer is never
// stopped by a test.
func TestUploadAndStopHitTheRightEndpoints(t *testing.T) {
	f := &fake{t: t, key: "k"}
	c := newTestClient(t, f)
	ctx := context.Background()

	// An io.Pipe has no length Go can discover, exactly like a file:
	// the length must reach the wire from the size argument.
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte("GCODE")); pw.Close() }()
	if err := c.Upload(ctx, "rib-03.bgcode", pr, 5, true); err != nil {
		t.Fatal(err)
	}
	if len(f.uploads) != 1 || f.uploads[0] != "/api/v1/files/usb/rib-03.bgcode|?1|GCODE" {
		t.Errorf("uploads = %v", f.uploads)
	}
	if err := c.Upload(ctx, "../evil", strings.NewReader(""), 0, false); err == nil {
		t.Error("a name with a slash must be refused before it reaches the printer")
	}
	if err := c.Stop(ctx, 141); err != nil {
		t.Fatal(err)
	}
	if len(f.stops) != 1 || f.stops[0] != "/api/v1/job/141/stop" {
		t.Errorf("stops = %v", f.stops)
	}
}

func TestNewRejectsBadInputs(t *testing.T) {
	for _, u := range []string{"", "192.168.1.7", "http://"} {
		if _, err := New(u, "k"); err == nil {
			t.Errorf("New(%q) accepted", u)
		}
	}
	if _, err := New("http://192.168.1.7", ""); err == nil {
		t.Error("empty key accepted")
	}
}

// The driver against the recorded fixtures: a printing Core One reads
// as printing, with its job and the slicer's file name.
func TestDriverStatus(t *testing.T) {
	d := Driver{C: newTestClient(t, &fake{t: t, key: "k"})}
	s, err := d.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.State != printer.Printing || s.Raw != "PRINTING" || s.Job == nil || s.Job.ID == 0 || s.Job.File == "" {
		t.Errorf("status = %+v, job = %+v", s, s.Job)
	}
}

// Every state the client names has a place in the daemon's vocabulary.
// (Driver.Status sends anything else to Attention.)
func TestDriverStateMapping(t *testing.T) {
	for _, s := range []State{StateIdle, StateReady, StateBusy, StatePrinting, StatePaused,
		StateFinished, StateStopped, StateError, StateAttention} {
		if _, ok := states[s]; !ok {
			t.Errorf("%s has no daemon state", s)
		}
	}
}
