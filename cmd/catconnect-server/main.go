// Command catconnect-server runs next to your game servers (in a homelab,
// on a NAS, in Docker) and lets friends with the CatConnect app reach them
// through one link. Configure it in the browser at http://<host>:8080.
//
// Settings come from flags or environment variables:
//
//	--data      CATCONNECT_DATA         where identity and config are kept
//	--listen    CATCONNECT_LISTEN       address of the admin web UI (default :8080)
//	--password  CATCONNECT_PASSWORD     admin password (generated on first start if unset)
//	            CATCONNECT_DERPMAP_URL  your own DERP map (optional)
package main

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Sniphs98/CatConnect/internal/host"
	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
)

var version = "dev"

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func defaultDataDir() string {
	if fi, err := os.Stat("/data"); err == nil && fi.IsDir() {
		return "/data" // Docker volume
	}
	return "catconnect-data"
}

func main() {
	dataDir := flag.String("data", env("CATCONNECT_DATA", defaultDataDir()), "directory for identity and config")
	listen := flag.String("listen", env("CATCONNECT_LISTEN", ":8080"), "address of the admin web UI")
	password := flag.String("password", os.Getenv("CATCONNECT_PASSWORD"), "admin password (generated if empty)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	log.SetFlags(log.Ldate | log.Ltime)
	tailcat.Verbose = false

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Fatalf("data directory: %v", err)
	}
	st := &store{dir: *dataDir}
	pw, generated, err := st.password(*password)
	if err != nil {
		log.Fatalf("admin password: %v", err)
	}

	srv, err := newServer(st, os.Getenv("CATCONNECT_DERPMAP_URL"))
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := srv.start(ctx); err != nil {
		log.Fatalf("connecting to the relay: %v", err)
	}
	defer srv.close()

	log.Printf("CatConnect server %s is online", version)
	log.Printf("Share link: %s", srv.link())
	if generated {
		log.Printf("Admin password (saved in %s): %s", filepath.Join(*dataDir, "admin-password"), pw)
	}

	hs := &http.Server{Addr: *listen, Handler: srv.handler(pw), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("Admin UI on http://%s", displayAddr(*listen))
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("admin UI: %v", err)
		}
	}()
	<-ctx.Done()
	log.Printf("shutting down")
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	hs.Shutdown(sctx)
}

func displayAddr(listen string) string {
	if strings.HasPrefix(listen, ":") {
		return "localhost" + listen
	}
	return listen
}

// Config is what the owner edits in the admin UI.
type Config struct {
	Name     string         `json:"name"`
	Services []host.Service `json:"services"`
	Blocked  []string       `json:"blocked"` // client keys
}

// store keeps identity, config and password in the data directory.
type store struct{ dir string }

func (s *store) path(name string) string { return filepath.Join(s.dir, name) }

