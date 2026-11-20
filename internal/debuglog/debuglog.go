// Package debuglog switches on protocol traces for troubleshooting, by area, from the
// --debug flag and the IPMI_CONNECT_DEBUG environment variable. It is process-wide: the
// traces are for diagnostics only, and threading a switch through every layer would cost more
// than it is worth.
package debuglog

import (
	"log"
	"strings"
	"sync/atomic"
)

// MegaRACMedia traces MegaRAC virtual media: each SCSI command's opcode, CDB and reply status
// (never the data), the private opcodes with a short data tail, and every MEDIA_STATE sent.
const MegaRACMedia = "megarac-media"

// known lists the areas in the order Areas reports them.
var known = []string{MegaRACMedia}

var enabled = func() map[string]*atomic.Bool {
	m := make(map[string]*atomic.Bool, len(known))
	for _, a := range known {
		m[a] = new(atomic.Bool)
	}
	return m
}()

// Enable switches on the areas in spec, a list separated by commas or spaces (case does not
// matter), and returns the names it does not know.
func Enable(spec string) (unknown []string) {
	for _, name := range strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if on, ok := enabled[name]; ok {
			on.Store(true)
		} else {
			unknown = append(unknown, name)
		}
	}
	return unknown
}

// Reset switches every area off.
func Reset() {
	for _, on := range enabled {
		on.Store(false)
	}
}

// On reports whether area is switched on.
func On(area string) bool {
	on, ok := enabled[area]
	return ok && on.Load()
}

// Known returns every area name.
func Known() []string { return append([]string(nil), known...) }

// Areas returns the areas that are on.
func Areas() []string {
	var out []string
	for _, a := range known {
		if On(a) {
			out = append(out, a)
		}
	}
	return out
}

// Printf logs a trace line for area when it is on.
func Printf(area, format string, a ...any) {
	if On(area) {
		log.Printf("[Debug "+area+"] "+format, a...)
	}
}
