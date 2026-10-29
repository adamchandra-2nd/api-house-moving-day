package httpapi

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/jpeg"
	// Registers the PNG decoder for thumbnail(). WebP is not in the standard
	// library; a censored WebP therefore fails to shrink and is refused
	// outright, which errs the safe way -- it stays hidden.
	_ "image/png"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/adamchandra-2nd/api-house-moving-day/internal/model"
	"github.com/adamchandra-2nd/api-house-moving-day/internal/store"
)

// The shared gallery: guests share photos from their phone, the committee
// looks through them and can censor one.

const (
	// galleryMax is how many photos one guest may share. Mirrors GALLERY_MAX
	// in src/data/gallery.ts.
	galleryMax = 5

	// maxPhotoBytes is the ceiling on one upload.
	//
	// The app resizes before sending -- roughly 1600px, JPEG -- which lands
	// well under this. The limit is here for the case the app does not: five
	// phone originals per guest would be a gigabyte of database for an event
	// of thirty people, on a plan with 500MB.
	maxPhotoBytes = 2 << 20 // 2 MiB

	captionMax = 100
	reasonMax  = 100
)

type photoUploadBody struct {
	// Data is a data: URL, as the browser's FileReader produces. Base64 over
	// JSON rather than multipart because every other endpoint here is JSON
	// and a photo at this size does not justify a second content type.
	Data    string `json:"data"`
	Caption string `json:"caption,omitempty"`
	RoomID  string `json:"roomId,omitempty"`
}

