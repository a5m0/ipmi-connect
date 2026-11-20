package main

import (
	"reflect"
	"testing"
	"time"
)

func TestParseArgs(t *testing.T) {
	defaults := options{Type: "auto", WebPort: 8080}
	with := func(f func(o *options)) options {
		o := defaults
		f(&o)
		return o
	}

	tests := []struct {
		name string
		args []string
		want options
	}{
		{"no args", nil, defaults},
		{"host only", []string{"10.0.0.1"}, with(func(o *options) { o.Host = "10.0.0.1" })},
		{"first positional wins", []string{"a", "b"}, with(func(o *options) { o.Host = "a" })},
		{
			"short flags with values",
			[]string{"10.0.0.1", "-u", "admin", "-p", "s3cret"},
			with(func(o *options) { o.Host, o.User, o.Password = "10.0.0.1", "admin", "s3cret" }),
		},
		{
			"long flags with equals",
			[]string{"--user=admin", "--pass=x", "--type=dell", "--web-port=9000", "h"},
			with(func(o *options) {
				o.Host, o.User, o.Password, o.Type, o.WebPort, o.WebPortSet = "h", "admin", "x", "dell", 9000, true
			}),
		},
		{
			"-p followed by a flag leaves password empty so it is prompted",
			[]string{"h", "-p", "--no-browser"},
			with(func(o *options) { o.Host, o.NoBrowser = "h", true }),
		},
		{"invalid web port keeps default", []string{"h", "--web-port", "abc"}, with(func(o *options) { o.Host = "h" })},
		{"--port value is consumed, not taken as host", []string{"--port", "623", "h"}, with(func(o *options) { o.Host, o.PortIgnored = "h", true })},
		{"--port= is noted as ignored", []string{"h", "--port=623"}, with(func(o *options) { o.Host, o.PortIgnored = "h", true })},
		{"help", []string{"-h"}, with(func(o *options) { o.ShowHelp = true })},
		{"version short", []string{"-v"}, with(func(o *options) { o.ShowVersion = true })},
		{"version long", []string{"--version"}, with(func(o *options) { o.ShowVersion = true })},
		{"version single dash", []string{"-version"}, with(func(o *options) { o.ShowVersion = true })},
		{"desktop", []string{"--desktop"}, with(func(o *options) { o.Desktop = true })},
		{"short user with equals", []string{"-u=admin", "h"}, with(func(o *options) { o.Host, o.User = "h", "admin" })},
		{
			"trust flags",
			[]string{"h", "--trust-fingerprint", "AA:BB", "--trust-fingerprint=CC", "--replace-fingerprint", "--allow-legacy-tls", "--allow-insecure-kvm"},
			with(func(o *options) {
				o.Host, o.TrustFingerprints, o.ReplaceFingerprint, o.AllowLegacyTLS, o.AllowInsecure = "h", []string{"AA:BB", "CC"}, true, true, true
			}),
		},
		{"media dirs", []string{"--media-dir", "/isos", "--media-dir=/more"}, with(func(o *options) { o.MediaDirs = []string{"/isos", "/more"} })},
		{"debug areas", []string{"--debug", "megarac-media", "--debug=x,y"}, with(func(o *options) { o.Debug = []string{"megarac-media", "x,y"} })},
		{"idle timeout", []string{"--idle-timeout", "5m"}, with(func(o *options) { o.IdleTimeout, o.IdleSet = 5*time.Minute, true })},
		{"idle timeout off", []string{"--idle-timeout=0"}, with(func(o *options) { o.IdleSet = true })},
		{"bad idle timeout ignored", []string{"--idle-timeout", "soon"}, defaults},
		{"web port 0 asks the OS", []string{"--web-port", "0"}, with(func(o *options) { o.WebPort, o.WebPortSet = 0, true })},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseArgs(tt.args); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestGetVersion(t *testing.T) {
	orig := version
	defer func() { version = orig }()

	version = "v1.2.3"
	if got := getVersion(); got != "v1.2.3" {
		t.Errorf("getVersion() = %q, want v1.2.3", got)
	}

	version = "dev"
	if got := getVersion(); got == "" {
		t.Errorf("getVersion() returned empty string")
	}
}
