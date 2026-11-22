// Package proto defines what CatConnect servers and clients exchange: the
// share link, and the messages on the control connection that tells a
// client which game servers it can reach.
//
// Every game port is forwarded 1:1: a service on TCP 7777 at the server is
// TCP 7777 inside the tunnel and 127.0.0.1:7777 on the client. Keeping the
// numbers identical matters for games such as Satisfactory, whose server
// tells clients to come back on a second port.
package proto

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/Sniphs98/CatConnect/internal/presets"
	"github.com/tailscale/tailcat"
)

// Version is the control protocol version.
const Version = 1

// ControlPort is the TCP port inside the tunnel for the control connection.
// Services may not use it.
const ControlPort = 61000

// KeepAlive is how often the server re-sends the manifest on an idle
// control connection; clients treat 3×KeepAlive of silence as a lost
// connection (a blocked or vanished peer never sends a FIN).
const KeepAlive = 10 * time.Second

// LinkScheme is the URL scheme of share links.
const LinkScheme = "catconnect"

// Service is one game server as clients see it.
type Service struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Preset string   `json:"preset,omitempty"`
	TCP    []string `json:"tcp"`
	UDP    []string `json:"udp"`
	Join   string   `json:"join,omitempty"` // in-game instructions, {addr}/{port} filled in by the client
}

// Ranges parses the service's port ranges.
func (s Service) Ranges() (tcp, udp []presets.Range, err error) {
	for _, r := range s.TCP {
		pr, err := presets.ParseRange(r)
		if err != nil {
			return nil, nil, err
		}
		tcp = append(tcp, pr)
	}
	for _, r := range s.UDP {
		pr, err := presets.ParseRange(r)
		if err != nil {
			return nil, nil, err
		}
		udp = append(udp, pr)
	}
	return tcp, udp, nil
}

// FirstPort is the port players type into the game: the first TCP port,
// else the first UDP port.
func (s Service) FirstPort() uint16 {
	tcp, udp, err := s.Ranges()
	if err != nil {
		return 0
	}
	if len(tcp) > 0 {
		return tcp[0].First
	}
	if len(udp) > 0 {
		return udp[0].First
	}
	return 0
}

// Hello is the first message a client sends on the control connection.
type Hello struct {
	V    int    `json:"v"`
	Name string `json:"name"` // the player's display name
}

// Manifest is what the server sends after Hello, and again whenever its
// services change.
type Manifest struct {
	V        int       `json:"v"`
	Server   string    `json:"server"` // display name of the server
	Services []Service `json:"services"`
}

// ValidateServices checks that services have valid ports and that no
// protocol/port pair is used twice.
func ValidateServices(services []Service) error {
	seen := map[string]string{}
	for _, s := range services {
		tcp, udp, err := s.Ranges()
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		if len(tcp)+len(udp) == 0 {
			return fmt.Errorf("%s: no ports", s.Name)
		}
		check := func(proto string, rs []presets.Range) error {
			for _, r := range rs {
				for _, p := range r.Ports() {
					if proto == "TCP" && p == ControlPort {
						return fmt.Errorf("%s: TCP %d is reserved for CatConnect", s.Name, p)
					}
					k := fmt.Sprintf("%s %d", proto, p)
					if other, dup := seen[k]; dup {
						return fmt.Errorf("%s %d is used by both %s and %s", proto, p, other, s.Name)
					}
					seen[k] = s.Name
				}
			}
			return nil
		}
		if err := check("TCP", tcp); err != nil {
			return err
		}
		if err := check("UDP", udp); err != nil {
			return err
		}
	}
	return nil
}

// MakeLink turns a server's tailcat address into a share link. The address
// goes in the path: it is case-sensitive, and URL handlers may lowercase
// hostnames.
func MakeLink(addr tailcat.Addr) string {
	return LinkScheme + "://join/" + string(addr)
}

// ParseLink accepts a share link (with or without a trailing slash, as chat
// apps sometimes add) or a bare tailcat address.
func ParseLink(s string) (tailcat.Addr, error) {
	s = strings.Trim(strings.TrimSpace(s), "<>\"'")
	if s == "" {
		return "", errors.New("no link given")
	}
	addr := s
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || !strings.EqualFold(u.Scheme, LinkScheme) {
			return "", errors.New("that is not a CatConnect link")
		}
		addr = strings.Trim(u.Path, "/")
		if !strings.EqualFold(u.Host, "join") {
			addr = strings.TrimPrefix(addr, "join/")
			if addr == "" {
				addr = u.Host
			}
		}
	}
	addr = strings.Trim(addr, "/ ")
	if _, err := tailcat.ParseAddr(tailcat.Addr(addr)); err != nil {
		return "", errors.New("the link is invalid or incomplete")
	}
	return tailcat.Addr(addr), nil
}

// WriteMsg writes one JSON message line.
func WriteMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// MaxMsg is the largest control message accepted.
const MaxMsg = 1 << 20

// ReadMsg reads one JSON message line.
func ReadMsg(r *bufio.Reader, v any) error {
	var line []byte
	for {
		chunk, more, err := r.ReadLine()
		if err != nil {
			return err
		}
		line = append(line, chunk...)
		if len(line) > MaxMsg {
			return errors.New("control message too large")
		}
		if !more {
			break
		}
	}
	return json.Unmarshal(line, v)
}
