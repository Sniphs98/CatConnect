// Package host is the server side of CatConnect: a tailcat server with a
// fixed identity that forwards the ports of configured game servers to
// their hosts on the local network.
package host

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sniphs98/CatConnect/internal/proto"
	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// Identity is what keeps a server's share link the same across restarts.
type Identity struct {
	Key      key.NodePrivate      `json:"key"`
	PSK      tailcat.PresharedKey `json:"psk"`
	RegionID int                  `json:"regionId,omitempty"` // DERP region picked on first start
}

// NewIdentity returns a fresh random identity.
func NewIdentity() Identity {
	return Identity{Key: key.NewNode(), PSK: tailcat.NewPresharedKey()}
}

// Service is a game server reachable through CatConnect.
type Service struct {
	proto.Service
	Host    string `json:"host"` // where the game server runs, e.g. "192.168.1.20"
	Enabled bool   `json:"enabled"`
}

// Options configure a Host.
type Options struct {
	Identity   Identity
	DERPMapURL string              // optional own DERP map
	Region     *tailcfg.DERPRegion // optional fixed relay (tests)
	Logf       logger.Logf
}

// Host serves game ports to CatConnect clients.
type Host struct {
	opts Options
	srv  *tailcat.Server
	logf logger.Logf

	mu        sync.Mutex
	name      string
	services  []Service // enabled only
	blocked   map[key.NodePublic]bool
	clients   map[key.NodePublic]*client
	controls  map[*controlConn]bool
	udpDrops  atomic.Int64
	startedAt time.Time
}

type client struct {
	Key         key.NodePublic
	Name        string
	FirstSeen   time.Time
	LastSeen    time.Time
	Online      int // open control connections
	ActiveConns int // forwarded TCP connections and UDP flows
}

type controlConn struct {
	c   net.Conn
	key key.NodePublic
	mu  sync.Mutex
}

// New creates a Host; call Start to bring it online.
func New(opts Options) *Host {
	logf := opts.Logf
	if logf == nil {
		logf = logger.Discard
	}
	h := &Host{
		opts:     opts,
		logf:     logf,
		blocked:  map[key.NodePublic]bool{},
		clients:  map[key.NodePublic]*client{},
		controls: map[*controlConn]bool{},
	}
	h.srv = &tailcat.Server{
		Key:          opts.Identity.Key,
		PresharedKey: opts.Identity.PSK,
		Region:       opts.Region,
		RegionID:     tailcfg.DERPRegionID(opts.Identity.RegionID),
		DERPMapURL:   opts.DERPMapURL,
		Logf:         logger.Discard,
		AllowClient:  h.allowClient,
		OnTCP:        h.onTCP,
		OnUDP:        h.onUDP,
	}
	return h
}

// Start connects to the relay. It returns the identity to persist, which
// now includes the chosen relay region.
func (h *Host) Start(ctx context.Context) (Identity, error) {
	if err := h.srv.StartContext(ctx); err != nil {
		return h.opts.Identity, err
	}
	h.mu.Lock()
	h.startedAt = time.Now()
	h.mu.Unlock()
	id := h.opts.Identity
	if ci, err := tailcat.ParseAddr(h.srv.TailcatAddr()); err == nil {
		if ci.RegionID != 0 {
			id.RegionID = int(ci.RegionID)
		} else if len(ci.Region) == 1 && h.opts.Region == nil {
			id.RegionID = int(ci.Region[0].RegionID)
		}
	}
	return id, nil
}

// Close shuts the host down.
func (h *Host) Close() error { return h.srv.Close() }

// Link returns the share link.
func (h *Host) Link() string { return proto.MakeLink(h.srv.TailcatAddr()) }

// SetConfig replaces the server name and services and tells connected
// clients. Only enabled services are served.
func (h *Host) SetConfig(name string, services []Service) error {
	var enabled []Service
	var ps []proto.Service
	for _, s := range services {
		if s.Enabled {
			enabled = append(enabled, s)
			ps = append(ps, s.Service)
		}
	}
	if err := proto.ValidateServices(ps); err != nil {
		return err
	}
	h.mu.Lock()
	h.name = name
	h.services = enabled
	m := h.manifestLocked()
	ctrls := make([]*controlConn, 0, len(h.controls))
	for c := range h.controls {
		ctrls = append(ctrls, c)
	}
	h.mu.Unlock()
	for _, c := range ctrls {
		c.send(m)
	}
	return nil
}

