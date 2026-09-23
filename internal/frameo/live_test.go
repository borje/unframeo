// SPDX-License-Identifier: GPL-3.0-or-later

//go:build live

// These tests talk to a real frame. They need a pairing first:
//
//	unframeo pair <the code on the frame>
//	go test -tags live ./internal/frameo -run TestLiveFrame -v
//
// The frame is taken from the stored configuration. Set UNFRAMEO_FRAME to choose
// one when several are paired, and UNFRAMEO_PHOTO to a file to run the transfer
// test, which puts a real photo on the frame.
package frameo_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/borje/unframeo/internal/config"
	"github.com/borje/unframeo/internal/frameo"
	"github.com/borje/unframeo/internal/sdg"
)

// connectLive opens a conversation with the paired frame.
func connectLive(t *testing.T) *frameo.Client {
	t.Helper()

	cfg, err := config.Load("")
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("nothing is paired, so there is no frame to test against: %v", err)
	}
	if err != nil {
		t.Fatalf("reading the configuration: %v", err)
	}
	name, peer, err := cfg.Resolve(os.Getenv("UNFRAMEO_FRAME"))
	if err != nil {
		t.Skipf("no frame to test against: %v", err)
	}
	id, err := cfg.Identity()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("connecting to %s at %s", name, peer)

	level := slog.LevelInfo
	if testing.Verbose() {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	g, err := sdg.Dial(ctx, sdg.FrameoServers, id, &sdg.Options{
		Logger:     log,
		ServerKeys: []sdg.Key{sdg.FrameoServerKey},
	})
	if err != nil {
		t.Fatalf("reaching the grid: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	p, err := g.Connect(ctx, peer, sdg.FrameoProtocol)
	if err != nil {
		t.Fatalf("reaching the frame: %v", err)
	}
	c := frameo.NewClient(p, &frameo.Options{Logger: log, Name: "unframeo live test"})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestLiveFrameInfo(t *testing.T) {
	c := connectLive(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	info, err := c.GetInfo(ctx)
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	t.Logf("name %q, placement %q, screen %dx%d, may view %t, may manage %t",
		info.GetName(), info.GetPlacement(), info.GetScreenWidth(), info.GetScreenHeight(),
		info.GetHasPermissionViewPhotos(), info.GetHasPermissionManagePhotos())

	if info.GetScreenWidth() == 0 || info.GetScreenHeight() == 0 {
		t.Error("the frame reported no screen size, which suggests the reply was misread")
	}
}

func TestLiveSendPhoto(t *testing.T) {
	path := os.Getenv("UNFRAMEO_PHOTO")
	if path == "" {
		t.Skip("set UNFRAMEO_PHOTO to a file to send it to the frame")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sending %s, %d bytes", path, len(data))

	c := connectLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	id, err := c.SendPhoto(ctx, frameo.Photo{Path: path, Caption: "sent by unframeo"})
	if err != nil {
		t.Fatalf("SendPhoto: %v", err)
	}
	t.Logf("the frame accepted the photo as %d", id)
}

// TestLiveSendPhotoSingleSegment sends the whole file as one message, letting
// the transport split it, instead of sending a series of segments. If the
// segmented form turns out not to work against a real frame, this is the other
// shape to try.
func TestLiveSendPhotoSingleSegment(t *testing.T) {
	path := os.Getenv("UNFRAMEO_PHOTO")
	if path == "" {
		t.Skip("set UNFRAMEO_PHOTO to a file to send it to the frame")
	}
	c := connectLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	id, err := c.SendPhoto(ctx, frameo.Photo{Path: path, SingleSegment: true, Caption: "one message"})
	if err != nil {
		t.Fatalf("SendPhoto: %v", err)
	}
	t.Logf("the frame accepted the photo as %d", id)
}

// TestLiveListMedia asks the real frame for its media listing. The message
// number (31) is confirmed: the frame recognises the request and answers with
// a genuine AllMediaMetaData reply. Whether that reply carries a usable
// listing depends on this pairing's permissions (see `unframeo info`), which is
// why a refusal only logs and skips rather than failing outright.
func TestLiveListMedia(t *testing.T) {
	c := connectLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	items, err := c.ListMedia(ctx)
	if err != nil {
		t.Logf("listing failed: %v", err)
		t.Skip("the frame answered but refused; this pairing may lack view/manage permission")
	}
	t.Logf("the frame listed %d item(s)", len(items))
	for i, m := range items {
		if i >= 5 {
			t.Logf("  and %d more", len(items)-i)
			break
		}
		t.Logf("  id %d, taken %s, visible %t",
			m.GetMediaId(), time.UnixMilli(m.GetCaptureDate()).UTC().Format(time.DateOnly), m.GetIsVisible())
	}
}

// TestLiveRoundTrip sends a photo and then looks for it in a listing, which is
// the check that the two halves agree about identifiers.
func TestLiveRoundTrip(t *testing.T) {
	path := os.Getenv("UNFRAMEO_PHOTO")
	if path == "" {
		t.Skip("set UNFRAMEO_PHOTO to a file to run the round trip")
	}
	c := connectLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	before, err := c.ListMedia(ctx)
	if err != nil {
		t.Skipf("cannot list media yet: %v", err)
	}
	id, err := c.SendPhoto(ctx, frameo.Photo{Path: path, Caption: "round trip"})
	if err != nil {
		t.Fatalf("SendPhoto: %v", err)
	}
	after, err := c.ListMedia(ctx)
	if err != nil {
		t.Fatalf("listing after sending: %v", err)
	}
	t.Logf("the frame held %d item(s) before and %d after; we sent %d", len(before), len(after), id)

	var found bool
	for _, m := range after {
		if m.GetMediaId() == id {
			found = true
		}
	}
	if !found {
		t.Logf("the frame files photos under its own identifiers, not the one we chose")
	}
}

// TestLiveGetMedia downloads a photo the frame already holds. It is also the
// experiment the protocol notes ask for, so it reports what it saw rather than
// only whether it worked: whether the frame echoes the id it was asked for,
// whether a full-resolution reply appends a thumbnail after the photo, and how
// long the transfer took against the six seconds allowed for it.
func TestLiveGetMedia(t *testing.T) {
	c := connectLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	items, err := c.ListMedia(ctx)
	if err != nil {
		t.Skipf("cannot list media: %v", err)
	}
	if len(items) == 0 {
		t.Skip("the frame holds no photos to fetch")
	}
	id := items[0].GetMediaId()

	start := time.Now()
	got, err := c.GetMedia(ctx, frameo.Fetch{ID: id})
	took := time.Since(start)
	if err != nil {
		t.Fatalf("GetMedia(%d): %v", id, err)
	}

	t.Logf("photo %d: %d bytes as %q in %s", id, len(got.Data), got.Extension(), took)
	t.Logf("  the header names photo %d (0 would mean the frame does not echo the id)", got.Media.GetId())
	if d := got.Media.GetCaptureDate(); d > 0 {
		t.Logf("  capture date %s, which is what the file is named after", time.UnixMilli(d).UTC())
	} else {
		t.Log("  no capture date: downloads will be named by id alone")
	}
	if cap := got.Media.GetCaption(); cap != "" {
		t.Logf("  caption %q", cap)
	}
	t.Logf("  extra streams: %d, thumbnail bytes received: %d", len(got.Media.GetExtra()), len(got.Thumbnail))
	for i, e := range got.Media.GetExtra() {
		t.Logf("  extra[%d]: %d bytes, %q", i, e.GetSize(), e.GetFileExtension())
	}
	if got.Media.GetId() != 0 && got.Media.GetId() != id {
		t.Errorf("asked for photo %d and the header names %d", id, got.Media.GetId())
	}

	if dir := os.Getenv("UNFRAMEO_OUT"); dir != "" {
		path := filepath.Join(dir, fmt.Sprintf("%d.%s", id, got.Extension()))
		if err := os.WriteFile(path, got.Data, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s; open it to confirm it is a photo", path)
	}
}

// TestLiveGetMediaSizes fetches the same photo both ways and checks that the
// two sizes are actually two copies. It replaces a test that asked for a
// scaled copy and asserted nothing about what came back, which is how the
// frame's real behaviour went unnoticed for so long.
func TestLiveGetMediaSizes(t *testing.T) {
	c := connectLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	items, err := c.ListMedia(ctx)
	if err != nil {
		t.Skipf("cannot list media: %v", err)
	}
	if len(items) == 0 {
		t.Skip("the frame holds no photos to fetch")
	}
	id := items[0].GetMediaId()

	full, err := c.GetMedia(ctx, frameo.Fetch{ID: id})
	if err != nil {
		t.Fatalf("GetMedia(%d) at full size: %v", id, err)
	}
	preview, err := c.GetMedia(ctx, frameo.Fetch{ID: id, Size: frameo.SizePreview})
	if err != nil {
		t.Fatalf("GetMedia(%d) at preview size: %v", id, err)
	}

	t.Logf("photo %d full:    %d bytes, %dx%d, %q", id, len(full.Data), full.Width, full.Height, full.Extension())
	t.Logf("photo %d preview: %d bytes, %dx%d, %q", id, len(preview.Data), preview.Width, preview.Height, preview.Extension())

	// A frame that cannot be measured is a finding rather than a flake: it
	// means this client has met a format its parser does not know.
	if full.Width == 0 || preview.Width == 0 {
		t.Errorf("a copy could not be measured, so imageSize does not know this frame's format: full %q, preview %q",
			full.Extension(), preview.Extension())
	}
	if bytes.Equal(full.Data, preview.Data) {
		t.Fatalf("both sizes returned the identical %d bytes: this frame keeps one copy, or the bound is ignored", len(full.Data))
	}
	if len(preview.Data) >= len(full.Data) {
		t.Errorf("the preview is %d bytes against the photo's %d", len(preview.Data), len(full.Data))
	}
	if max(preview.Width, preview.Height) >= max(full.Width, full.Height) {
		t.Errorf("the preview is %dx%d against the photo's %dx%d",
			preview.Width, preview.Height, full.Width, full.Height)
	}
}

// TestLiveGetMediaCutoff finds where this frame stops handing out the preview
// and starts handing out the photo. On the frame this client was built against
// it is 500, which is a probe's answer and not a documented rule: anyone who
// gets a different number here should record it beside frameo.Size and reach
// their own frame with the numeric form of -size.
func TestLiveGetMediaCutoff(t *testing.T) {
	c := connectLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	items, err := c.ListMedia(ctx)
	if err != nil {
		t.Skipf("cannot list media: %v", err)
	}
	if len(items) == 0 {
		t.Skip("the frame holds no photos to fetch")
	}
	id := items[0].GetMediaId()

	at := func(size frameo.Size) *frameo.Download {
		t.Helper()
		got, err := c.GetMedia(ctx, frameo.Fetch{ID: id, Size: size})
		if err != nil {
			t.Fatalf("GetMedia(%d) at %d: %v", id, size, err)
		}
		t.Logf("  bound %5d: %7d bytes, %dx%d", size, len(got.Data), got.Width, got.Height)
		return got
	}

	small, large := frameo.Size(1), frameo.Size(4096)
	lo, hi := at(small), at(large)
	if len(lo.Data) == len(hi.Data) {
		t.Skipf("this frame answers %d and %d alike, so it keeps one copy", small, large)
	}

	// Halve the gap until the two bounds sit next to each other; the cutoff is
	// then the lower of the two that still fetches the photo.
	for large-small > 1 {
		mid := small + (large-small)/2
		if len(at(mid).Data) == len(lo.Data) {
			small = mid
		} else {
			large = mid
		}
	}
	t.Logf("this frame serves the preview below %d and the photo from %d up", large, large)
	if large != 500 {
		t.Logf("that is not the 500 measured when this was written; worth recording beside frameo.Size")
	}
}

// TestLiveGetMediaRoundTrip sends a photo and reads it straight back, which is
// the check that the two directions agree: the same id, and the same bytes if
// the frame stores what it is given rather than re-encoding it.
func TestLiveGetMediaRoundTrip(t *testing.T) {
	path := os.Getenv("UNFRAMEO_PHOTO")
	if path == "" {
		t.Skip("set UNFRAMEO_PHOTO to a file to run the round trip")
	}
	sent, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c := connectLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	id, err := c.SendPhoto(ctx, frameo.Photo{Path: path, Caption: "round trip"})
	if err != nil {
		t.Fatalf("SendPhoto: %v", err)
	}
	got, err := c.GetMedia(ctx, frameo.Fetch{ID: id})
	if err != nil {
		t.Fatalf("GetMedia(%d): %v", id, err)
	}
	if bytes.Equal(got.Data, sent) {
		t.Logf("the frame returned the %d bytes it was given, unchanged", len(sent))
		return
	}
	t.Logf("sent %d bytes and %d came back: the frame re-encodes what it stores",
		len(sent), len(got.Data))
}

// TestLiveRequestPermission sends a real permission request (27) and waits for
// someone to tap Allow on the frame. It needs a person there, so it runs only
// when UNFRAMEO_ASK_PERMISSION is set; the message number is read from
// yasoob/frameo-client and this is the test that confirms it. Whether the
// frame also learned this client's name is visible in the -v log: the
// introduction goes out in answer to the frame's own GetInfo, which every
// connection begins with.
func TestLiveRequestPermission(t *testing.T) {
	if os.Getenv("UNFRAMEO_ASK_PERMISSION") == "" {
		t.Skip("set UNFRAMEO_ASK_PERMISSION=1, and stand at the frame, to test asking for permission")
	}
	c := connectLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	before, err := c.GetInfo(ctx)
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	t.Logf("before: may view %t, may manage %t", before.GetHasPermissionViewPhotos(), before.GetHasPermissionManagePhotos())
	if frameo.Granted(before, frameo.PermissionManage) {
		t.Skip("this pairing already has every permission; remove it on the frame to test asking")
	}

	t.Log("asking to manage photos; tap Allow on the frame")
	if err := c.RequestPermission(ctx, frameo.PermissionManage); err != nil {
		t.Fatalf("RequestPermission: %v", err)
	}
	after, err := c.GetInfo(ctx)
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	t.Logf("after: may view %t, may manage %t", after.GetHasPermissionViewPhotos(), after.GetHasPermissionManagePhotos())
}
