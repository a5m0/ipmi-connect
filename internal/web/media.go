package web

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/a5m0/ipmi-connect/internal/bmc"
	"github.com/a5m0/ipmi-connect/internal/diskimage"
)

// pickTimeout bounds how long the native file chooser may stay open.
const pickTimeout = 10 * time.Minute

// errNoRoots means virtual media has nowhere it may read images from.
var errNoRoots = badRequest("virtual media folders are not configured")

// mediaRoutes registers the virtual media API. Every path is checked against s.Media: only
// files in the allowed folders, or picked in the desktop's file chooser, can be used. Images are
// read-only unless the request explicitly confirms that the host may write to them.
func (s *Server) mediaRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/files", s.handleFiles)
	mux.HandleFunc("POST /api/media/pick", s.handlePick)
	mux.HandleFunc("GET /api/media", s.media(func(vm bmc.VirtualMedia, r *http.Request) error { return nil }))
	mux.HandleFunc("POST /api/media/images", s.mediaJSON(func(vm bmc.VirtualMedia, r *http.Request) error {
		var req struct {
			Path            string `json:"path"`
			ReadOnly        *bool  `json:"read_only"`
			ConfirmWritable bool   `json:"confirm_writable"`
		}
		if err := decode(r, &req); err != nil || req.Path == "" {
			return badRequest(`send {"path": "/full/path/to/image.iso"}`)
		}
		readOnly := req.ReadOnly == nil || *req.ReadOnly
		if !readOnly && !req.ConfirmWritable {
			return badRequest("writable images need confirmation: the host can change or erase them")
		}
		path, err := s.resolveMedia(req.Path)
		if err != nil {
			return err
		}
		_, err = vm.AddImage(path, readOnly)
		return err
	}))
	mux.HandleFunc("PATCH /api/media/images/{id}", s.mediaJSON(func(vm bmc.VirtualMedia, r *http.Request) error {
		var req struct {
			ReadOnly        *bool `json:"read_only"`
			ConfirmWritable bool  `json:"confirm_writable"`
		}
		if err := decode(r, &req); err != nil || req.ReadOnly == nil {
			return badRequest(`send {"read_only": true}`)
		}
		if !*req.ReadOnly && !req.ConfirmWritable {
			return badRequest("writable images need confirmation: the host can change or erase them")
		}
		return vm.SetImageReadOnly(r.PathValue("id"), *req.ReadOnly)
	}))
	mux.HandleFunc("DELETE /api/media/images/{id}", s.media(func(vm bmc.VirtualMedia, r *http.Request) error {
		return vm.RemoveImage(r.PathValue("id"))
	}))
	mux.HandleFunc("POST /api/media/drives/{target}", s.mediaJSON(func(vm bmc.VirtualMedia, r *http.Request) error {
		target, err := mediaTarget(r)
		if err != nil {
			return err
		}
		// Either an image already in the list, or a file to insert: it is added to the list
		// (or its entry reused) first. Read-only unless writing is explicitly confirmed.
		var req struct {
			ImageID         string `json:"image_id"`
			Path            string `json:"path"`
			ReadOnly        *bool  `json:"read_only"`
			ConfirmWritable bool   `json:"confirm_writable"`
		}
		if err := decode(r, &req); err != nil || (req.ImageID == "") == (req.Path == "") {
			return badRequest(`send {"image_id": "..."} or {"path": "/full/path/to/image.iso"}`)
		}
		id := req.ImageID
		if req.Path != "" {
			readOnly := req.ReadOnly == nil || *req.ReadOnly
			if !readOnly && !req.ConfirmWritable {
				return badRequest("writable images need confirmation: the host can change or erase them")
			}
			path, err := s.resolveMedia(req.Path)
			if err != nil {
				return err
			}
			if id, err = listedImage(vm, path, readOnly); err != nil {
				return err
			}
		}
		log.Printf("[Server] Mapping image %q to the %s drive", id, target)
		return vm.MapImage(id, target)
	}))
	mux.HandleFunc("DELETE /api/media/drives/{target}", s.media(func(vm bmc.VirtualMedia, r *http.Request) error {
		target, err := mediaTarget(r)
		if err != nil {
			return err
		}
		return vm.UnmapDrive(target)
	}))
	mux.HandleFunc("POST /api/media/create", s.mediaJSON(func(vm bmc.VirtualMedia, r *http.Request) error {
		var req struct {
			Path   string `json:"path"`
			Size   int64  `json:"size"`   // a blank image of this many bytes, or
			Folder string `json:"folder"` // an image holding a copy of this folder
			Label  string `json:"label"`
		}
		if err := decode(r, &req); err != nil || req.Path == "" || (req.Size > 0) == (req.Folder != "") {
			return badRequest(`send {"path": "/full/path/new.img", "size": bytes} or {"path": ..., "folder": "/full/path"}`)
		}
		if s.Media == nil {
			return errNoRoots
		}
		path, err := s.Media.ResolveNew(req.Path)
		if err != nil {
			return badRequest(err.Error())
		}
		if req.Folder != "" {
			folder, err := s.resolveMedia(req.Folder)
			if err != nil {
				return err
			}
			log.Printf("[Server] Creating an image from a folder")
			err = diskimage.CreateFromFolder(path, folder, req.Label)
			if err != nil {
				return err
			}
		} else {
			log.Printf("[Server] Creating a blank image")
			if err := diskimage.CreateBlank(path, req.Size, req.Label); err != nil {
				return err
			}
		}
		// The user asked for a new disk to write to: it is created writable.
		_, err = vm.AddImage(path, false)
		return err
	}))
	mux.HandleFunc("POST /api/media/usb-reset", s.media(func(vm bmc.VirtualMedia, r *http.Request) error {
		return vm.USBReset()
	}))
}