// SetBlocked sets which client keys are refused, and disconnects them.
func (h *Host) SetBlocked(keys []key.NodePublic) {
	h.mu.Lock()
	h.blocked = map[key.NodePublic]bool{}
	for _, k := range keys {
		h.blocked[k] = true
	}
	var kick []*controlConn
	for c := range h.controls {
		if h.blocked[c.key] {
			kick = append(kick, c)
		}
	}
	h.mu.Unlock()
	for _, k := range keys {
		h.srv.DisconnectClient(k)
	}
	for _, c := range kick {
		c.c.Close()
	}
}

func (h *Host) allowClient(k key.NodePublic) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.blocked[k]
}

func (h *Host) manifestLocked() proto.Manifest {
	m := proto.Manifest{V: proto.Version, Server: h.name, Services: []proto.Service{}}
	for _, s := range h.services {
		m.Services = append(m.Services, s.Service)
	}
	return m
}

// target finds the game server for a protocol/port pair.
func (h *Host) target(udp bool, port uint16) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.services {
		tcp, u, err := s.Ranges()
		if err != nil {
			continue
		}
		rs := tcp
		if udp {
			rs = u
		}
		for _, r := range rs {
			if port >= r.First && port <= r.Last {
				return net.JoinHostPort(s.Host, strconv.Itoa(int(port))), true
			}
		}
	}
	return "", false
}

func (h *Host) peer(remote net.Addr) key.NodePublic {
	k, _ := h.srv.PeerKey(remote)
	return k
}

func (h *Host) trackConn(k key.NodePublic, delta int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.clientLocked(k)
	c.ActiveConns += delta
	c.LastSeen = time.Now()
}

func (h *Host) clientLocked(k key.NodePublic) *client {
	c, ok := h.clients[k]
	if !ok {
		c = &client{Key: k, FirstSeen: time.Now(), LastSeen: time.Now()}
		h.clients[k] = c
	}
	return c
}

func (h *Host) onTCP(port uint16) func(net.Conn) {
	if port == proto.ControlPort {
		return h.control
	}
	dst, ok := h.target(false, port)
	if !ok {
		return nil
	}
	return func(c net.Conn) {
		defer c.Close()
		k := h.peer(c.RemoteAddr())
		out, err := net.DialTimeout("tcp", dst, 5*time.Second)
		if err != nil {
			h.logf("TCP %d: game server %s unreachable: %v", port, dst, err)
			return
		}
		h.trackConn(k, 1)
		defer h.trackConn(k, -1)
		tailcat.ProxyConns(c, out)
	}
}

func (h *Host) onUDP(port uint16) func(tailcat.ConnPacketConn) {
	dst, ok := h.target(true, port)
	if !ok {
		return nil
	}
	return func(pc tailcat.ConnPacketConn) {
		defer pc.Close()
		k := h.peer(pc.RemoteAddr())
		out, err := net.Dial("udp", dst)
		if err != nil {
			h.logf("UDP %d: game server %s unreachable: %v", port, dst, err)
			return
		}
		defer out.Close()
		h.trackConn(k, 1)
		defer h.trackConn(k, -1)
		relayUDP(pc, out, &h.udpDrops)
	}
}

// relayUDP copies datagrams both ways until either side fails. tunnel
// closes itself after DefaultUDPIdleTimeout without traffic, which ends
// the relay.
func relayUDP(tunnel, game net.Conn, drops *atomic.Int64) {
	done := make(chan struct{}, 2)
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := tunnel.Read(buf)
			if err != nil {
				break
			}
			if _, err := game.Write(buf[:n]); err != nil {
				break
			}
		}
		done <- struct{}{}
	}()
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := game.Read(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					continue
				}
				break
			}
			if n > tailcat.MaxUDPPayload {
				drops.Add(1) // can't cross the tunnel without fragmentation
				continue
			}
			if _, err := tunnel.Write(buf[:n]); err != nil {
				break
			}
		}
		done <- struct{}{}
	}()
	<-done
	tunnel.Close()
	game.Close()
	<-done
}

