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
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kalopa/print_manager/internal/farm"
	"github.com/kalopa/print_manager/internal/mrp"
)

// mrpFromEnv builds the MRP client from MRP_URL and MRP_API_KEY.
func mrpFromEnv() (*mrp.Client, error) {
	u, k := os.Getenv("MRP_URL"), os.Getenv("MRP_API_KEY")
	if u == "" || k == "" {
		return nil, fmt.Errorf("MRP_URL and MRP_API_KEY must be set")
	}
	return mrp.New(u, k)
}

// printersCmd lists the fleet the daemon would drive, and why any
// candidate was skipped. Read-only.
func printersCmd(log *slog.Logger) error {
	m, err := mrpFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fleet, err := farm.Discover(ctx, m, log)
	if err != nil {
		return err
	}
	for _, p := range fleet {
		fmt.Printf("%-14s id %-3d %-28s %-9s %s  profile %s  window %s–%s\n", p.WS.Code, p.WS.ID, p.WS.Name,
			cmp.Or(p.Settings.Driver, "prusalink"), p.Settings.Address(), p.Settings.Profile, p.Settings.Window.Start, p.Settings.Window.End)
	}
	if len(fleet) == 0 {
		fmt.Println("no printers to drive")
	}
	return nil
}

// serveCmd runs the loop until SIGINT/SIGTERM.
func serveCmd(log *slog.Logger) error {
	m, err := mrpFromEnv()
	if err != nil {
		return err
	}
	poll := 15 * time.Second
	if v := os.Getenv("POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("POLL_INTERVAL: %w", err)
		}
		poll = d
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("print-manager starting", "version", version, "poll", poll)
	gdir := os.Getenv("GCODE_DIR")
	if gdir == "" {
		gdir = "/var/lib/print-manager/gcode"
	}
	f := &farm.Farm{MRP: m, Log: log, Poll: poll, Cfg: farm.Config{GCodeDir: gdir}}
	err = f.Run(ctx)
	log.Info("print-manager stopped")
	return err
}
