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
package bambu

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/textproto"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kalopa/print_manager/internal/printer"
)

const (
	testSerial = "01P00A000000001"
	testCode   = "12345678"
)

// fakePrinter is a printer's two servers: an MQTT broker that answers
// pushall with the current fixture and records commands, and an
// implicit-FTPS server that keeps what is stored. Both on TLS with a
// throwaway certificate, as a real printer's is unverifiable too.
type fakePrinter struct {
	t       *testing.T
	tls     *tls.Config
	mqttLn  net.Listener
	ftpLn   net.Listener
	refuse  bool // answer print commands with result "failed"
	silent  bool // never answer print commands
	mu      sync.Mutex
	current []string // fixtures pushall sends, in order
	conns   []*mqttConn
	cmds    []map[string]any
	files   map[string][]byte
	resumed []bool // per FTPS data connection: did it resume the control session
}

func newFake(t *testing.T) *fakePrinter {
	t.Helper()
	f := &fakePrinter{t: t, tls: &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}},
		current: []string{"push_idle.json"}, files: map[string][]byte{}}
	var err error
	if f.mqttLn, err = tls.Listen("tcp", "127.0.0.1:0", f.tls); err != nil {
		t.Fatal(err)
	}
	if f.ftpLn, err = tls.Listen("tcp", "127.0.0.1:0", f.tls); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.mqttLn.Close(); f.ftpLn.Close(); f.dropAll() })
	go f.serve(f.mqttLn, f.broker)
	go f.serve(f.ftpLn, f.ftps)
	return f
}

func (f *fakePrinter) client(t *testing.T, code string, opts Options) *Client {
	t.Helper()
	c, err := New("mqtts://127.0.0.1", testSerial, code, opts)
	if err != nil {
		t.Fatal(err)
	}
	_, c.mqttPort, _ = net.SplitHostPort(f.mqttLn.Addr().String())
	_, c.ftpsPort, _ = net.SplitHostPort(f.ftpLn.Addr().String())
	t.Cleanup(func() { c.Close() })
	return c
}

func (f *fakePrinter) serve(ln net.Listener, h func(net.Conn)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go h(c)
	}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

// push sends a fixture to every connected client, and makes it part of
// what the next pushall sends.
func (f *fakePrinter) push(name string) {
	f.mu.Lock()
	f.current = append(f.current, name)
	conns := append([]*mqttConn(nil), f.conns...)
	f.mu.Unlock()
	for _, m := range conns {
		_ = m.publish("device/"+testSerial+"/report", fixture(f.t, name))
	}
}

func (f *fakePrinter) dropAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.conns {
		m.c.Close()
	}
	f.conns = nil
}

func (f *fakePrinter) broker(nc net.Conn) {
	defer nc.Close()
	m := &mqttConn{c: nc, r: bufio.NewReader(nc)}
	typ, body, err := m.read()
	if err != nil || typ>>4 != pktConnect {
		return
	}
	_, pass := connectCreds(body)
	if pass != testCode {
		_ = m.write(pktConnack<<4, []byte{0, 5})
		return
	}
	_ = m.write(pktConnack<<4, []byte{0, 0})
	f.mu.Lock()
	f.conns = append(f.conns, m)
	f.mu.Unlock()
	for {
		typ, body, err := m.read()
		if err != nil {
			return
		}
		switch typ >> 4 {
		case pktSubscribe:
			_ = m.write(pktSuback<<4, []byte{body[0], body[1], 0})
		case pktPingreq:
			_ = m.write(pktPingresp<<4, nil)
		case pktPublish:
			n := int(binary.BigEndian.Uint16(body))
			if string(body[2:2+n]) != "device/"+testSerial+"/request" {
				f.t.Errorf("publish to %q", body[2:2+n])
				continue
			}
			f.request(m, body[2+n:])
		}
	}
}

func (f *fakePrinter) request(m *mqttConn, payload []byte) {
	var req map[string]map[string]any
	if err := json.Unmarshal(payload, &req); err != nil {
		f.t.Errorf("request not JSON: %s", payload)
		return
	}
	topic := "device/" + testSerial + "/report"
	if p, ok := req["pushing"]; ok && p["command"] == "pushall" {
		f.mu.Lock()
		cur := append([]string(nil), f.current...)
		f.mu.Unlock()
		for _, name := range cur {
			_ = m.publish(topic, fixture(f.t, name))
		}
		return
	}
	p := req["print"]
	f.mu.Lock()
	f.cmds = append(f.cmds, p)
	refuse, silent := f.refuse, f.silent
	f.mu.Unlock()
	if silent {
		return
	}
	result := "success"
	if refuse {
		result = "failed"
	}
	reply, _ := json.Marshal(map[string]any{"print": map[string]any{
		"command": p["command"], "sequence_id": p["sequence_id"], "result": result, "reason": "sd card not ready"}})
	_ = m.publish(topic, reply)
}

