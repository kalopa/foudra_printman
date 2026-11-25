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
// Package bambu drives a Bambu Lab printer in LAN mode with Developer
// Mode on: status from the printer's MQTT broker (TLS, port 8883),
// files over implicit FTPS (port 990), and prints started with an MQTT
// command. User "bblp", password the access code on the printer's
// screen. Without Developer Mode the printer still pushes status but
// refuses to start prints from anything but Bambu's own software.
package bambu

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	user      = "bblp"
	keepalive = 30 * time.Second
	// A printer pushes on every change, but an idle P1 may say nothing
	// for a long while. Past this, Status asks for everything again
	// (pushall is heavy for the printer, so not every poll).
	staleAfter = 5 * time.Minute
)

var ErrUnauthorized = errors.New("bambu: unauthorized (check the access code, and that LAN mode is on)")

var errTimeout = errors.New("timed out")

// Client is one printer. It keeps one MQTT connection open, merging the
// printer's pushes into a current report, and reconnects on the next
// call after the connection drops. Safe for concurrent use.
type Client struct {
	host   string
	// The printer's ports; fixed on a real printer, changed by tests.
	mqttPort, ftpsPort string
	serial             string
	code   string
	opts   Options
	tls    *tls.Config

	dial    sync.Mutex // one connect at a time
	mu      sync.Mutex
	conn    *mqttConn
	state   map[string]json.RawMessage // the merged "print" object
	last    time.Time                  // when the printer last pushed status
	lastErr error                      // why the last connection ended
	changed chan struct{}              // closed and replaced on each message
	replies map[string]json.RawMessage // command replies by sequence_id
	seq     int
}

// New makes a client for the printer at addr (a host or IP, optionally
// as a URL such as mqtts://192.168.1.50) with its serial number and
// access code. Nothing is dialled until the first call.
func New(addr, serial, accessCode string, opts Options) (*Client, error) {
	host := addr
	if strings.Contains(addr, "://") {
		u, err := url.Parse(addr)
		if err != nil {
			return nil, fmt.Errorf("bambu: bad address %q: %w", addr, err)
		}
		host = u.Hostname()
	} else if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if host == "" {
		return nil, fmt.Errorf("bambu: bad address %q", addr)
	}
	if serial == "" {
		return nil, errors.New("bambu: the printer's serial number is needed")
	}
	return &Client{
		host: host, mqttPort: "8883", ftpsPort: "990",
		serial: serial, code: accessCode, opts: opts,
		// The printer's certificate is signed by Bambu's own CA for a
		// name that is its serial number, not its address. It is not
		// checked: this is a LAN protocol with a shared password, and the
		// access code is the authentication that matters.
		tls: &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         host, // one name for FTPS session reuse
			ClientSessionCache: tls.NewLRUClientSessionCache(4),
		},
		changed: make(chan struct{}),
		replies: map[string]json.RawMessage{},
	}, nil
}

func (c *Client) reportTopic() string  { return "device/" + c.serial + "/report" }
func (c *Client) requestTopic() string { return "device/" + c.serial + "/request" }

