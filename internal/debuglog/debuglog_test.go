package debuglog

import (
	"bytes"
	"log"
	"slices"
	"strings"
	"testing"
)

// reset switches every area off again when the test ends.
func reset(t *testing.T) {
	t.Helper()
	Reset()
	t.Cleanup(Reset)
}

// capture sends the standard logger to a buffer for the rest of the test.
func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(out); log.SetFlags(flags) })
	return &buf
}

func TestOffByDefault(t *testing.T) {
	reset(t)
	if On(MegaRACMedia) {
		t.Error("megarac-media is on before Enable")
	}
}

func TestEnable(t *testing.T) {
	reset(t)
	if unknown := Enable("megarac-media"); len(unknown) != 0 {
		t.Errorf("unknown %q", unknown)
	}
	if !On(MegaRACMedia) {
		t.Error("megarac-media is off after Enable")
	}
}

func TestEnableTrimsFoldsAndReportsUnknown(t *testing.T) {
	reset(t)
	unknown := Enable(" MegaRAC-Media , bogus")
	if !On(MegaRACMedia) {
		t.Error("megarac-media is off")
	}
	if !slices.Equal(unknown, []string{"bogus"}) {
		t.Errorf("unknown %q, want [bogus]", unknown)
	}
	if On("bogus") {
		t.Error("an unknown area was switched on")
	}
}

func TestEnableEmpty(t *testing.T) {
	reset(t)
	if unknown := Enable(" , "); len(unknown) != 0 {
		t.Errorf("unknown %q", unknown)
	}
	if len(Areas()) != 0 {
		t.Errorf("areas %q", Areas())
	}
}

func TestEnableSpaceSeparated(t *testing.T) {
	reset(t)
	Enable("x megarac-media")
	if !slices.Equal(Areas(), []string{MegaRACMedia}) {
		t.Errorf("areas %q", Areas())
	}
}

func TestPrintfOnlyWhenOn(t *testing.T) {
	reset(t)
	buf := capture(t)
	Printf(MegaRACMedia, "hidden %d", 1)
	if buf.Len() != 0 {
		t.Errorf("logged while off: %q", buf)
	}
	Enable(MegaRACMedia)
	Printf(MegaRACMedia, "shown %d", 2)
	if got := buf.String(); !strings.Contains(got, "[Debug megarac-media] shown 2") {
		t.Errorf("logged %q", got)
	}
}
