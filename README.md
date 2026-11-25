# Foudra Print Manager

[Foudra](https://foudra.com/) is a Manufacturing Resource Planning
(MRP II) system for managing the shop floor. It schedules purchase
orders and work orders to build what you want, when you want it.
It has an extensive API for adding third-party integrations, like this.

A daemon that drives 3D printers — Prusa printers over PrusaLink,
Bambu Lab printers in Developer Mode, and anything behind OctoPrint —
and keeps the Foudra MRP honest about what
they made and what they ate. Runs on a Raspberry Pi beside the printers.
Well, it runs on whatever hardware you have lying around that can access
the Foudra API over the Internet, and your printer(s).

One daemon drives the whole farm, each printer in a goroutine of its
own, so a slow upload to one never holds up the others.

## Installing on a Pi

First you'll need an API key on Foudra. Get one here:
https://yourcompany.foudra.com/api-keys.

A Pi Zero 2 W on the **64-bit** Raspberry Pi OS is plenty: the binary is
a few MB and idles at a few MB of memory. (`make build-pi32` for the
32-bit image.)

```sh
make build-pi && scp bin/pi/print-manager pi:/usr/local/bin/
ssh pi 'sudo useradd -r -s /usr/sbin/nologin print-manager; \
        sudo mkdir -p /etc/print-manager /var/lib/print-manager/gcode; \
        sudo chown -R print-manager /var/lib/print-manager'
scp deploy/print-manager.service pi:/tmp/ && ssh pi 'sudo mv /tmp/print-manager.service /etc/systemd/system/'
# /etc/print-manager/env (root:root 0600):
#   MRP_URL=https://yourcompany.foudra.com
#   MRP_API_KEY=<Your new API key - something like fdr_xxxxx>.
#   PRINTER_KEY_CORE_ONEP_1=<The private key for your printer>.
ssh pi 'sudo systemctl daemon-reload && sudo systemctl enable --now print-manager && journalctl -fu print-manager'
```

Copy sliced files and their sidecars into `/var/lib/print-manager/gcode/`.

## Building

```sh
make build        # for this machine, into ./bin
make build-pi     # linux/arm64 static binary, into ./bin/pi
make check        # fmt, vet, test
```

Copy `bin/pi/print-manager` to the Pi with the environment file that
`deploy/print-manager.service` expects.

## Configuration

The daemon needs two things from the environment and discovers the rest:

| Variable | Meaning |
|---|---|
| `MRP_URL` | `https://yourcompany.foudra.com` |
| `MRP_API_KEY` | A key made under the MRP's *API keys* page, named for the printer farm |
| `PRINTER_KEY_<CODE>` | One per printer, named by its workstation code upper-cased, other characters as `_`: `PRINTER_KEY_CORE_ONEP_1`. |
| `GCODE_DIR` | Where a part's G-code name resolves; default `/var/lib/print-manager/gcode` |
| `POLL_INTERVAL` | Default `15s` |

Which printers exist, and where, comes from the MRP: every **active,
automatic** workstation whose *Custom data* names a printer `url` is
driven. That JSON also says which driver to use, and carries the slicer
profile and the per-printer policy. A Prusa:

```json
{"driver": "prusalink", "url": "http://192.168.1.102", "profile": "core-one-0.4",
 "window": {"start": "08:00", "end": "22:00", "timezone": "Europe/Dublin", "must_finish_in_window": true},
 "filament_policy": {"change_for_priority_at_or_above": 1}}
```

A printer behind OctoPrint:

```json
{"driver": "octoprint", "url": "http://octopi.local", "profile": "mk3s-0.4",
 "window": {"start": "08:00", "end": "22:00", "timezone": "Europe/Dublin", "must_finish_in_window": true}}
```

A Bambu Lab printer:

```json
{"driver": "bambu", "url": "192.168.1.120", "serial": "01P00A000000001", "profile": "p2s-0.4",
 "bambu": {"use_ams": true, "ams_mapping": [0]}}
```

`driver` defaults to `prusalink`, and `prusalink_url` is still read in
place of `url`, so stations set up before there was a choice need no
change. `profile` is any name you like: it picks the G-code from the
part (below), so use one per printer type and nozzle. The `window` and
`filament_policy` blocks are optional.

The daemon also writes its own bookkeeping into this JSON
(`awaiting_clear_job`, `last_completed_job`, `run`). Leave those alone
while it is running.

### OctoPrint

- **The key:** in OctoPrint, *Settings → Application Keys* (or your
  user's API key under *User Settings*). Put it on the Pi as
  `PRINTER_KEY_<CODE>`.
- **G-code, not bgcode:** OctoPrint printers take plain `.gcode`, so
  give the part a G-code under the OctoPrint printer's profile:
  `{"gcode": {"mk3s-0.4": "bracket_PETG_MK3S.gcode"}}`.
- **Uploaded files are replaced.** Before each upload the daemon deletes
  any file of the same name in OctoPrint's local storage, then uploads
  afresh. The deletion clears the file's print history, which is how the
  daemon tells "a print has finished and its parts are on the bed" from
  "ready for a job" — OctoPrint itself just says *Operational* for both.
  Don't keep files you care about under the names the daemon uses.
- **No printer connected** (USB unplugged, printer off) reads as needing
  attention: the daemon logs it and starts nothing.
- **Testing without a printer:** OctoPrint's bundled virtual printer is
  enough. With an OctoPrint running and connected to it,

  ```sh
  export OCTOPRINT_URL=http://127.0.0.1:5000 OCTOPRINT_KEY=…
  go test -p 1 -run TestLive -v ./internal/octoprint/ ./internal/farm/
  ```

  prints, finishes, clears and cancels on it for real, and runs the
  farm loop through two beds. `-p 1` because both share the one printer.
  Without those variables the live tests are skipped.

### Bambu Lab

The daemon talks to Bambu printers the way Bambu Studio does on a LAN:
status from the printer's own MQTT broker (TLS, port 8883), files over
FTPS (port 990), prints started with an MQTT command. No cloud account,
no Bambu Connect, no SDK.

- **Developer Mode is required to print.** Bambu's firmware now refuses
  to start a print over the LAN from anything but its own software
  unless the printer is in *LAN Only* mode with *Developer Mode* on
  (see Bambu's [third-party integration](https://wiki.bambulab.com/en/software/third-party-integration)
  page). Status is still pushed without it, so a printer that isn't in
  Developer Mode shows up and is mirrored, but its prints fail to start
  and come back as an upload task.
- **What it costs.** Developer Mode keeps the printer off Bambu Cloud:
  no Bambu Handy, no cloud printing, no remote view from Bambu's apps.
  Bambu Studio and OrcaSlicer still work on the LAN. Anyone on that
  network with the access code has full control, so put the printers
  on their own VLAN or subnet next to the Pi.
- **The key** is the *access code* on the printer's screen (*Settings →
  LAN Only*), as `PRINTER_KEY_<CODE>`. It changes if LAN mode is toggled.
- **`serial`** is the printer's serial number (*Settings → Device*, or
  the label). It names the printer's MQTT topics, so it is required.