// Report returns the printer's current status, connecting first if
// need be and waiting for the first full report on a new connection.
func (c *Client) Report(ctx context.Context) (*Report, error) {
	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	stale := time.Since(c.last) > staleAfter
	c.mu.Unlock()
	if stale {
		if err := c.pushall(); err != nil {
			return nil, err
		}
		if err := c.waitFor(ctx, 10*time.Second, func() bool { return time.Since(c.last) < time.Second*10 }); err != nil {
			c.drop(fmt.Errorf("no status from the printer: %w", err))
			return nil, c.dropped()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := json.Marshal(c.state)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("bambu: unreadable report: %w", err)
	}
	return &r, nil
}

// connect makes sure there is a live connection that has had a report.
func (c *Client) connect(ctx context.Context) error {
	c.dial.Lock()
	defer c.dial.Unlock()
	c.mu.Lock()
	if c.conn != nil {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(20 * time.Second)
	}
	d := tls.Dialer{NetDialer: &net.Dialer{Deadline: deadline}, Config: c.tls}
	nc, err := d.DialContext(ctx, "tcp", net.JoinHostPort(c.host, c.mqttPort))
	if err != nil {
		return fmt.Errorf("bambu: %w", err)
	}
	m, err := mqttConnect(nc, "foudra-printman-"+randHex(4), user, c.code, keepalive, deadline)
	if err != nil {
		nc.Close()
		if errors.Is(err, ErrUnauthorized) {
			return err
		}
		return fmt.Errorf("bambu: %w", err)
	}
	m.idle = 2*keepalive + 5*time.Second
	if err := m.subscribe(c.reportTopic()); err != nil {
		m.close()
		return fmt.Errorf("bambu: subscribe: %w", err)
	}

	c.mu.Lock()
	c.conn, c.state, c.last, c.lastErr = m, map[string]json.RawMessage{}, time.Time{}, nil
	c.mu.Unlock()
	done := make(chan struct{})
	go c.readLoop(m, done)
	go c.pingLoop(m, done)

	if err := c.pushall(); err != nil {
		return c.dropped()
	}
	if err := c.waitFor(ctx, time.Until(deadline), func() bool { return !c.last.IsZero() }); err != nil {
		c.drop(fmt.Errorf("no status after connecting: %w", err))
		return c.dropped()
	}
	return nil
}

func (c *Client) readLoop(m *mqttConn, done chan struct{}) {
	defer close(done)
	for {
		topic, payload, err := m.next()
		if err != nil {
			c.dropConn(m, err)
			return
		}
		if topic != c.reportTopic() {
			continue
		}
		c.handle(payload)
	}
}

func (c *Client) pingLoop(m *mqttConn, done chan struct{}) {
	t := time.NewTicker(keepalive)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if err := m.ping(); err != nil {
				c.dropConn(m, err)
				return
			}
		}
	}
}

// handle merges a status push, or files a command's reply under its
// sequence_id for whoever sent it.
func (c *Client) handle(payload []byte) {
	var msg struct {
		Print map[string]json.RawMessage `json:"print"`
	}
	if json.Unmarshal(payload, &msg) != nil || msg.Print == nil {
		return
	}
	var cmd, seq string
	_ = json.Unmarshal(msg.Print["command"], &cmd)
	_ = json.Unmarshal(msg.Print["sequence_id"], &seq)

	c.mu.Lock()
	defer c.mu.Unlock()
	if cmd == "push_status" {
		for k, v := range msg.Print {
			c.state[k] = v
		}
		c.last = time.Now()
	} else if seq != "" {
		c.replies[seq] = payload
	}
	close(c.changed)
	c.changed = make(chan struct{})
}

// waitFor waits until cond (called with c.mu held) holds.
func (c *Client) waitFor(ctx context.Context, d time.Duration, cond func() bool) error {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		c.mu.Lock()
		if c.conn == nil {
			c.mu.Unlock()
			return c.dropped()
		}
		ok, ch := cond(), c.changed
		c.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ch:
		case <-t.C:
			return errTimeout
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *Client) drop(err error) {
	c.mu.Lock()
	m := c.conn
	c.mu.Unlock()
	if m != nil {
		c.dropConn(m, err)
	}
}

// dropConn forgets m if it is still the connection, keeping why.
func (c *Client) dropConn(m *mqttConn, err error) {
	c.mu.Lock()
	if c.conn == m {
		c.conn, c.lastErr = nil, err
		close(c.changed)
		c.changed = make(chan struct{})
	}
	c.mu.Unlock()
	m.c.Close()
}

func (c *Client) dropped() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fmt.Errorf("bambu: connection lost: %w", cmp.Or(c.lastErr, io.ErrUnexpectedEOF))
}

// Close ends the connection; the next call makes a new one.
func (c *Client) Close() error {
	c.mu.Lock()
	m := c.conn
	c.conn = nil
	c.mu.Unlock()
	if m != nil {
		return m.close()
	}
	return nil
}

func (c *Client) pushall() error {
	return c.send(map[string]any{"pushing": map[string]any{"command": "pushall", "version": 1, "push_target": 1}}, "")
}

