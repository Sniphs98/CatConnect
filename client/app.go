package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sniphs98/CatConnect/internal/guest"
	"github.com/Sniphs98/CatConnect/internal/proto"
	"github.com/wailsapp/wails/v3/pkg/application"
	"tailscale.com/types/key"
)

const linkScheme = proto.LinkScheme

// App is bound to the frontend; its exported methods are callable from JS.
type App struct {
	desktop *desktop // nil in tests

	mu      sync.Mutex
	cfg     clientConfig
	session *guest.Session
	link    string // link of the current session
	dirty   atomic.Bool

	pendingLink string
	uiReady     bool
}

// SavedServer is a server the player has joined before.
type SavedServer struct {
	Link     string `json:"link"`
	Name     string `json:"name"`
	LastUsed int64  `json:"lastUsed"`
}

type clientConfig struct {
	PlayerName  string          `json:"playerName"`
	Key         key.NodePrivate `json:"key"`
	Servers     []SavedServer   `json:"servers"`
	AutoConnect bool            `json:"autoConnect"`
	Last        string          `json:"last"` // link connected when the app last closed
}

// Settings are the user-editable options.
type Settings struct {
	PlayerName  string `json:"playerName"`
	AutoConnect bool   `json:"autoConnect"`
}

// State is everything the UI renders.
type State struct {
	Settings Settings      `json:"settings"`
	Servers  []SavedServer `json:"servers"`
	Session  *guest.Status `json:"session"`
	Platform string        `json:"platform"`
}

func NewApp() *App {
	a := &App{}
	a.cfg = loadConfig()
	return a
}

// ---- lifecycle ----

func (a *App) startup() {
	go a.emitLoop()
	go registerProtocol()
	a.mu.Lock()
	last, auto := a.cfg.Last, a.cfg.AutoConnect
	a.mu.Unlock()
	if auto && last != "" {
		a.Connect(last)
	}
}

func (a *App) shutdown() {
	a.mu.Lock()
	s := a.session
	a.mu.Unlock()
	if s != nil {
		s.Close()
	}
}

func (a *App) emitLoop() {
	tk := time.NewTicker(250 * time.Millisecond)
	defer tk.Stop()
	for range tk.C {
		if a.dirty.Swap(false) {
			st := a.GetState()
			a.emit("state", st)
			a.rememberServerName(st)
		}
	}
}

func (a *App) changed() { a.dirty.Store(true) }

func (a *App) emit(name string, data any) {
	if a.desktop != nil {
		a.desktop.emit(name, data)
	}
}

func (a *App) showWindow() {
	if a.desktop != nil {
		a.desktop.show()
	}
}

// GetState returns everything the UI renders.
func (a *App) GetState() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := State{
		Settings: Settings{PlayerName: a.cfg.PlayerName, AutoConnect: a.cfg.AutoConnect},
		Servers:  append([]SavedServer{}, a.cfg.Servers...),
		Platform: goruntime.GOOS,
	}
	if a.session != nil {
		s := a.session.Status()
		st.Session = &s
	}
	return st
}

// rememberServerName stores the server's display name once it is known.
func (a *App) rememberServerName(st State) {
	if st.Session == nil || st.Session.Server == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.cfg.Servers {
		if a.cfg.Servers[i].Link == st.Session.Link && a.cfg.Servers[i].Name != st.Session.Server {
			a.cfg.Servers[i].Name = st.Session.Server
			saveConfig(a.cfg)
		}
	}
}

// ---- actions ----

// Connect joins the server behind a link (a new one or a saved one),
// replacing any current connection.
func (a *App) Connect(link string) error {
	link = strings.TrimSpace(link)
	if _, err := proto.ParseLink(link); err != nil {
		return err
	}
	a.mu.Lock()
	if a.session != nil && a.link == link {
		a.mu.Unlock()
		return nil // already connected
	}
	old := a.session
	a.session, a.link = nil, ""
	a.mu.Unlock()
	if old != nil {
		old.Close()
	}

	a.mu.Lock()
	s, err := guest.New(guest.Options{
		Link:       link,
		Name:       a.cfg.PlayerName,
		Key:        a.cfg.Key,
		DERPMapURL: os.Getenv("CATCONNECT_DERPMAP_URL"),
		OnChange:   a.changed,
	})
	if err != nil {
		a.mu.Unlock()
		return err
	}
	a.session, a.link = s, link
	found := false
	for i := range a.cfg.Servers {
		if a.cfg.Servers[i].Link == link {
			a.cfg.Servers[i].LastUsed = time.Now().UnixMilli()
			found = true
		}
	}
	if !found {
		a.cfg.Servers = append([]SavedServer{{Link: link, LastUsed: time.Now().UnixMilli()}}, a.cfg.Servers...)
	}
	a.cfg.Last = link
	saveConfig(a.cfg)
	a.mu.Unlock()

	go s.Run()
	a.changed()
	return nil
}