// handlePhotos is the gallery as a guest sees it.
//
// `mine` is computed from the caller's token, and the committee's own notes
// -- why something was censored, whether it has been reviewed -- are stripped
// before the response is built. They are not for guests.
func (s *Server) handlePhotos(w http.ResponseWriter, r *http.Request) {
	g := guestOf(r)

	photos, err := s.store.Photos(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}

	out := make([]model.Photo, 0, len(photos))
	for _, p := range photos {
		p.Mine = p.GuestID == g.ID
		p.CensorReason = ""
		p.Checked = false
		out = append(out, p)
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAddPhoto(w http.ResponseWriter, r *http.Request) {
	g := guestOf(r)

	var body photoUploadBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}

	raw, mime, err := decodeDataURL(body.Data)
	if err != nil {
		s.badRequest(w, err.Error())
		return
	}
	if len(raw) > maxPhotoBytes {
		s.badRequest(w, "foto terlalu besar, maksimal "+strconv.Itoa(maxPhotoBytes>>20)+" MB setelah dikecilkan")
		return
	}

	caption := strings.TrimSpace(body.Caption)
	if len([]rune(caption)) > captionMax {
		s.badRequest(w, "keterangan terlalu panjang")
		return
	}

	s.idempotent(w, r, "photo:"+g.ID, func() (any, error) {
		p, err := s.store.AddPhoto(r.Context(), model.PhotoUpload{
			GuestID: g.ID,
			Author:  g.Name,
			RoomID:  body.RoomID,
			Caption: caption,
			At:      rundownMinute(time.Now()),
			Mime:    mime,
			Bytes:   raw,
		}, galleryMax)
		if err != nil {
			return nil, err
		}
		p.Mine = true
		s.invalidate(r.Context(), keyPhotos)
		return p, nil
	})
}

func (s *Server) handleDeletePhoto(w http.ResponseWriter, r *http.Request) {
	g := guestOf(r)
	id := r.PathValue("id")

	if err := s.store.DeletePhoto(r.Context(), id, g.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.invalidate(r.Context(), keyPhotos)
	s.writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// handlePhotoImage serves the bytes.
//
// A censored photo is served as a thumbnail small enough that nothing in it
// can be made out -- the full pixels never leave the server. The prototype
// blurred the tile in CSS, which hides a photo from someone looking at the
// screen and from nobody else: the image was still in the page, one "open in
// new tab" away. Censoring has to mean the picture does not arrive.
//
// The owner and the committee still get the original. The committee has to
// see what they are judging, and a guest censoring their own photo would be
// an odd thing to enforce against them.
func (s *Server) handlePhotoImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	photos, err := s.store.Photos(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var meta model.Photo
	for _, p := range photos {
		if p.ID == id {
			meta = p
			break
		}
	}
	if meta.ID == "" {
		s.fail(w, r, store.ErrNotFound)
		return
	}

	raw, mime, err := s.store.PhotoBytes(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	if meta.Censored && !s.maySeeFullPhoto(r, meta) {
		small, err := thumbnail(raw)
		if err != nil {
			// Rather than fall back to the full image, which would defeat the
			// censor entirely.
			s.writeJSON(w, http.StatusForbidden, errBody{Error: "foto ini disembunyikan panitia", Code: "censored"})
			return
		}
		raw, mime = small, "image/jpeg"
	}

	// The bytes for an id never change -- an edit is a new photo -- so this
	// can be cached hard. Private: a photo is for the people at this event.
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// maySeeFullPhoto is true for the photo's owner and for the committee.
func (s *Server) maySeeFullPhoto(r *http.Request, p model.Photo) bool {
	if g := guestOf(r); g.ID != "" && g.ID == p.GuestID {
		return true
	}
	return sessionOf(r).Role == "admin"
}

// thumbnail shrinks an image until nothing in it can be read.
//
// 24px on the long edge, by nearest-neighbour averaging. Deliberately crude:
// the point is that the detail is gone before the bytes leave the process,
// not that the result looks good. The app scales it back up with a blur, so
// the tile keeps the shape and colour the design wants.
func thumbnail(raw []byte) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	const side = 24
	w, h := side, side
	if b.Dx() > b.Dy() {
		h = max(1, side*b.Dy()/b.Dx())
	} else {
		w = max(1, side*b.Dx()/b.Dy())
	}

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			dst.Set(x, y, src.At(b.Min.X+x*b.Dx()/w, b.Min.Y+y*b.Dy()/h))
		}
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 60}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ---- the committee's side ----

// handleAdminPhotos is the same gallery with the committee's notes intact.
func (s *Server) handleAdminPhotos(w http.ResponseWriter, r *http.Request) {
	photos, err := s.store.Photos(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if photos == nil {
		photos = []model.Photo{}
	}
	s.writeJSON(w, http.StatusOK, photos)
}

type censorBody struct {
	IDs []string `json:"ids"`
	// Censored false lifts it, which is what "urungkan" does.
	Censored bool   `json:"censored"`
	Reason   string `json:"reason,omitempty"`
}

func (s *Server) handleCensorPhotos(w http.ResponseWriter, r *http.Request) {
	var body censorBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	if len(body.IDs) == 0 {
		s.badRequest(w, "pilih minimal satu foto")
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if len([]rune(reason)) > reasonMax {
		s.badRequest(w, "alasan terlalu panjang")
		return
	}

	if err := s.store.CensorPhotos(r.Context(), body.IDs, body.Censored, reason); err != nil {
		s.fail(w, r, err)
		return
	}
	s.invalidate(r.Context(), keyPhotos)
	s.writeJSON(w, http.StatusOK, map[string]any{"ids": body.IDs, "censored": body.Censored})
}

type checkedBody struct {
	IDs []string `json:"ids"`
}

func (s *Server) handlePhotosChecked(w http.ResponseWriter, r *http.Request) {
	var body checkedBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	if err := s.store.MarkPhotosChecked(r.Context(), body.IDs); err != nil {
		s.fail(w, r, err)
		return
	}
	s.invalidate(r.Context(), keyPhotos)
	s.writeJSON(w, http.StatusOK, map[string]any{"ids": body.IDs, "checked": true})
}

// decodeDataURL turns "data:image/jpeg;base64,...." into bytes and a type.
//
// Only the image types a phone's picker actually produces are accepted. An
// allowlist rather than trusting the prefix: whatever the client claims, this
// is what will be echoed back in Content-Type to every other guest's browser,
// and "image/svg+xml" there is a script that runs on this origin.
func decodeDataURL(s string) ([]byte, string, error) {
	const marker = ";base64,"
	i := strings.Index(s, marker)
	if !strings.HasPrefix(s, "data:") || i < 0 {
		return nil, "", errors.New("format foto tidak dikenal")
	}
	mime := s[len("data:"):i]
	switch mime {
	case "image/jpeg", "image/png", "image/webp":
	default:
		return nil, "", errors.New("format foto harus JPEG, PNG atau WebP")
	}

	raw, err := base64.StdEncoding.DecodeString(s[i+len(marker):])
	if err != nil {
		return nil, "", errors.New("foto tidak terbaca")
	}
	if len(raw) == 0 {
		return nil, "", errors.New("foto kosong")
	}
	return raw, mime, nil
}

// ---- character pictures for the kenalan cards ----

// maxSpriteBytes caps one character picture. The phone sends a cropped webp
// of a few tens of KB; anything near this is not a character picture.
const maxSpriteBytes = 512 << 10

type spriteBody struct {
	// Data is a data: URL, like a photo upload.
	Data string `json:"data"`
}

// handleSetSprite stores the picture of this guest's character. Their phone
// renders it from the 3D scene when they confirm the outfit; every other
// phone only ever gets the outfit's label, so without this a kenalan card
// said "Hoodie" over the bare character.
func (s *Server) handleSetSprite(w http.ResponseWriter, r *http.Request) {
	g := guestOf(r)
	var body spriteBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	raw, mime, err := decodeDataURL(body.Data)
	if err != nil {
		s.badRequest(w, err.Error())
		return
	}
	if len(raw) > maxSpriteBytes {
		s.badRequest(w, "gambar karakter terlalu besar")
		return
	}
	if err := s.store.SetSprite(r.Context(), g.ID, raw, mime); err != nil {
		s.fail(w, r, err)
		return
	}
	s.invalidate(r.Context(), keyGuests)
	w.WriteHeader(http.StatusNoContent)
}

// handleSprite serves a guest's character picture to another guest. The URL
// carries ?v=<spriteAt>, so a new upload is a new URL and the old one can be
// cached for as long as the browser likes.
func (s *Server) handleSprite(w http.ResponseWriter, r *http.Request) {
	raw, mime, err := s.store.Sprite(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=604800, immutable")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}
