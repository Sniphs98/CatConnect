package internal_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sniphs98/CatConnect/internal/guest"
	"github.com/Sniphs98/CatConnect/internal/host"
	"github.com/Sniphs98/CatConnect/internal/proto"
	"tailscale.com/envknob"
	"tailscale.com/net/netcheck"
	"tailscale.com/syncs"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// TestMain keeps the tests hermetic, like tailcat's own.
func TestMain(m *testing.M) {
	envknob.Setenv("IN_TS_TEST", "true")
	netcheck.HookStartCaptivePortalDetection.SetForTest(func(ctx context.Context, c *netcheck.Client, dm *tailcfg.DERPMap, preferredDERP tailcfg.DERPRegionID, setCaptivePortal func(bool)) (done <-chan struct{}, stop func()) {
		return syncs.ClosedChan(), func() {}
	})
	os.Exit(m.Run())
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// freePort returns a port that is free for both TCP and UDP on 127.0.0.1.
func freePort(t *testing.T) uint16 {
	t.Helper()
	for range 50 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		p := l.Addr().(*net.TCPAddr).Port
		l.Close()
		u, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p})
		if err != nil {
			continue
		}
		u.Close()
		return uint16(p)
	}
	t.Fatal("no free port")
	return 0
}

// TestForwarding runs a "game server" (TCP and UDP echo) behind a host,
// connects a guest, and plays through the guest's local ports. Because
// ports are forwarded 1:1, the game server listens on one loopback address
// and the guest on another (127.0.0.2 vs 127.0.0.1).
func TestForwarding(t *testing.T) {
	dm := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1")
	port := freePort(t)
	ps := strconv.Itoa(int(port))

	// Game server on 127.0.0.2.
	gameTCP, err := net.Listen("tcp", "127.0.0.2:"+ps)
	if err != nil {
		t.Skipf("127.0.0.2 not usable here: %v", err)
	}
	defer gameTCP.Close()
	go func() {
		for {
			c, err := gameTCP.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	gameUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: int(port)})
	if err != nil {
		t.Fatal(err)
	}
	defer gameUDP.Close()
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, src, err := gameUDP.ReadFromUDP(buf)
			if err != nil {
				return
			}
			gameUDP.WriteToUDP(buf[:n], src)
		}
	}()

	id := host.NewIdentity()
	h := host.New(host.Options{Identity: id, Region: dm.Regions[1], Logf: t.Logf})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	svc := host.Service{
		Service: proto.Service{ID: "g", Name: "Test Game", TCP: []string{ps}, UDP: []string{ps}, Join: "connect {addr}"},
		Host:    "127.0.0.2",
		Enabled: true,
	}
	if err := h.SetConfig("Homelab", []host.Service{svc}); err != nil {
		t.Fatal(err)
	}
	link := h.Link()
	t.Logf("link: %s", link)

	var changes atomic.Int32
	playerKey := key.NewNode()
	g, err := guest.New(guest.Options{
		Link:      link,
		Name:      "Alex",
		Key:       playerKey,
		Logf:      t.Logf,
		OnChange:  func() { changes.Add(1) },
		BindAddrs: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	go g.Run()
	defer g.Close()

	waitFor(t, "connected", 30*time.Second, func() bool {
		st := g.Status()
		if st.State == "error" {
			t.Fatalf("guest error: %s", st.Error)
		}
		return st.State == "connected" && len(st.Services) == 1
	})
	st := g.Status()
	if st.Server != "Homelab" || st.Services[0].Address != "localhost:"+ps || st.Services[0].Join != "connect localhost:"+ps {
		t.Fatalf("status = %+v", st)
	}
	if st.Services[0].Error != "" {
		t.Fatalf("service error: %s", st.Services[0].Error)
	}

	// TCP round trip with a chunk bigger than a single packet.
	c, err := net.Dial("tcp", "127.0.0.1:"+ps)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 256<<10)
	rand.Read(payload)
	go c.Write(payload)
	got := make([]byte, len(payload))
	c.SetReadDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("TCP echo: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("TCP echo mismatch")
	}
	c.Close()

	// UDP round trips from two "players" (two local source ports).
	for i := range 2 {
		u, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)})
		if err != nil {
			t.Fatal(err)
		}
		msg := []byte("hello from player " + strconv.Itoa(i))
		ok := false
		for try := 0; try < 20 && !ok; try++ {
			u.Write(msg)
			u.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			buf := make([]byte, 2048)
			n, err := u.Read(buf)
			ok = err == nil && bytes.Equal(buf[:n], msg)
		}
		if !ok {
			t.Fatalf("UDP echo for player %d failed", i)
		}
		u.Close()
	}

	// The host lists the player by name.
	waitFor(t, "client listed", 5*time.Second, func() bool {
		cl := h.Clients()
		return len(cl) == 1 && cl[0].Name == "Alex" && cl[0].Online
	})

	// Config changes reach the guest: a second game appears.
	port2 := freePort(t)
	svc2 := host.Service{Service: proto.Service{ID: "h", Name: "Other", TCP: []string{strconv.Itoa(int(port2))}}, Host: "127.0.0.2", Enabled: true}
	if err := h.SetConfig("Homelab", []host.Service{svc, svc2}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "manifest update", 10*time.Second, func() bool { return len(g.Status().Services) == 2 })

	// Duplicate ports are rejected.
	dup := svc2
	dup.ID, dup.TCP = "x", []string{ps}
	if err := h.SetConfig("Homelab", []host.Service{svc, dup}); err == nil {
		t.Fatal("duplicate port accepted")
	}

	// Blocking disconnects the player.
	h.SetBlocked([]key.NodePublic{playerKey.Public()})
	waitFor(t, "blocked client offline", 10*time.Second, func() bool {
		cl := h.Clients()
		return len(cl) == 1 && !cl[0].Online && cl[0].Blocked
	})
	waitFor(t, "guest notices", 3*proto.KeepAlive+10*time.Second, func() bool { return g.Status().State != "connected" })
	t.Logf("guest after block: %+v", g.Status().State)
}