func (s *store) readJSON(name string, v any) (bool, error) {
	b, err := os.ReadFile(s.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func (s *store) writeJSON(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(name + ".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(name))
}

func (s *store) password(fromFlag string) (pw string, generated bool, err error) {
	if fromFlag != "" {
		return fromFlag, false, nil
	}
	b, err := os.ReadFile(s.path("admin-password"))
	if err == nil {
		return strings.TrimSpace(string(b)), false, nil
	}
	buf := make([]byte, 10)
	rand.Read(buf)
	pw = strings.ToLower(base32.StdEncoding.EncodeToString(buf))
	return pw, true, os.WriteFile(s.path("admin-password"), []byte(pw+"\n"), 0o600)
}

// server ties the host, its stored config and the admin UI together.
type server struct {
	st      *store
	derpURL string

	mu       sync.Mutex
	h        *host.Host
	identity host.Identity
	cfg      Config
}

func newServer(st *store, derpURL string) (*server, error) {
	s := &server{st: st, derpURL: derpURL}
	ok, err := st.readJSON("identity.json", &s.identity)
	if err != nil {
		return nil, fmt.Errorf("identity.json: %w", err)
	}
	if !ok {
		s.identity = host.NewIdentity()
	}
	if _, err := st.readJSON("config.json", &s.cfg); err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	if s.cfg.Name == "" {
		hn, _ := os.Hostname()
		s.cfg.Name = hn
	}
	if s.cfg.Services == nil {
		s.cfg.Services = []host.Service{}
	}
	return s, nil
}

func (s *server) start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startLocked(ctx)
}

func (s *server) startLocked(ctx context.Context) error {
	h := host.New(host.Options{Identity: s.identity, DERPMapURL: s.derpURL, Logf: log.Printf})
	sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	id, err := h.Start(sctx)
	if err != nil && s.identity.RegionID != 0 {
		// The relay region the link was made with is gone. Start anyway with
		// the nearest one; the link changes, which beats being offline.
		h.Close()
		log.Printf("relay region %d unavailable (%v); picking a new one. The share link changes", s.identity.RegionID, err)
		s.identity.RegionID = 0
		h = host.New(host.Options{Identity: s.identity, DERPMapURL: s.derpURL, Logf: log.Printf})
		id, err = h.Start(sctx)
	}
	if err != nil {
		h.Close()
		return err
	}
	s.identity = id
	if err := s.st.writeJSON("identity.json", id); err != nil {
		h.Close()
		return fmt.Errorf("saving identity: %w", err)
	}
	if err := h.SetConfig(s.cfg.Name, s.cfg.Services); err != nil {
		log.Printf("config has errors, serving no games until fixed: %v", err)
		h.SetConfig(s.cfg.Name, nil)
	}
	h.SetBlocked(parseKeys(s.cfg.Blocked))
	s.h = h
	return nil
}

func (s *server) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.h != nil {
		s.h.Close()
	}
}

func (s *server) link() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.h.Link()
}

// setConfig validates, applies and saves a new config.
func (s *server) setConfig(c Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		return errors.New("give the server a name")
	}
	for i := range c.Services {
		svc := &c.Services[i]
		svc.Name = strings.TrimSpace(svc.Name)
		svc.Host = strings.TrimSpace(svc.Host)
		if svc.Name == "" {
			return errors.New("every game needs a name")
		}
		if svc.Host == "" {
			return fmt.Errorf("%s: enter the address of the game server, e.g. 192.168.1.20", svc.Name)
		}
		if svc.ID == "" {
			svc.ID = randomID()
		}
	}
	c.Blocked = s.cfg.Blocked
	if err := s.h.SetConfig(c.Name, c.Services); err != nil {
		return err
	}
	s.cfg = c
	return s.st.writeJSON("config.json", c)
}

func (s *server) setBlocked(k string, blocked bool) error {
	if _, err := host.ParseClientKey(k); err != nil {
		return errors.New("unknown player")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, b := range s.cfg.Blocked {
		if b != k {
			out = append(out, b)
		}
	}
	if blocked {
		out = append(out, k)
	}
	s.cfg.Blocked = out
	s.h.SetBlocked(parseKeys(out))
	return s.st.writeJSON("config.json", s.cfg)
}

// resetLink gives the server a new identity: the old link stops working
// for everyone.
func (s *server) resetLink(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.h
	oldID := s.identity
	old.Close()
	s.identity = host.NewIdentity()
	if err := s.startLocked(ctx); err != nil {
		s.identity = oldID
		if err2 := s.startLocked(ctx); err2 != nil {
			log.Printf("restarting with the old link failed too: %v", err2)
		}
		return err
	}
	log.Printf("Share link reset. New link: %s", s.h.Link())
	return nil
}

func parseKeys(ss []string) []key.NodePublic {
	var out []key.NodePublic
	for _, s := range ss {
		if k, err := host.ParseClientKey(s); err == nil {
			out = append(out, k)
		}
	}
	return out
}

func randomID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}