// listedImage returns the id of the image at path, adding it to the list if it is not there and
// setting its read-only flag as asked.
func listedImage(vm bmc.VirtualMedia, path string, readOnly bool) (string, error) {
	for _, im := range vm.MediaStatus().Images {
		if im.Path != path {
			continue
		}
		if im.ReadOnly != readOnly && im.MappedTo == "" {
			if err := vm.SetImageReadOnly(im.ID, readOnly); err != nil {
				return "", err
			}
		}
		return im.ID, nil
	}
	im, err := vm.AddImage(path, readOnly)
	return im.ID, err
}

// resolveMedia checks a path against the media roots and returns its real path.
func (s *Server) resolveMedia(path string) (string, error) {
	if s.Media == nil {
		return "", errNoRoots
	}
	real, err := s.Media.Resolve(path)
	if err != nil {
		return "", badRequest(err.Error())
	}
	return real, nil
}

// handlePick opens the desktop's file chooser (on the user's screen, outside the browser) and
// allows the chosen file. In a Flatpak this is how files outside the sandbox become available:
// the document portal exposes just the chosen file.
func (s *Server) handlePick(w http.ResponseWriter, r *http.Request) {
	if s.PickFile == nil || s.Media == nil {
		writeJSONError(w, http.StatusNotImplemented, badRequest("no desktop file chooser is available"))
		return
	}
	var req struct {
		Directory bool `json:"directory"`
	}
	if r.ContentLength != 0 {
		if !requireJSON(w, r) {
			return
		}
		_ = decode(r, &req)
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(pickTimeout + 10*time.Second))
	ctx, cancel := context.WithTimeout(r.Context(), pickTimeout)
	defer cancel()
	title := "Choose a disk image for virtual media"
	if req.Directory {
		title = "Choose a folder to copy into a new image"
	}
	paths, err := s.PickFile(ctx, title, req.Directory)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	var picked []string
	for _, p := range paths {
		real, err := s.Media.AddFile(p)
		if err != nil {
			continue
		}
		if req.Directory {
			s.Media.AddRoot(real)
		}
		picked = append(picked, real)
	}
	if picked == nil {
		picked = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"paths": picked})
}

type badRequest string

func (e badRequest) Error() string { return string(e) }

func mediaTarget(r *http.Request) (bmc.MediaTarget, error) {
	switch t := bmc.MediaTarget(r.PathValue("target")); t {
	case bmc.TargetCD, bmc.TargetRemovable, bmc.TargetFloppy:
		return t, nil
	}
	return "", badRequest("the drive must be cd, removable or floppy")
}

func decode(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

// media wraps a virtual media action: it answers with the full media status, or the action's
// error (400 for bad input, 502 for anything the console or BMC refused).
func (s *Server) media(action func(bmc.VirtualMedia, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vm, ok := s.console().(bmc.VirtualMedia)
		if !ok {
			http.Error(w, "virtual media is not supported for this BMC", http.StatusNotImplemented)
			return
		}
		if err := action(vm, r); err != nil {
			code := http.StatusBadGateway
			var bad badRequest
			if errors.As(err, &bad) {
				code = http.StatusBadRequest
			}
			writeJSONError(w, code, err)
			return
		}
		writeJSON(w, http.StatusOK, vm.MediaStatus())
	}
}

// mediaJSON is media for actions with a JSON body.
func (s *Server) mediaJSON(action func(bmc.VirtualMedia, *http.Request) error) http.HandlerFunc {
	h := s.media(action)
	return func(w http.ResponseWriter, r *http.Request) {
		if requireJSON(w, r) {
			h(w, r)
		}
	}
}

func writeJSONError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