// TestStableLink checks that a host restarted with the same identity keeps
// its link. Like a real server it picks its relay from a DERP map, whose
// region IDs the link itself doesn't carry.
func TestStableLink(t *testing.T) {
	dm := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1")
	dm.Regions[900] = dm.Regions[1]
	dm.Regions[900].RegionID = 900
	for _, n := range dm.Regions[900].Nodes {
		n.RegionID = 900
	}
	delete(dm.Regions, 1)
	mapSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(dm)
	}))
	defer mapSrv.Close()

	id := host.NewIdentity()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var links []string
	for range 2 {
		h := host.New(host.Options{Identity: id, DERPMapURL: mapSrv.URL})
		var err error
		if id, err = h.Start(ctx); err != nil {
			t.Fatal(err)
		}
		links = append(links, h.Link())
		h.Close()
	}
	if id.RegionID != 900 {
		t.Fatalf("stored region = %d, want 900", id.RegionID)
	}
	if links[0] != links[1] {
		t.Fatalf("link changed across restarts:\n%s\n%s", links[0], links[1])
	}
}

// TestHostRestart restarts the host (same identity, same link) while a
// guest is connected; the guest must find its way back on its own.
func TestHostRestart(t *testing.T) {
	dm := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1")
	port := freePort(t)
	ps := strconv.Itoa(int(port))
	game, err := net.Listen("tcp", "127.0.0.2:"+ps)
	if err != nil {
		t.Skipf("127.0.0.2 not usable here: %v", err)
	}
	defer game.Close()
	go func() {
		for {
			c, err := game.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	svc := []host.Service{{Service: proto.Service{ID: "g", Name: "Game", TCP: []string{ps}}, Host: "127.0.0.2", Enabled: true}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	id := host.NewIdentity()
	start := func() *host.Host {
		h := host.New(host.Options{Identity: id, Region: dm.Regions[1]})
		if _, err := h.Start(ctx); err != nil {
			t.Fatal(err)
		}
		if err := h.SetConfig("Lab", svc); err != nil {
			t.Fatal(err)
		}
		return h
	}
	h := start()
	g, err := guest.New(guest.Options{Link: h.Link(), Name: "Sam", Key: key.NewNode(), BindAddrs: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	go g.Run()
	defer g.Close()

	echo := func() bool {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+ps, 2*time.Second)
		if err != nil {
			return false
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		c.Write([]byte("ping"))
		buf := make([]byte, 4)
		_, err = io.ReadFull(c, buf)
		return err == nil && string(buf) == "ping"
	}
	waitFor(t, "first connection", 30*time.Second, func() bool { return g.Status().State == "connected" && echo() })

	h.Close()
	waitFor(t, "guest notices restart", 3*proto.KeepAlive+10*time.Second, func() bool { return g.Status().State != "connected" })
	h = start()
	defer h.Close()
	waitFor(t, "reconnect after host restart", 90*time.Second, func() bool { return g.Status().State == "connected" && echo() })
}
