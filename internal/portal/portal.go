// Package portal talks to xdg-desktop-portal over the session bus: OpenURI to show the console in
// the user's browser, and FileChooser to let the user pick a disk image. Inside a Flatpak these
// are the sanctioned ways out of the sandbox and need no extra permissions (the portal bus names
// are always reachable); a file chosen in FileChooser is exposed to the sandbox individually by
// the document portal, under /run/user/<uid>/doc/.
//
// Only the two portal interfaces are used; no other bus name is contacted.
package portal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/godbus/dbus/v5"
)

const (
	busName    = "org.freedesktop.portal.Desktop"
	objectPath = "/org/freedesktop/portal/desktop"
	requestIfc = "org.freedesktop.portal.Request"
)

// InFlatpak reports whether the process runs inside a Flatpak sandbox.
func InFlatpak() bool {
	if os.Getenv("FLATPAK_ID") != "" {
		return true
	}
	_, err := os.Stat("/.flatpak-info")
	return err == nil
}

// Available reports whether a portal answers on the session bus.
func Available(ctx context.Context) bool {
	conn, err := dbus.ConnectSessionBus(dbus.WithContext(ctx))
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	var owner string
	err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, busName).Store(&owner)
	if err == nil {
		return true
	}
	// The portal may be D-Bus activatable without running yet.
	var names []string
	if conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.ListActivatableNames", 0).Store(&names) == nil {
		for _, n := range names {
			if n == busName {
				return true
			}
		}
	}
	return false
}

// OpenURI asks the desktop to open uri in the default handler (the browser, for http).
func OpenURI(ctx context.Context, uri string) error {
	conn, err := dbus.ConnectSessionBus(dbus.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("session bus: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_, err = request(ctx, conn, "org.freedesktop.portal.OpenURI.OpenURI", false, "", uri, map[string]dbus.Variant{})
	return err
}

// PickFiles shows the desktop's file chooser and returns the local paths chosen; nil when the
// user cancelled. directory chooses a folder instead of an image file.
func PickFiles(ctx context.Context, title string, directory bool) ([]string, error) {
	conn, err := dbus.ConnectSessionBus(dbus.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("session bus: %w", err)
	}
	defer func() { _ = conn.Close() }()
	opts := map[string]dbus.Variant{
		"modal":    dbus.MakeVariant(true),
		"multiple": dbus.MakeVariant(false),
	}
	if directory {
		opts["directory"] = dbus.MakeVariant(true)
	} else {
		type pattern struct {
			Kind uint32
			Glob string
		}
		type filter struct {
			Name     string
			Patterns []pattern
		}
		images := filter{"Disk images (.iso, .img)", []pattern{{0, "*.iso"}, {0, "*.img"}, {0, "*.ISO"}, {0, "*.IMG"}}}
		opts["filters"] = dbus.MakeVariant([]filter{images})
		opts["current_filter"] = dbus.MakeVariant(images)
	}
	results, err := request(ctx, conn, "org.freedesktop.portal.FileChooser.OpenFile", true, "", title, opts)
	if err != nil || results == nil {
		return nil, err
	}
	var uris []string
	if v, ok := results["uris"]; ok {
		_ = v.Store(&uris)
	}
	var paths []string
	for _, u := range uris {
		p, err := fileURIPath(u)
		if err != nil {
			continue
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// fileURIPath converts a file:// URI to a local path.
func fileURIPath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") || !strings.HasPrefix(u.Path, "/") {
		return "", errors.New("not a local file URI")
	}
	return u.Path, nil
}

// errCancelled is the portal's "user cancelled" response.
var errCancelled = errors.New("cancelled")

// request calls a portal method that returns a Request handle and waits for its Response
// signal. It subscribes before calling, using a handle token, so the response cannot be missed.
// It returns nil results when the user cancelled (and wantResults is set) or the call had none.
func request(ctx context.Context, conn *dbus.Conn, method string, wantResults bool, args ...any) (map[string]dbus.Variant, error) {
	token := newHandleToken()
	sender := strings.ReplaceAll(strings.TrimPrefix(conn.Names()[0], ":"), ".", "_")
	handle := dbus.ObjectPath(fmt.Sprintf("%s/request/%s/%s", objectPath, sender, token))

	if err := conn.AddMatchSignal(dbus.WithMatchObjectPath(handle), dbus.WithMatchInterface(requestIfc), dbus.WithMatchMember("Response")); err != nil {
		return nil, fmt.Errorf("portal subscription: %w", err)
	}
	signals := make(chan *dbus.Signal, 4)
	conn.Signal(signals)
	defer conn.RemoveSignal(signals)

	// The last argument of every portal method is its options dictionary.
	opts := args[len(args)-1].(map[string]dbus.Variant)
	opts["handle_token"] = dbus.MakeVariant(token)

	var got dbus.ObjectPath
	if err := conn.Object(busName, objectPath).CallWithContext(ctx, method, 0, args...).Store(&got); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if got != handle { // older portals ignore handle_token
		_ = conn.AddMatchSignal(dbus.WithMatchObjectPath(got), dbus.WithMatchInterface(requestIfc), dbus.WithMatchMember("Response"))
		handle = got
	}
	// Only the portal's own connection may answer: another client on the bus could otherwise
	// emit a Response that names a file the user never chose. (The call above activated the
	// portal, so it has an owner now.)
	var owner string
	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, busName).Store(&owner); err != nil {
		return nil, fmt.Errorf("portal owner: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			_ = conn.Object(busName, handle).Call(requestIfc+".Close", 0).Err
			return nil, ctx.Err()
		case sig := <-signals:
			if sig == nil || sig.Sender != owner || sig.Path != handle || sig.Name != requestIfc+".Response" || len(sig.Body) < 2 {
				continue
			}
			code, _ := sig.Body[0].(uint32)
			results, _ := sig.Body[1].(map[string]dbus.Variant)
			switch code {
			case 0:
				return results, nil
			case 1:
				if wantResults {
					return nil, nil
				}
				return nil, errCancelled
			default:
				return nil, fmt.Errorf("%s failed (portal response %d)", method, code)
			}
		}
	}
}

func newHandleToken() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "ipmi_connect_" + hex.EncodeToString(b)
}