// Disconnect leaves the current server and closes its local ports.
func (a *App) Disconnect() {
	a.mu.Lock()
	s := a.session
	a.session, a.link = nil, ""
	a.cfg.Last = ""
	saveConfig(a.cfg)
	a.mu.Unlock()
	if s != nil {
		s.Close()
	}
	a.changed()
}

// RemoveServer forgets a saved server.
func (a *App) RemoveServer(link string) {
	a.mu.Lock()
	var out []SavedServer
	for _, s := range a.cfg.Servers {
		if s.Link != link {
			out = append(out, s)
		}
	}
	a.cfg.Servers = out
	saveConfig(a.cfg)
	connected := a.link == link
	a.mu.Unlock()
	if connected {
		a.Disconnect()
	}
	a.changed()
}

// SetPlayerName changes the name the server owner sees (applies on the
// next connection).
func (a *App) SetPlayerName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("enter a name")
	}
	if len(name) > 40 {
		name = name[:40]
	}
	a.mu.Lock()
	a.cfg.PlayerName = name
	saveConfig(a.cfg)
	a.mu.Unlock()
	a.changed()
	return nil
}

// SetAutoConnect controls whether CatConnect reconnects to the last server
// when it starts.
func (a *App) SetAutoConnect(on bool) {
	a.mu.Lock()
	a.cfg.AutoConnect = on
	saveConfig(a.cfg)
	a.mu.Unlock()
	a.changed()
}

// CopyText puts text on the clipboard.
func (a *App) CopyText(s string) error {
	if a.desktop == nil || !a.desktop.app.Clipboard.SetText(s) {
		return errors.New("couldn't copy to the clipboard")
	}
	return nil
}

// HideWindow hides the window back into the tray.
func (a *App) HideWindow() {
	if a.desktop != nil {
		a.desktop.hide()
	}
}

// Quit exits CatConnect.
func (a *App) Quit() {
	if a.desktop != nil {
		a.desktop.app.Quit()
	}
}

// GetAutostart reports whether CatConnect starts with the computer.
func (a *App) GetAutostart() bool {
	if a.desktop == nil {
		return false
	}
	on, err := a.desktop.app.Autostart.IsEnabled()
	return err == nil && on
}

// SetAutostart turns starting with the computer on or off.
func (a *App) SetAutostart(on bool) error {
	if a.desktop == nil {
		return nil
	}
	if on {
		return a.desktop.app.Autostart.EnableWithOptions(application.AutostartOptions{Arguments: []string{backgroundFlag}})
	}
	return a.desktop.app.Autostart.Disable()
}

// UIReady is called by the frontend once it listens for events; a link
// that started the app is opened now.
func (a *App) UIReady() {
	a.mu.Lock()
	a.uiReady = true
	link := a.pendingLink
	a.pendingLink = ""
	a.mu.Unlock()
	if link != "" {
		a.openLinkFromOS(link)
	}
	a.changed()
}

// handleLaunchArgs handles a catconnect:// link passed on the command line
// (a clicked link). It reports whether there was one.
func (a *App) handleLaunchArgs(args []string) bool {
	for _, arg := range args {
		if strings.HasPrefix(strings.ToLower(arg), linkScheme+":") {
			a.handleURL(arg)
			return true
		}
	}
	return false
}

func (a *App) handleURL(link string) {
	a.mu.Lock()
	if !a.uiReady {
		a.pendingLink = link
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	a.openLinkFromOS(link)
}

func (a *App) openLinkFromOS(link string) {
	a.showWindow()
	if err := a.Connect(link); err != nil {
		a.emit("toast", map[string]string{"kind": "error", "text": err.Error()})
	}
}

// ---- config ----

func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "CatConnect", "config.json")
}

func defaultPlayerName() string {
	if u, err := user.Current(); err == nil {
		n := u.Username
		if i := strings.LastIndexAny(n, `\/`); i >= 0 {
			n = n[i+1:] // DOMAIN\user on Windows
		}
		if n != "" {
			return n
		}
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "Player"
}

func loadConfig() clientConfig {
	c := clientConfig{AutoConnect: true}
	changed := true
	if p := configPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &c)
			changed = false
		}
	}
	if c.Key.IsZero() {
		c.Key = key.NewNode() // the owner recognises (and can block) players by this
		changed = true
	}
	if c.PlayerName == "" {
		c.PlayerName = defaultPlayerName()
		changed = true
	}
	if c.Servers == nil {
		c.Servers = []SavedServer{}
	}
	if changed {
		saveConfig(c)
	}
	return c
}

func saveConfig(c clientConfig) {
	p := configPath()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	b, _ := json.MarshalIndent(c, "", "  ")
	_ = os.WriteFile(p, b, 0o600)
}
