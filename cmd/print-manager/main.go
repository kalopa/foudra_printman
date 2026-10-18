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
// Command print-manager drives Prusa printers through PrusaLink and
// reports to the Foudra MRP. This file is subcommand dispatch and
// process lifecycle only; the logic lives in internal/.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"runtime"
)

// Set at link time by the Makefile (-X main.version=...). Vars, not
// consts, because the linker needs somewhere to write.
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "print-manager:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "help"
	if len(args) > 0 {
		cmd = args[0]
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	switch cmd {
	case "serve":
		return serveCmd(log)
	case "printers":
		return printersCmd(log)
	case "version", "-v", "--version":
		fmt.Printf("print-manager %s (commit %s, built %s, %s)\n", version, commit, buildTime, runtime.Version())
		return nil
	case "printer":
		return printerCmd(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: print-manager <command>

  serve      Run the daemon: discover printers from the MRP and keep it honest
  printers   List the printers the daemon would drive (read-only)
  printer    Read-only look at one printer: printer [-url U] [-key-env VAR] status|info
  version    Print build metadata
  help       This text
`)
}
