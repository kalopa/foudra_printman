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

// Just enough implicit FTPS to put one file on a printer's storage:
// log in, protect the data channel, STOR in passive mode. The printer's
// server wants the data connection to resume the control connection's
// TLS session, so both share one session cache and one server name.

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

type ftpConn struct {
	c    net.Conn
	tp   *textproto.Conn
	host string
	tls  *tls.Config
}

// ftpsStore uploads body to name on the printer at addr ("host:990").
func ftpsStore(ctx context.Context, addr string, cfg *tls.Config, user, pass, name string, body io.Reader) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	d := tls.Dialer{Config: cfg}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("ftps: %w", err)
	}
	defer c.Close()
	// Cancelling ctx closes the connection, which ends any blocked read
	// or write; the error that follows is reported as ctx's.
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()

	f := &ftpConn{c: c, tp: textproto.NewConn(c), host: host, tls: cfg}
	err = f.store(ctx, user, pass, name, body)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (f *ftpConn) store(ctx context.Context, user, pass, name string, body io.Reader) error {
	if _, _, err := f.tp.ReadResponse(220); err != nil {
		return fmt.Errorf("ftps greeting: %w", err)
	}
	if code, msg, err := f.cmd("USER "+user, 0); err != nil {
		return err
	} else if code != 230 && code != 331 {
		return fmt.Errorf("ftps USER: %d %s", code, msg)
	} else if code == 331 {
		if code, msg, err := f.cmd("PASS "+pass, 0); err != nil {
			return err
		} else if code == 530 {
			return ErrUnauthorized
		} else if code != 230 {
			return fmt.Errorf("ftps PASS: %d %s", code, msg)
		}
	}
	for _, c := range []string{"PBSZ 0", "PROT P", "TYPE I"} {
		if _, _, err := f.cmd(c, 200); err != nil {
			return err
		}
	}
	_, msg, err := f.cmd("PASV", 227)
	if err != nil {
		return err
	}
	port, err := pasvPort(msg)
	if err != nil {
		return err
	}
	// The address in the reply is ignored: the data connection goes to
	// the host the control connection reached, whatever the printer
	// thinks its address is. TCP first, then STOR, and only after the
	// printer's 150 the TLS handshake: the server starts TLS on the data
	// connection once it has the command, so shaking hands any earlier
	// waits forever.
	var nd net.Dialer
	raw, err := nd.DialContext(ctx, "tcp", net.JoinHostPort(f.host, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("ftps data: %w", err)
	}
	defer raw.Close()
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()

	if err := f.tp.PrintfLine("STOR %s", name); err != nil {
		return err
	}
	if code, msg, err := f.tp.ReadResponse(1); err != nil {
		return fmt.Errorf("ftps STOR: %d %s: %w", code, msg, err)
	}
	dc := tls.Client(raw, f.tls)
	if err := dc.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("ftps data: %w", err)
	}
	if _, err := io.Copy(dc, bufio.NewReaderSize(body, 64<<10)); err != nil {
		return fmt.Errorf("ftps send: %w", err)
	}
	if err := dc.Close(); err != nil {
		return fmt.Errorf("ftps send: %w", err)
	}
	_ = f.c.SetReadDeadline(time.Now().Add(2 * time.Minute)) // the printer may sync to storage first
	if _, _, err := f.tp.ReadResponse(226); err != nil {
		return fmt.Errorf("ftps STOR: %w", err)
	}
	_, _, _ = f.cmd("QUIT", 0)
	return nil
}

// cmd sends a command and reads the reply. With want 0 any code is
// returned for the caller to judge (textproto checks nothing); otherwise
// a different code is an error.
func (f *ftpConn) cmd(line string, want int) (int, string, error) {
	if err := f.tp.PrintfLine("%s", line); err != nil {
		return 0, "", err
	}
	code, msg, err := f.tp.ReadResponse(want)
	if err != nil {
		verb, _, _ := strings.Cut(line, " ")
		return code, msg, fmt.Errorf("ftps %s: %w", verb, err)
	}
	return code, msg, nil
}

// pasvPort reads the port from "Entering Passive Mode (h1,h2,h3,h4,p1,p2)".
func pasvPort(msg string) (int, error) {
	i, j := strings.IndexByte(msg, '('), strings.IndexByte(msg, ')')
	if i < 0 || j < i {
		return 0, fmt.Errorf("ftps: unreadable PASV reply %q", msg)
	}
	parts := strings.Split(msg[i+1:j], ",")
	if len(parts) != 6 {
		return 0, fmt.Errorf("ftps: unreadable PASV reply %q", msg)
	}
	p1, err1 := strconv.Atoi(strings.TrimSpace(parts[4]))
	p2, err2 := strconv.Atoi(strings.TrimSpace(parts[5]))
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("ftps: unreadable PASV reply %q", msg)
	}
	return p1<<8 | p2, nil
}
