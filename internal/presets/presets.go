// Package presets holds the default ports of popular game servers, so a
// server owner can pick "Satisfactory" instead of looking up its ports.
//
// Presets list only the ports players need to join. Admin ports such as
// RCON are left out on purpose: they shouldn't be reachable by friends.
package presets

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

//go:embed presets.json
var presetsJSON []byte

// Preset is a game and the ports its dedicated server listens on.
type Preset struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	TCP  []string `json:"tcp"` // ports or ranges, e.g. "7777" or "2456-2457"
	UDP  []string `json:"udp"`
	Join string   `json:"join"` // how to join in-game; {addr} and {port} are filled in
}

var all []Preset

func init() {
	if err := json.Unmarshal(presetsJSON, &all); err != nil {
		panic("presets.json: " + err.Error())
	}
	for _, p := range all {
		for _, r := range append(append([]string{}, p.TCP...), p.UDP...) {
			if _, err := ParseRange(r); err != nil {
				panic(fmt.Sprintf("presets.json: %s: %v", p.ID, err))
			}
		}
	}
}

// All returns every preset.
func All() []Preset { return append([]Preset(nil), all...) }

// Get returns the preset with the given ID.
func Get(id string) (Preset, bool) {
	for _, p := range all {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// Range is an inclusive port range.
type Range struct {
	First, Last uint16
}

func (r Range) String() string {
	if r.First == r.Last {
		return strconv.Itoa(int(r.First))
	}
	return fmt.Sprintf("%d-%d", r.First, r.Last)
}

// Ports returns every port in r.
func (r Range) Ports() []uint16 {
	var out []uint16
	for p := int(r.First); p <= int(r.Last); p++ {
		out = append(out, uint16(p))
	}
	return out
}

// MaxRangeSize limits how many ports one range may cover.
const MaxRangeSize = 100

// ParseRange parses "7777" or "2456-2457".
func ParseRange(s string) (Range, error) {
	s = strings.TrimSpace(s)
	a, b, isRange := strings.Cut(s, "-")
	first, err := parsePort(a)
	if err != nil {
		return Range{}, err
	}
	last := first
	if isRange {
		if last, err = parsePort(b); err != nil {
			return Range{}, err
		}
	}
	if last < first {
		return Range{}, fmt.Errorf("port range %q is backwards", s)
	}
	if int(last)-int(first)+1 > MaxRangeSize {
		return Range{}, fmt.Errorf("port range %q is larger than %d ports", s, MaxRangeSize)
	}
	return Range{first, last}, nil
}

func parsePort(s string) (uint16, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("%q is not a port (1-65535)", s)
	}
	return uint16(n), nil
}
