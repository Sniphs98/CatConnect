// Package guest is the client side of CatConnect: it connects to a server
// by its link and opens the server's game ports on this computer's
// loopback address, forwarding them through the tunnel.
package guest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sniphs98/CatConnect/internal/presets"
	"github.com/Sniphs98/CatConnect/internal/proto"
	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// Options configure a Session.
type Options struct {
	Link       string
	Name       string          // player name shown to the server owner
	Key        key.NodePrivate // persistent identity, so the owner can recognise and block players
	DERPMapURL string
	Logf       logger.Logf
	// OnChange is called (from any goroutine) whenever Status changes.
	OnChange func()
	// BindAddrs are the local addresses to listen on; default 127.0.0.1 and ::1.
	BindAddrs []string
}

// Session is a connection to one CatConnect server.
type Session struct {
	opts   Options
	addr   tailcat.Addr
	client *tailcat.Client
	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	state     string // connecting, connected, reconnecting, error, closed
	err       string
	manifest  proto.Manifest
	listeners map[string]*portListener // "tcp/7777" -> listener
	route     string
	routeInfo string
	latency   time.Duration
	drops     atomic.Int64
}

type portListener struct {
	udp     bool
	port    uint16
	service string
	err     string
	active  atomic.Int32
	closers []func()
}