// connectCreds reads the username and password from a CONNECT body.
func connectCreds(b []byte) (user, pass string) {
	str := func() string {
		n := int(binary.BigEndian.Uint16(b))
		s := string(b[2 : 2+n])
		b = b[2+n:]
		return s
	}
	str()      // "MQTT"
	b = b[4:]  // level, flags, keepalive
	str()      // client id
	return str(), str()
}

func (f *fakePrinter) ftps(nc net.Conn) {
	defer nc.Close()
	tp := textproto.NewConn(nc)
	reply := func(code int, msg string) { _ = tp.PrintfLine("%d %s", code, msg) }
	reply(220, "Bambu FTP ready")
	var data net.Listener
	authed := false
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		switch {
		case verb == "USER":
			reply(331, "password please")
		case verb == "PASS":
			if arg != testCode {
				reply(530, "Login incorrect.")
				continue
			}
			authed = true
			reply(230, "Login successful.")
		case !authed:
			reply(530, "Please login.")
		case verb == "PBSZ" || verb == "PROT" || verb == "TYPE":
			reply(200, "ok")
		case verb == "PASV":
			if data, err = tls.Listen("tcp", "127.0.0.1:0", f.tls); err != nil {
				reply(425, err.Error())
				continue
			}
			p := data.Addr().(*net.TCPAddr).Port
			// A real printer may name an address the client can't reach;
			// the client must use the control connection's host.
			reply(227, fmt.Sprintf("Entering Passive Mode (10,9,8,7,%d,%d).", p>>8, p&0xff))
		case verb == "STOR":
			reply(150, "Ok to send data.")
			dc, err := data.Accept()
			data.Close()
			if err != nil {
				return
			}
			b, _ := io.ReadAll(dc)
			resumed := dc.(*tls.Conn).ConnectionState().DidResume
			dc.Close()
			f.mu.Lock()
			f.files[arg] = b
			f.resumed = append(f.resumed, resumed)
			f.mu.Unlock()
			reply(226, "Transfer complete.")
		case verb == "QUIT":
			reply(221, "Goodbye.")
			return
		default:
			reply(502, "not implemented")
		}
	}
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: testSerial},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
}