- **Files** are sliced `.gcode.3mf` from Bambu Studio or OrcaSlicer
  (*Export plate sliced file*), named under the Bambu printer's profile:
  `{"gcode": {"p2s-0.4": "bracket_PETG_P2S.gcode.3mf"}}`. The sidecar
  is `bracket_PETG_P2S.json` beside it. They go to the printer's storage
  (an SD card on most models) under the same name, replacing any file
  of that name.
- **`bambu` options**, all optional: `plate` (default 1), `use_ams` and
  `ams_mapping` (the file's filaments to AMS slots, in order; without
  `use_ams` the external spool feeds), `bed_levelling` (default true),
  `timelapse`, and `storage` — the URL prefix the printer opens uploads
  under, `file:///sdcard/` by default.
- **Checking a printer** without the MRP, read-only:

  ```sh
  BAMBU_CODE=12345678 print-manager printer -driver bambu -url 192.168.1.120 \
      -serial 01P00A000000001 -key-env BAMBU_CODE status
  ```

  This proves the address, serial and access code. It cannot prove
  Developer Mode: only starting a print does.

A printed part's G-code is named in **its** custom data, by profile:
`{"gcode": {"core-one-0.4": "bow_node-case_PP_COREONE.bgcode"}}` — a bare
name resolves under `GCODE_DIR`, a URL is fetched there once. The
sidecar `.json` that `scripts/slice.sh` writes beside it supplies
`per_bed`, `est_seconds` (for the window) and `filament_g` (the actual).
`slice.sh -n 4 part.stl` puts four copies on the bed (PrusaSlicer
arranges them), names the file `…_x4.bgcode` and writes `per_bed: 4`:
each print then completes four units and charges the whole bed's
filament to them. The render is still one part; the layout the slicer
chose is not known to Blender.

A printer key never goes in the MRP — it is full control of a hot
machine — so it lives only in `/etc/print-manager/env` on the Pi.
`print-manager printers` shows what would be driven and why anything
was skipped.