// New prepares a session; call Run to connect.
func New(opts Options) (*Session, error) {
	addr, err := proto.ParseLink(opts.Link)
	if err != nil {
		return nil, err
	}
	if opts.Logf == nil {
		opts.Logf = logger.Discard
	}
	if len(opts.BindAddrs) == 0 {
		opts.BindAddrs = []string{"127.0.0.1", "::1"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Session{
		opts:      opts,
		addr:      addr,
		client:    &tailcat.Client{Server: addr, Key: opts.Key, DERPMapURL: opts.DERPMapURL, Logf: logger.Discard},
		ctx:       ctx,
		cancel:    cancel,
		state:     "connecting",
		listeners: map[string]*portListener{},
	}, nil
}

// Run connects and keeps the session alive until Close. It reconnects the
// control connection when it drops.
func (s *Session) Run() {
	go s.pingLoop()
	backoff := time.Second
	for s.ctx.Err() == nil {
		err := s.controlOnce()
		if s.ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		if s.state == "connected" {
			s.state = "reconnecting"
			backoff = time.Second
		} else if s.state == "connecting" || s.state == "error" {
			s.state = "error"
		}
		if err != nil {
			s.err = err.Error()
		}
		s.mu.Unlock()
		s.changed()
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// Close disconnects and closes every local port.
func (s *Session) Close() {
	s.cancel()
	s.mu.Lock()
	s.state = "closed"
	for k, l := range s.listeners {
		l.close()
		delete(s.listeners, k)
	}
	s.mu.Unlock()
	s.client.Close()
	s.changed()
}

func (s *Session) changed() {
	if s.opts.OnChange != nil {
		s.opts.OnChange()
	}
}

func (s *Session) controlOnce() error {
	ctx, cancel := context.WithTimeout(s.ctx, 45*time.Second)
	c, err := s.client.DialTCPPort(ctx, proto.ControlPort)
	cancel()
	if err != nil {
		return errors.New("can't reach the server. It may be offline, or the link is outdated")
	}
	defer c.Close()
	go func() { <-s.ctx.Done(); c.Close() }()

	if err := proto.WriteMsg(c, proto.Hello{V: proto.Version, Name: s.opts.Name}); err != nil {
		return err
	}
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(20 * time.Second))
	for first := true; ; first = false {
		var m proto.Manifest
		if err := proto.ReadMsg(br, &m); err != nil {
			if first {
				return errors.New("the server didn't answer")
			}
			return errors.New("connection to the server lost")
		}
		c.SetReadDeadline(time.Now().Add(3 * proto.KeepAlive))
		if m.V != proto.Version {
			return fmt.Errorf("this server needs a different CatConnect version (protocol %d)", m.V)
		}
		s.apply(m)
	}
}

// apply opens listeners for the manifest's ports and closes the rest.
func (s *Session) apply(m proto.Manifest) {
	want := map[string]*portListener{}
	for _, svc := range m.Services {
		tcp, udp, err := svc.Ranges()
		if err != nil {
			continue
		}
		add := func(isUDP bool, rs []presets.Range) {
			for _, r := range rs {
				for _, p := range r.Ports() {
					want[lkey(isUDP, p)] = &portListener{udp: isUDP, port: p, service: svc.ID}
				}
			}
		}
		add(false, tcp)
		add(true, udp)
	}

	s.mu.Lock()
	if s.state == "connected" && sameManifest(s.manifest, m) {
		s.mu.Unlock()
		return // keepalive
	}
	s.manifest = m
	s.state = "connected"
	s.err = ""
	for k, l := range s.listeners {
		if _, ok := want[k]; !ok || l.err != "" {
			l.close()
			delete(s.listeners, k)
		}
	}
	for k, l := range want {
		if old, ok := s.listeners[k]; ok {
			old.service = l.service
			continue
		}
		s.open(l)
		s.listeners[k] = l
	}
	s.mu.Unlock()
	s.changed()
}

func sameManifest(a, b proto.Manifest) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

func lkey(udp bool, port uint16) string {
	if udp {
		return "udp/" + strconv.Itoa(int(port))
	}
	return "tcp/" + strconv.Itoa(int(port))
}

func (l *portListener) close() {
	for _, c := range l.closers {
		c()
	}
	l.closers = nil
}

// open starts listening on every bind address. Failing on 127.0.0.1 is an
// error (the port is taken); ::1 is best effort.
func (s *Session) open(l *portListener) {
	for i, ba := range s.opts.BindAddrs {
		hp := net.JoinHostPort(ba, strconv.Itoa(int(l.port)))
		var err error
		if l.udp {
			err = s.listenUDP(l, hp)
		} else {
			err = s.listenTCP(l, hp)
		}
		if err != nil && i == 0 {
			proto := "TCP"
			if l.udp {
				proto = "UDP"
			}
			l.err = fmt.Sprintf("%s port %d is already in use on this computer", proto, l.port)
			s.opts.Logf("listen %s: %v", hp, err)
			l.close()
			return
		}
	}
}

func (s *Session) listenTCP(l *portListener, hp string) error {
	ln, err := net.Listen("tcp", hp)
	if err != nil {
		return err
	}
	l.closers = append(l.closers, func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
				tc, err := s.client.DialTCPPort(ctx, l.port)
				cancel()
				if err != nil {
					s.opts.Logf("dial TCP %d: %v", l.port, err)
					return
				}
				l.active.Add(1)
				s.changed()
				tailcat.ProxyConns(c, tc)
				l.active.Add(-1)
				s.changed()
			}()
		}
	}()
	return nil
}

const udpIdle = 3 * time.Minute

type udpFlow struct {
	conn     tailcat.ConnPacketConn
	lastSeen atomic.Int64
}

func (s *Session) listenUDP(l *portListener, hp string) error {
	ua, err := net.ResolveUDPAddr("udp", hp)
	if err != nil {
		return err
	}
	pc, err := net.ListenUDP("udp", ua)
	if err != nil {
		return err
	}
	var mu sync.Mutex
	flows := map[string]*udpFlow{}
	closed := make(chan struct{})
	l.closers = append(l.closers, func() {
		close(closed)
		pc.Close()
		mu.Lock()
		for k, f := range flows {
			f.conn.Close()
			delete(flows, k)
		}
		mu.Unlock()
	})

	// Close flows the game stopped using.
	go func() {
		tk := time.NewTicker(30 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-closed:
				return
			case <-tk.C:
			}
			cutoff := time.Now().Add(-udpIdle).UnixNano()
			mu.Lock()
			for k, f := range flows {
				if f.lastSeen.Load() < cutoff {
					f.conn.Close()
					delete(flows, k)
				}
			}
			mu.Unlock()
		}
	}()

	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, src, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n > tailcat.MaxUDPPayload {
				s.drops.Add(1)
				continue
			}
			k := src.String()
			mu.Lock()
			f, ok := flows[k]
			mu.Unlock()
			if !ok {
				ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
				fc, err := s.client.DialUDPPort(ctx, l.port)
				cancel()
				if err != nil {
					s.opts.Logf("dial UDP %d: %v", l.port, err)
					continue
				}
				f = &udpFlow{conn: fc}
				f.lastSeen.Store(time.Now().UnixNano())
				mu.Lock()
				flows[k] = f
				mu.Unlock()
				l.active.Add(1)
				s.changed()
				go func(src *net.UDPAddr) {
					rb := make([]byte, 64<<10)
					for {
						n, err := f.conn.Read(rb)
						if err != nil {
							break
						}
						f.lastSeen.Store(time.Now().UnixNano())
						pc.WriteToUDP(rb[:n], src)
					}
					mu.Lock()
					if flows[src.String()] == f {
						delete(flows, src.String())
					}
					mu.Unlock()
					l.active.Add(-1)
					s.changed()
				}(src)
			}
			f.lastSeen.Store(time.Now().UnixNano())
			f.conn.Write(buf[:n])
		}
	}()
	return nil
}