// waitState polls the driver until it reports want.
func waitState(t *testing.T, d Driver, want printer.State) *printer.Status {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		s, err := d.Status(ctx)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if s.State == want {
			return s
		}
		select {
		case <-ctx.Done():
			t.Fatalf("still %s (%s), want %s", s.State, s.Raw, want)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestStatusThroughAJob(t *testing.T) {
	f := newFake(t)
	d := Driver{C: f.client(t, testCode, Options{})}

	s := waitState(t, d, printer.Idle)
	if s.Job != nil {
		t.Errorf("idle printer reports job %+v", s.Job)
	}

	f.push("push_running.json")
	s = waitState(t, d, printer.Printing)
	if s.Job == nil || s.Job.File != "bracket_PETG_P2S" || s.Job.Progress != 37 || s.Job.TimeRemaining != 42*60 {
		t.Fatalf("running job = %+v", s.Job)
	}
	running := s.Job.ID

	// A P1 pushes only what changed: the rest must be remembered.
	f.push("push_delta.json")
	ctx := context.Background()
	for i := 0; i < 500; i++ {
		if s, _ = d.Status(ctx); s.Job != nil && s.Job.Progress == 38 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s.State != printer.Printing || s.Job.Progress != 38 || s.Job.File != "bracket_PETG_P2S" || s.Job.ID != running {
		t.Fatalf("after delta: %s %+v", s.State, s.Job)
	}

	f.push("push_finish.json")
	s = waitState(t, d, printer.Finished)
	if s.Job == nil || s.Job.ID != running {
		t.Errorf("finished job %+v, want id %d as while running", s.Job, running)
	}

	f.push("push_cancelled.json")
	s = waitState(t, d, printer.Stopped)
	if s.Raw != "FAILED" {
		t.Errorf("cancelled raw = %q", s.Raw)
	}

	f.push("push_fault.json")
	s = waitState(t, d, printer.Error)
	if !strings.Contains(s.Raw, "03008015") {
		t.Errorf("fault raw = %q, want the error code", s.Raw)
	}
}

func TestUploadAndPrint(t *testing.T) {
	f := newFake(t)
	no := false
	c := f.client(t, testCode, Options{Plate: 2, UseAMS: true, AMSMapping: []int{1, 0}, BedLevelling: &no})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	body := "PK\x03\x04 pretend 3mf"
	if err := c.Upload(ctx, "/some/dir/bracket_PETG_P2S.gcode.3mf", strings.NewReader(body), int64(len(body)), true); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := string(f.files["bracket_PETG_P2S.gcode.3mf"]); got != body {
		t.Errorf("stored %q", got)
	}
	if len(f.resumed) != 1 || !f.resumed[0] {
		t.Errorf("data connection resumed the TLS session: %v; a printer refuses one that doesn't", f.resumed)
	}
	if len(f.cmds) != 1 {
		t.Fatalf("commands %v", f.cmds)
	}
	p := f.cmds[0]
	for k, want := range map[string]any{
		"command":       "project_file",
		"url":           "file:///sdcard/bracket_PETG_P2S.gcode.3mf",
		"param":         "Metadata/plate_2.gcode",
		"subtask_name":  "bracket_PETG_P2S",
		"use_ams":       true,
		"bed_levelling": false,
	} {
		if p[k] != want {
			t.Errorf("%s = %v, want %v", k, p[k], want)
		}
	}
	if m, _ := json.Marshal(p["ams_mapping"]); string(m) != "[1,0]" {
		t.Errorf("ams_mapping = %s", m)
	}
}

func TestUploadOnly(t *testing.T) {
	f := newFake(t)
	c := f.client(t, testCode, Options{})
	if err := c.Upload(context.Background(), "a.gcode.3mf", strings.NewReader("x"), 1, false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cmds) != 0 {
		t.Errorf("printNow false still sent %v", f.cmds)
	}
}

func TestPlainGCode(t *testing.T) {
	f := newFake(t)
	c := f.client(t, testCode, Options{})
	if err := c.Upload(context.Background(), "cube.gcode", strings.NewReader("G28\n"), 4, true); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cmds) != 1 || f.cmds[0]["command"] != "gcode_file" || f.cmds[0]["param"] != "/sdcard/cube.gcode" {
		t.Errorf("commands %v", f.cmds)
	}
}

func TestPrintRefused(t *testing.T) {
	f := newFake(t)
	f.refuse = true
	c := f.client(t, testCode, Options{})
	err := c.Print(context.Background(), "a.3mf")
	if err == nil || !strings.Contains(err.Error(), "sd card not ready") {
		t.Errorf("err = %v, want the printer's refusal", err)
	}
}

func TestUnauthorized(t *testing.T) {
	f := newFake(t)
	c := f.client(t, "wrong", Options{})
	if _, err := c.Report(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("mqtt: err = %v, want ErrUnauthorized", err)
	}
	if err := c.Upload(context.Background(), "a.3mf", strings.NewReader("x"), 1, false); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("ftps: err = %v, want ErrUnauthorized", err)
	}
}

func TestReconnects(t *testing.T) {
	f := newFake(t)
	d := Driver{C: f.client(t, testCode, Options{})}
	waitState(t, d, printer.Idle)

	f.dropAll() // the printer rebooted, or the Wi-Fi blinked
	f.push("push_running.json")
	// The first look after the drop may still see the old connection;
	// soon after, a new one is made and reads the current state.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s, err := d.Status(context.Background())
		if err == nil && s.State == printer.Printing {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("never reconnected")
}

func TestUnreachable(t *testing.T) {
	c, err := New("127.0.0.1", testSerial, testCode, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	_, c.mqttPort, _ = net.SplitHostPort(ln.Addr().String())
	ln.Close() // nothing listening there now
	if _, err := (Driver{C: c}).Status(context.Background()); err == nil {
		t.Error("status from nowhere")
	}
}

func TestJobIDTellsReprintsApart(t *testing.T) {
	a := &Report{SubtaskName: "cube", GCodeStartTime: "1795000000"}
	b := &Report{SubtaskName: "cube", GCodeStartTime: "1795009999"}
	if jobID(a) == jobID(b) {
		t.Error("a reprint of the same file has the same job id")
	}
	if jobID(a) != jobID(&Report{SubtaskName: "cube", GCodeStartTime: "1795000000"}) {
		t.Error("job id not stable")
	}
}

func TestFlexString(t *testing.T) {
	var r Report
	if err := json.Unmarshal([]byte(`{"task_id":123,"subtask_id":"456","job_id":null}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.TaskID != "123" || r.SubtaskID != "456" || r.JobID != "" {
		t.Errorf("%+v", r)
	}
}

func TestNewAddress(t *testing.T) {
	for _, a := range []string{"192.168.1.50", "mqtts://192.168.1.50", "192.168.1.50:8883", "bambu://192.168.1.50/"} {
		c, err := New(a, testSerial, testCode, Options{})
		if err != nil || c.host != "192.168.1.50" {
			t.Errorf("New(%q) host %q err %v", a, c.host, err)
		}
	}
	if _, err := New("192.168.1.50", "", testCode, Options{}); err == nil {
		t.Error("no serial accepted")
	}
}
