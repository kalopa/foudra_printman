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
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/kalopa/print_manager/internal/prusalink"
)

// printerCmd is `print-manager printer <status|info>`: read-only looks at
// one printer, for proving connectivity and for operators. There is
// deliberately no start or stop here — the daemon's loop is the only
// thing that should change a printer's state, and it does not exist yet.
func printerCmd(args []string) error {
	fs := flag.NewFlagSet("printer", flag.ContinueOnError)
	urlFlag := fs.String("url", os.Getenv("PRUSALINK_URL"), "printer address, e.g. http://192.168.1.7 (or $PRUSALINK_URL)")
	keyEnv := fs.String("key-env", "PRUSALINK_KEY", "environment variable holding the API key")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: print-manager printer [-url http://host] [-key-env VAR] status|info")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("printer: want one of status, info")
	}
	key := os.Getenv(*keyEnv)
	if key == "" {
		return fmt.Errorf("printer: $%s is not set", *keyEnv)
	}
	c, err := prusalink.New(*urlFlag, key)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	switch fs.Arg(0) {
	case "info":
		v, err := c.Version(ctx)
		if err != nil {
			return err
		}
		i, err := c.Info(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%s  firmware %s  PrusaLink %s (api %s)\n", i.Hostname, v.Firmware, v.Server, v.API)
		fmt.Printf("nozzle %.1f mm  mmu %v  serial %s  upload-by-put %v\n", i.NozzleDiameter, i.MMU, i.Serial, v.Capabilities.UploadByPut)
		return nil
	case "status":
		s, err := c.Status(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("state %s  nozzle %.0f/%.0f °C  bed %.0f/%.0f °C  z %.1f mm\n",
			s.Printer.State, s.Printer.TempNozzle, s.Printer.TargetNozzle, s.Printer.TempBed, s.Printer.TargetBed, s.Printer.AxisZ)
		if s.Job == nil {
			fmt.Println("no job")
			return nil
		}
		j, err := c.Job(ctx)
		if err != nil {
			return err
		}
		name := ""
		if j != nil {
			name = j.File.DisplayName
		}
		fmt.Printf("job %d  %s\n  %.0f%%  printed %s  remaining %s\n",
			s.Job.ID, name, s.Job.Progress, hms(s.Job.TimePrinting), hms(s.Job.TimeRemaining))
		return nil
	default:
		fs.Usage()
		return fmt.Errorf("printer: unknown action %q", fs.Arg(0))
	}
}

func hms(seconds int) string {
	d := time.Duration(seconds) * time.Second
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}