func (s *Session) pingLoop() {
	for s.ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
		start := time.Now()
		r, err := s.client.DiscoPing(ctx)
		cancel()
		if s.ctx.Err() != nil {
			return
		}
		wait := 2 * time.Second
		if err == nil {
			s.mu.Lock()
			s.route, s.routeInfo = "relay", r.DERPRegionCode
			if r.Endpoint != "" {
				s.route, s.routeInfo = "direct", r.Endpoint
				wait = 5 * time.Second
			}
			if r.LatencySeconds > 0 {
				s.latency = time.Duration(r.LatencySeconds * float64(time.Second))
			} else {
				s.latency = time.Since(start)
			}
			s.mu.Unlock()
			s.changed()
		}
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// ServiceStatus is a game as shown to the player.
type ServiceStatus struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Preset  string   `json:"preset,omitempty"`
	Address string   `json:"address"` // what to type into the game, e.g. "localhost:25565"
	Ports   []string `json:"ports"`   // e.g. ["TCP 7777", "UDP 7777"]
	Join    string   `json:"join,omitempty"`
	Error   string   `json:"error,omitempty"`
	Active  int      `json:"active"`
}

// Status is a snapshot of the session for the UI.
type Status struct {
	Link      string          `json:"link"`
	Server    string          `json:"server"`
	State     string          `json:"state"`
	Error     string          `json:"error,omitempty"`
	Route     string          `json:"route,omitempty"`
	RouteInfo string          `json:"routeInfo,omitempty"`
	LatencyMs float64         `json:"latencyMs"`
	Services  []ServiceStatus `json:"services"`
	UDPDrops  int64           `json:"udpDrops"`
}

// Status returns the current state.
func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{
		Link:      s.opts.Link,
		Server:    s.manifest.Server,
		State:     s.state,
		Error:     s.err,
		Route:     s.route,
		RouteInfo: s.routeInfo,
		LatencyMs: float64(s.latency.Microseconds()) / 1000,
		Services:  []ServiceStatus{},
		UDPDrops:  s.drops.Load(),
	}
	for _, svc := range s.manifest.Services {
		port := svc.FirstPort()
		addr := "localhost:" + strconv.Itoa(int(port))
		ss := ServiceStatus{
			ID:      svc.ID,
			Name:    svc.Name,
			Preset:  svc.Preset,
			Address: addr,
			Join:    strings.NewReplacer("{addr}", addr, "{port}", strconv.Itoa(int(port))).Replace(svc.Join),
		}
		for _, r := range svc.TCP {
			ss.Ports = append(ss.Ports, "TCP "+r)
		}
		for _, r := range svc.UDP {
			ss.Ports = append(ss.Ports, "UDP "+r)
		}
		for _, l := range s.listeners {
			if l.service != svc.ID {
				continue
			}
			ss.Active += int(l.active.Load())
			if l.err != "" && ss.Error == "" {
				ss.Error = l.err
			}
		}
		st.Services = append(st.Services, ss)
	}
	return st
}