// send publishes a request. seq, when given, is the sequence_id the
// caller will wait on; it is set inside the request's only object.
func (c *Client) send(req map[string]any, seq string) error {
	for _, v := range req {
		if o, ok := v.(map[string]any); ok {
			o["sequence_id"] = cmp.Or(seq, "0")
		}
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	c.mu.Lock()
	m := c.conn
	c.mu.Unlock()
	if m == nil {
		return c.dropped()
	}
	if err := m.publish(c.requestTopic(), b); err != nil {
		c.dropConn(m, err)
		return c.dropped()
	}
	return nil
}

// command sends a print command and waits briefly for its reply. Not
// every firmware answers every command, so silence is not an error; an
// answer that says it failed is.
func (c *Client) command(ctx context.Context, body map[string]any) error {
	if err := c.connect(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	c.seq++
	seq := strconv.Itoa(20000 + c.seq)
	c.mu.Unlock()
	if err := c.send(map[string]any{"print": body}, seq); err != nil {
		return err
	}
	err := c.waitFor(ctx, 10*time.Second, func() bool { _, ok := c.replies[seq]; return ok })
	c.mu.Lock()
	raw, ok := c.replies[seq]
	delete(c.replies, seq)
	c.mu.Unlock()
	if !ok {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !errors.Is(err, errTimeout) {
			return err
		}
		return nil
	}
	var reply struct {
		Print struct {
			Result string `json:"result"`
			Reason string `json:"reason"`
		} `json:"print"`
	}
	_ = json.Unmarshal(raw, &reply)
	switch strings.ToLower(reply.Print.Result) {
	case "fail", "failed", "error":
		return fmt.Errorf("bambu: %s refused: %s", body["command"], cmp.Or(reply.Print.Reason, "no reason given"))
	}
	return nil
}

// Upload puts a file on the printer's storage over FTPS and, with
// printNow, starts it. A .3mf (as Bambu Studio and OrcaSlicer export a
// sliced plate) starts as a project; anything else as plain G-code.
func (c *Client) Upload(ctx context.Context, name string, body io.Reader, size int64, printNow bool) error {
	name = path.Base(name)
	if err := ftpsStore(ctx, net.JoinHostPort(c.host, c.ftpsPort), c.tls, user, c.code, name, body); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			return err
		}
		return fmt.Errorf("bambu: upload %s: %w", name, err)
	}
	if !printNow {
		return nil
	}
	return c.Print(ctx, name)
}

// Print starts a file already on the printer's storage.
func (c *Client) Print(ctx context.Context, name string) error {
	storage := cmp.Or(c.opts.Storage, "file:///sdcard/")
	if !strings.HasSuffix(strings.ToLower(name), ".3mf") {
		return c.command(ctx, map[string]any{
			"command": "gcode_file",
			"param":   strings.TrimPrefix(storage, "file://") + name,
		})
	}
	levelling := true
	if c.opts.BedLevelling != nil {
		levelling = *c.opts.BedLevelling
	}
	mapping := c.opts.AMSMapping
	if mapping == nil {
		mapping = []int{}
	}
	// A fresh id each time, in case the printer echoes it back: it then
	// tells a reprint of the same file from the last one.
	id := strconv.FormatInt(time.Now().Unix(), 10)
	return c.command(ctx, map[string]any{
		"command":        "project_file",
		"param":          fmt.Sprintf("Metadata/plate_%d.gcode", cmp.Or(c.opts.Plate, 1)),
		"url":            storage + name,
		"subtask_name":   strings.TrimSuffix(strings.TrimSuffix(name, ".3mf"), ".gcode"),
		"project_id":     "0",
		"profile_id":     "0",
		"task_id":        id,
		"subtask_id":     id,
		"md5":            "",
		"timelapse":      c.opts.Timelapse,
		"bed_type":       "auto",
		"bed_levelling":  levelling,
		"bed_leveling":   levelling, // both spellings are seen in the wild
		"flow_cali":      false,
		"vibration_cali": false,
		"layer_inspect":  false,
		"use_ams":        c.opts.UseAMS,
		"ams_mapping":    mapping,
	})
}

// Stop cancels the running job.
func (c *Client) Stop(ctx context.Context) error {
	return c.command(ctx, map[string]any{"command": "stop", "param": ""})
}

// Pause and Resume hold and continue the running job.
func (c *Client) Pause(ctx context.Context) error {
	return c.command(ctx, map[string]any{"command": "pause", "param": ""})
}

func (c *Client) Resume(ctx context.Context) error {
	return c.command(ctx, map[string]any{"command": "resume", "param": ""})
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