// control serves the control connection: Hello in, Manifest out (again on
// every config change), until the client goes away.
func (h *Host) control(c net.Conn) {
	defer c.Close()
	k := h.peer(c.RemoteAddr())
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	var hello proto.Hello
	if err := proto.ReadMsg(br, &hello); err != nil {
		return
	}
	c.SetReadDeadline(time.Time{})
	if len(hello.Name) > 40 {
		hello.Name = hello.Name[:40]
	}

	cc := &controlConn{c: c, key: k}
	h.mu.Lock()
	cl := h.clientLocked(k)
	cl.Name = hello.Name
	cl.Online++
	cl.LastSeen = time.Now()
	h.controls[cc] = true
	m := h.manifestLocked()
	h.mu.Unlock()
	h.logf("%s connected (%s)", displayName(hello.Name), shortKey(k))
	defer func() {
		h.mu.Lock()
		delete(h.controls, cc)
		cl.Online--
		cl.LastSeen = time.Now()
		h.mu.Unlock()
		h.logf("%s disconnected", displayName(hello.Name))
	}()

	if err := cc.send(m); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		// Clients send nothing further; this read returns when they leave.
		io.Copy(io.Discard, br)
		close(done)
	}()
	tk := time.NewTicker(proto.KeepAlive)
	defer tk.Stop()
	for {
		select {
		case <-done:
			return
		case <-tk.C:
			h.mu.Lock()
			m := h.manifestLocked()
			h.mu.Unlock()
			if cc.send(m) != nil {
				return
			}
		}
	}
}

func (cc *controlConn) send(m proto.Manifest) error {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return proto.WriteMsg(cc.c, m)
}

// ClientInfo describes a client for the admin UI.
type ClientInfo struct {
	Key         string    `json:"key"`
	ShortKey    string    `json:"shortKey"`
	Name        string    `json:"name"`
	Online      bool      `json:"online"`
	Blocked     bool      `json:"blocked"`
	ActiveConns int       `json:"activeConns"`
	FirstSeen   time.Time `json:"firstSeen"`
	LastSeen    time.Time `json:"lastSeen"`
	Route       string    `json:"route,omitempty"` // direct, relay
	RouteInfo   string    `json:"routeInfo,omitempty"`
}

// Clients lists every client seen since start, online first.
func (h *Host) Clients() []ClientInfo {
	st := h.srv.Status()
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]ClientInfo, 0, len(h.clients))
	for k, c := range h.clients {
		ci := ClientInfo{
			Key:         k.String(),
			ShortKey:    shortKey(k),
			Name:        c.Name,
			Online:      c.Online > 0,
			Blocked:     h.blocked[k],
			ActiveConns: c.ActiveConns,
			FirstSeen:   c.FirstSeen,
			LastSeen:    c.LastSeen,
		}
		if ps, ok := st.Peer[k]; ok && ci.Online {
			ci.Route, ci.RouteInfo = "relay", ps.Relay
			if ps.CurAddr != "" {
				ci.Route, ci.RouteInfo = "direct", ps.CurAddr
			}
		}
		out = append(out, ci)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out
}

// UDPDrops counts game datagrams too large for the tunnel.
func (h *Host) UDPDrops() int64 { return h.udpDrops.Load() }

// ParseClientKey parses a key as shown by ClientInfo.Key.
func ParseClientKey(s string) (key.NodePublic, error) {
	var k key.NodePublic
	err := k.UnmarshalText([]byte(s))
	return k, err
}

func shortKey(k key.NodePublic) string { return k.ShortString() }

func displayName(n string) string {
	if n == "" {
		return "A player"
	}
	return n
}

// IdentityJSON round-trips an identity for storage.
func (id Identity) JSON() ([]byte, error) { return json.MarshalIndent(id, "", "  ") }
