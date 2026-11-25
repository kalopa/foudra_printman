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

// The smallest MQTT 3.1.1 client that talks to a Bambu printer: CONNECT
// with a username and password, one SUBSCRIBE, PUBLISH at QoS 0 both
// ways, and PINGREQ to keep the line up. The printer's broker needs no
// more, and a whole MQTT library for it would be the daemon's first
// dependency.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	pktConnect    = 1
	pktConnack    = 2
	pktPublish    = 3
	pktPuback     = 4
	pktSubscribe  = 8
	pktSuback     = 9
	pktPingreq    = 12
	pktPingresp   = 13
	pktDisconnect = 14
)

// mqttConn is one MQTT session over an established connection (TLS, for
// a printer). Writes are serialised; reads belong to one goroutine.
type mqttConn struct {
	c    net.Conn
	r    *bufio.Reader
	idle time.Duration // a read waiting longer than this fails; 0 waits forever
	mu   sync.Mutex    // guards writes
}

// mqttConnect sends CONNECT and waits for a CONNACK that accepts it.
func mqttConnect(c net.Conn, clientID, user, pass string, keepalive time.Duration, deadline time.Time) (*mqttConn, error) {
	m := &mqttConn{c: c, r: bufio.NewReader(c)}
	var b []byte
	b = appendString(b, "MQTT")
	b = append(b, 4)              // protocol level 3.1.1
	b = append(b, 0x80|0x40|0x02) // username, password, clean session
	b = binary.BigEndian.AppendUint16(b, uint16(keepalive/time.Second))
	b = appendString(b, clientID)
	b = appendString(b, user)
	b = appendString(b, pass)
	_ = c.SetDeadline(deadline)
	defer c.SetDeadline(time.Time{})
	if err := m.write(pktConnect<<4, b); err != nil {
		return nil, err
	}
	typ, body, err := m.read()
	if err != nil {
		return nil, fmt.Errorf("mqtt connack: %w", err)
	}
	if typ>>4 != pktConnack || len(body) < 2 {
		return nil, fmt.Errorf("mqtt: expected CONNACK, got packet type %d", typ>>4)
	}
	switch body[1] {
	case 0:
		return m, nil
	case 4, 5:
		return nil, ErrUnauthorized
	default:
		return nil, fmt.Errorf("mqtt: connection refused, code %d", body[1])
	}
}

// subscribe asks for one topic at QoS 0. The SUBACK arrives on the read
// loop, which ignores it: a refused subscription shows as no reports.
func (m *mqttConn) subscribe(topic string) error {
	b := binary.BigEndian.AppendUint16(nil, 1) // packet id
	b = appendString(b, topic)
	b = append(b, 0) // QoS 0
	return m.write(pktSubscribe<<4|0x02, b)
}

// publish sends at QoS 0.
func (m *mqttConn) publish(topic string, payload []byte) error {
	b := appendString(nil, topic)
	b = append(b, payload...)
	return m.write(pktPublish<<4, b)
}

func (m *mqttConn) ping() error { return m.write(pktPingreq<<4, nil) }

func (m *mqttConn) close() error {
	_ = m.write(pktDisconnect<<4, nil)
	return m.c.Close()
}

// next returns the next PUBLISH, skipping the packets a QoS 0 client can
// ignore (SUBACK, PINGRESP). A PUBLISH at QoS 1 is acknowledged, in case
// a broker ever sends one.
func (m *mqttConn) next() (topic string, payload []byte, err error) {
	for {
		typ, body, err := m.read()
		if err != nil {
			return "", nil, err
		}
		if typ>>4 != pktPublish {
			continue
		}
		qos := (typ >> 1) & 3
		if len(body) < 2 {
			return "", nil, errors.New("mqtt: short publish")
		}
		n := int(binary.BigEndian.Uint16(body))
		if len(body) < 2+n {
			return "", nil, errors.New("mqtt: short publish topic")
		}
		topic, rest := string(body[2:2+n]), body[2+n:]
		if qos > 0 {
			if len(rest) < 2 {
				return "", nil, errors.New("mqtt: short publish id")
			}
			if qos == 1 {
				_ = m.write(pktPuback<<4, rest[:2])
			}
			rest = rest[2:]
		}
		return topic, rest, nil
	}
}

// read returns one packet's fixed-header byte (type in the high nibble,
// flags in the low) and its body.
func (m *mqttConn) read() (byte, []byte, error) {
	if m.idle > 0 {
		_ = m.c.SetReadDeadline(time.Now().Add(m.idle))
	}
	h, err := m.r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	n, mult := 0, 1
	for i := 0; ; i++ {
		b, err := m.r.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		n += int(b&0x7f) * mult
		if b&0x80 == 0 {
			break
		}
		if i == 3 {
			return 0, nil, errors.New("mqtt: malformed remaining length")
		}
		mult *= 128
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(m.r, body); err != nil {
		return 0, nil, err
	}
	return h, body, nil
}

func (m *mqttConn) write(header byte, body []byte) error {
	pkt := []byte{header}
	n := len(body)
	for {
		b := byte(n % 128)
		n /= 128
		if n > 0 {
			b |= 0x80
		}
		pkt = append(pkt, b)
		if n == 0 {
			break
		}
	}
	pkt = append(pkt, body...)
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.c.Write(pkt)
	return err
}

func appendString(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint16(b, uint16(len(s)))
	return append(b, s...)
}
