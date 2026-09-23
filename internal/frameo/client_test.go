// SPDX-License-Identifier: GPL-3.0-or-later

package frameo_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/borje/unframeo/internal/frameo"
	"github.com/borje/unframeo/internal/frameo/frameotest"
	"github.com/borje/unframeo/internal/frameo/pb"
	"github.com/borje/unframeo/internal/sdg"
	"github.com/borje/unframeo/internal/sdg/sdgtest"
	"google.golang.org/protobuf/proto"
)

// setup wires a real client to a fake frame through the real transport and a
// fake grid, so every layer under test is the one that ships.
func setup(t *testing.T, frame *frameotest.Frame) *frameo.Client {
	t.Helper()
	return setupWith(t, frame, nil)
}

// setupWith is setup for a client that needs options, such as a name.
func setupWith(t *testing.T, frame *frameotest.Frame, opts *frameo.Options) *frameo.Client {
	t.Helper()

	grid, err := sdgtest.NewGrid()
	if err != nil {
		t.Fatalf("start fake grid: %v", err)
	}
	t.Cleanup(func() {
		if err := grid.Close(); err != nil {
			t.Errorf("fake grid reported: %v", err)
		}
	})

	frameErrs := make(chan error, 4)
	peerID, err := grid.AddDevice(func(tun *sdgtest.Tunnel) {
		if err := frame.Serve(tun); err != nil {
			frameErrs <- err
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case err := <-frameErrs:
			t.Errorf("fake frame reported: %v", err)
		default:
		}
	})

	id, err := sdg.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	g, err := sdg.Dial(ctx, []sdg.Endpoint{{Host: grid.Host(), Port: grid.Port()}}, id, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	p, err := g.Connect(ctx, sdg.PeerID(peerID), sdg.FrameoProtocol)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	c := frameo.NewClient(p, opts)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// writePhoto creates a file of pseudo-random bytes standing in for a photo.
// photoBytes makes pseudo-random bytes that begin and end the way a JPEG does,
// because the client checks that a photo it downloaded is one. Sizes too small
// to hold the markers are left as they are; nothing downloads those.
func photoBytes(size int) []byte {
	data := make([]byte, size)
	rnd := rand.New(rand.NewPCG(7, uint64(size)))
	for i := range data {
		data[i] = byte(rnd.UintN(256))
	}
	if size >= 4 {
		copy(data, []byte{0xff, 0xd8, 0xff})
		copy(data[size-2:], []byte{0xff, 0xd9})
	}
	return data
}

func writePhoto(t *testing.T, size int) string {
	t.Helper()
	data := photoBytes(size)
	path := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGetInfo(t *testing.T) {
	frame := frameotest.New()
	c := setup(t, frame)

	info, err := c.GetInfo(testCtx(t))
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if info.GetName() != "Test Frame" {
		t.Errorf("name = %q", info.GetName())
	}
	if info.GetScreenWidth() != 1280 || info.GetScreenHeight() != 800 {
		t.Errorf("screen = %dx%d", info.GetScreenWidth(), info.GetScreenHeight())
	}
}

func TestSendPhoto(t *testing.T) {
	// Sizes either side of a segment boundary, plus one large enough that the
	// header alone would not fit in a single transport message.
	for _, size := range []int{1, 16000, 16001, 100 << 10} {
		frame := frameotest.New()
		c := setup(t, frame)
		path := writePhoto(t, size)
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		id, err := c.SendPhoto(testCtx(t), frameo.Photo{Path: path, Caption: "hello"})
		if err != nil {
			t.Fatalf("size %d: SendPhoto: %v", size, err)
		}

		photos := frame.Photos()
		if len(photos) != 1 {
			t.Fatalf("size %d: frame holds %d photos, want 1", size, len(photos))
		}
		got := photos[0]
		if !bytes.Equal(got.Data, want) {
			t.Errorf("size %d: the frame received different bytes", size)
		}
		if got.Media.GetId() != id {
			t.Errorf("size %d: frame filed the photo under %d, we reported %d", size, got.Media.GetId(), id)
		}
		if int(got.Media.GetSize()) != size {
			t.Errorf("size %d: header declared %d bytes", size, got.Media.GetSize())
		}
		if got.Media.GetCaption() != "hello" {
			t.Errorf("size %d: caption = %q", size, got.Media.GetCaption())
		}
		if got.Media.GetFileExtension() != "jpg" {
			t.Errorf("size %d: extension = %q", size, got.Media.GetFileExtension())
		}
	}
}

func TestSendPhotoSegmentsByDefault(t *testing.T) {
	frame := frameotest.New()
	c := setup(t, frame)
	path := writePhoto(t, 50<<10)

	if _, err := c.SendPhoto(testCtx(t), frameo.Photo{Path: path}); err != nil {
		t.Fatal(err)
	}
	// 50 KiB at 16000 bytes per segment is four segments, matching how the app
	// splits a file rather than relying on the transport to split it.
	if got := frame.Segments(); got != 4 {
		t.Errorf("frame saw %d segments, want 4", got)
	}
}

func TestSendPhotoSingleSegment(t *testing.T) {
	frame := frameotest.New()
	c := setup(t, frame)
	path := writePhoto(t, 50<<10)
	want, _ := os.ReadFile(path)

	if _, err := c.SendPhoto(testCtx(t), frameo.Photo{Path: path, SingleSegment: true}); err != nil {
		t.Fatal(err)
	}
	if got := frame.Segments(); got != 1 {
		t.Errorf("frame saw %d segments, want 1", got)
	}
	photos := frame.Photos()
	if len(photos) != 1 || !bytes.Equal(photos[0].Data, want) {
		t.Error("the photo did not survive being sent as one oversized message")
	}
}

func TestSendPhotoMetadata(t *testing.T) {
	frame := frameotest.New()
	c := setup(t, frame)
	path := writePhoto(t, 1000)
	taken := time.Date(2021, 6, 5, 12, 0, 0, 0, time.UTC)

	if _, err := c.SendPhoto(testCtx(t), frameo.Photo{
		Path: path, Taken: taken, Fit: true, Center: &[2]float32{0.25, 0.75},
	}); err != nil {
		t.Fatal(err)
	}
	m := frame.Photos()[0].Media
	if m.GetCaptureDate() != taken.UnixMilli() {
		t.Errorf("capture date = %d, want %d", m.GetCaptureDate(), taken.UnixMilli())
	}
	if m.GetScaleType() != 1 { // fit inside
		t.Errorf("scale type = %v, want fit inside", m.GetScaleType())
	}
	if m.GetCenterPointX() != 0.25 || m.GetCenterPointY() != 0.75 {
		t.Errorf("centre point = %v,%v", m.GetCenterPointX(), m.GetCenterPointY())
	}
}

// A photo that did not come off the disk under its own name has to be able to
// say what format it is in, rather than the caller having to spell it into the
// name of whatever file it was written to.
func TestSendPhotoFileExtension(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct{ file, stated, want string }{
		// Nothing stated, so the name answers, and answers jpg when it cannot.
		{"photo.jpg", "", "jpg"},
		{"photo.png", "", "png"},
		{"photo", "", "jpg"},
		// Stated, and it wins: the file may be a temporary one named for
		// nothing in particular.
		{"photo.jpg", "png", "png"},
		{"photo-12345", "webp", "webp"},
		// Put in the form the frame files photos under either way.
		{"photo.JPEG", "", "jpg"},
		{"photo", ".JPEG", "jpg"},
	} {
		frame := frameotest.New()
		client := setup(t, frame)
		path := filepath.Join(dir, c.file)
		if err := os.WriteFile(path, photoBytes(500), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := client.SendPhoto(testCtx(t), frameo.Photo{Path: path, Extension: c.stated}); err != nil {
			t.Fatal(err)
		}
		if got := frame.Photos()[0].Media.GetFileExtension(); got != c.want {
			t.Errorf("%s stated as %q was filed as %q, want %q", c.file, c.stated, got, c.want)
		}
	}
}

func TestSendPhotoDefaultsCaptureDateToFileTime(t *testing.T) {
	frame := frameotest.New()
	c := setup(t, frame)
	path := writePhoto(t, 500)
	when := time.Date(2019, 3, 2, 9, 30, 0, 0, time.UTC)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}

	if _, err := c.SendPhoto(testCtx(t), frameo.Photo{Path: path}); err != nil {
		t.Fatal(err)
	}
	if got := frame.Photos()[0].Media.GetCaptureDate(); got != when.UnixMilli() {
		t.Errorf("capture date = %d, want the file's own time %d", got, when.UnixMilli())
	}
}

func TestSendPhotoReportsFrameError(t *testing.T) {
	frame := frameotest.New()
	frame.RefuseTransfers = true
	c := setup(t, frame)
	path := writePhoto(t, 2000)

	_, err := c.SendPhoto(testCtx(t), frameo.Photo{Path: path})
	if err == nil {
		t.Fatal("want an error when the frame refuses the transfer")
	}
	t.Logf("reported: %v", err)
}

func TestSendPhotoTimesOutWithoutAck(t *testing.T) {
	frame := frameotest.New()
	frame.DropAck = true
	c := setup(t, frame)
	path := writePhoto(t, 1000)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := c.SendPhoto(ctx, frameo.Photo{Path: path}); err == nil {
		t.Error("want an error when the frame never confirms")
	}
}

func TestSendPhotoRejectsMissingFile(t *testing.T) {
	frame := frameotest.New()
	c := setup(t, frame)
	if _, err := c.SendPhoto(testCtx(t), frameo.Photo{Path: "/nonexistent/photo.jpg"}); err == nil {
		t.Error("want an error for a missing file")
	}
}

func TestSendPhotoRejectsEmptyFile(t *testing.T) {
	frame := frameotest.New()
	c := setup(t, frame)
	path := filepath.Join(t.TempDir(), "empty.jpg")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendPhoto(testCtx(t), frameo.Photo{Path: path}); err == nil {
		t.Error("want an error for an empty file")
	}
}

func TestSendSeveralPhotos(t *testing.T) {
	frame := frameotest.New()
	c := setup(t, frame)
	ctx := testCtx(t)

	for i := range 3 {
		path := writePhoto(t, 20000+i)
		if _, err := c.SendPhoto(ctx, frameo.Photo{Path: path}); err != nil {
			t.Fatalf("photo %d: %v", i, err)
		}
	}
	if got := len(frame.Photos()); got != 3 {
		t.Errorf("frame holds %d photos, want 3", got)
	}
	ids := map[int64]bool{}
	for _, p := range frame.Photos() {
		if ids[p.Media.GetId()] {
			t.Errorf("two photos share the id %d", p.Media.GetId())
		}
		ids[p.Media.GetId()] = true
	}
}

func TestDeleteMedia(t *testing.T) {
	frame := frameotest.New()
	frame.DeleteType = frameo.TypeDeleteMedia
	c := setup(t, frame)
	ctx := testCtx(t)

	if err := c.DeleteMedia(ctx, []int64{1, 2}); err != nil {
		t.Fatalf("DeleteMedia: %v", err)
	}
	if got := frame.Deleted(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("frame was asked to delete %v, want [1 2]", got)
	}
	// Nothing to do is not an error, and must not reach the frame.
	if err := c.DeleteMedia(ctx, nil); err != nil {
		t.Errorf("DeleteMedia with no ids = %v, want nil", err)
	}
	if got := frame.Deleted(); len(got) != 2 {
		t.Errorf("an empty deletion still reached the frame: %v", got)
	}
}

// Hiding a photo keeps it on the frame, which is what separates it from
// deleting: the listing still reports the photo, marked hidden.
func TestSetMediaVisible(t *testing.T) {
	frame := frameotest.New()
	frame.ListType = frameo.TypeGetAllMediaMetaData
	frame.VisibilityType = frameo.TypeChangeMediaVisibility
	frame.Library = []*pb.MediaMetaData{
		{MediaId: 1, IsVisible: true},
		{MediaId: 2, IsVisible: true},
	}
	c := setup(t, frame)
	ctx := testCtx(t)

	if err := c.SetMediaVisible(ctx, []int64{1}, false); err != nil {
		t.Fatalf("SetMediaVisible: %v", err)
	}
	items, err := c.ListMedia(ctx)
	if err != nil {
		t.Fatalf("ListMedia: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want the hidden photo to still be listed", len(items))
	}
	if items[0].GetIsVisible() {
		t.Errorf("photo 1 is still visible")
	}
	if !items[1].GetIsVisible() {
		t.Errorf("photo 2 was hidden too, and should not have been")
	}

	if err := c.SetMediaVisible(ctx, []int64{1}, true); err != nil {
		t.Fatalf("SetMediaVisible back: %v", err)
	}
	if items, err = c.ListMedia(ctx); err != nil {
		t.Fatalf("ListMedia: %v", err)
	}
	if !items[0].GetIsVisible() {
		t.Errorf("photo 1 was not shown again")
	}
}

func TestListMedia(t *testing.T) {
	frame := frameotest.New()
	frame.ListType = frameo.TypeGetAllMediaMetaData
	frame.Library = []*pb.MediaMetaData{
		{MediaId: 1, CaptureDate: 1000, IsVisible: true},
		{MediaId: 2, CaptureDate: 2000, IsVisible: false},
	}
	c := setup(t, frame)

	items, err := c.ListMedia(testCtx(t))
	if err != nil {
		t.Fatalf("ListMedia: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].GetMediaId() != 1 || items[1].GetMediaId() != 2 {
		t.Errorf("items = %+v, want ids 1 and 2 in order", items)
	}
}

func TestSendRawReachesTheFrame(t *testing.T) {
	frame := frameotest.New()
	c := setup(t, frame)

	if err := c.SendRaw(testCtx(t), 99, nil); err != nil {
		t.Fatalf("SendRaw: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		if got := frame.UnknownTypes(); len(got) > 0 {
			if got[0] != 99 {
				t.Errorf("frame saw message number %d, want 99", got[0])
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("the frame never saw the message")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestOperationFailsAfterClose(t *testing.T) {
	frame := frameotest.New()
	c := setup(t, frame)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetInfo(testCtx(t)); err == nil {
		t.Error("want an error after closing")
	}
}

func TestSendPhotoCropFocus(t *testing.T) {
	t.Run("defaults to the middle", func(t *testing.T) {
		frame := frameotest.New()
		c := setup(t, frame)
		if _, err := c.SendPhoto(testCtx(t), frameo.Photo{Path: writePhoto(t, 500)}); err != nil {
			t.Fatal(err)
		}
		m := frame.Photos()[0].Media
		if m.GetCenterPointX() != 0.5 || m.GetCenterPointY() != 0.5 {
			t.Errorf("centre point = %v,%v, want 0.5,0.5", m.GetCenterPointX(), m.GetCenterPointY())
		}
	})

	// The top-left corner is a legitimate focus and must not be mistaken for
	// "not specified".
	t.Run("top left corner", func(t *testing.T) {
		frame := frameotest.New()
		c := setup(t, frame)
		photo := frameo.Photo{Path: writePhoto(t, 500), Center: &[2]float32{0, 0}}
		if _, err := c.SendPhoto(testCtx(t), photo); err != nil {
			t.Fatal(err)
		}
		m := frame.Photos()[0].Media
		if m.GetCenterPointX() != 0 || m.GetCenterPointY() != 0 {
			t.Errorf("centre point = %v,%v, want 0,0", m.GetCenterPointX(), m.GetCenterPointY())
		}
	})
}

// A failed acknowledgement belongs to one transfer. It must not end an
// unrelated wait, or sending several photos would report the wrong one as
// having failed.
func TestUnrelatedFailedAckDoesNotDerailAnotherWait(t *testing.T) {
	frame := frameotest.New()
	c := setup(t, frame)
	ctx := testCtx(t)

	// A stray failure arrives before the request we care about is answered.
	if err := c.SendRaw(ctx, 6, mustAck(t, 999999, 7)); err != nil {
		t.Fatal(err)
	}
	info, err := c.GetInfo(ctx)
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if info.GetName() != "Test Frame" {
		t.Errorf("name = %q", info.GetName())
	}
}

// mustAck builds an acknowledgement carrying an error, as the frame would.
func mustAck(t *testing.T, id int64, code int32) []byte {
	t.Helper()
	b, err := proto.Marshal(&pb.AcknowledgeReceipt{
		AcknowledgeId: id,
		Error:         &pb.Error{Code: pb.Error_Code(code)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// servingFrame is a frame holding one photo under the given id.
func servingFrame(id int64, data []byte) *frameotest.Frame {
	frame := frameotest.New()
	frame.GetMediaType = frameo.TypeGetMedia
	frame.Servable = map[int64]frameotest.Servable{
		id: {Data: data, Extension: "jpg"},
	}
	return frame
}

func TestGetMedia(t *testing.T) {
	// The sizes straddle both boundaries that matter: the segment size the
	// frame might chunk at, and the largest message the transport carries,
	// above which a reply arrives in parts and has to be put back together.
	for _, size := range []int{4, 16000, 16001, 16416, 16417, 100 << 10} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			want := photoBytes(size)
			frame := servingFrame(7, want)
			c := setup(t, frame)

			got, err := c.GetMedia(testCtx(t), frameo.Fetch{ID: 7})
			if err != nil {
				t.Fatalf("GetMedia: %v", err)
			}
			if !bytes.Equal(got.Data, want) {
				t.Errorf("got %d bytes, want %d, and they differ", len(got.Data), len(want))
			}
			if got.Extension() != "jpg" {
				t.Errorf("extension = %q, want jpg", got.Extension())
			}
			if got.Media.GetId() != 7 {
				t.Errorf("header names photo %d, want 7", got.Media.GetId())
			}
			if len(got.Thumbnail) != 0 {
				t.Errorf("got %d thumbnail bytes, want none", len(got.Thumbnail))
			}
		})
	}
}

// TestGetMediaInManySegments covers the other shape a frame might use: the
// photo arriving as a stream of small messages rather than one large one.
func TestGetMediaInManySegments(t *testing.T) {
	want := photoBytes(50000)
	frame := servingFrame(7, want)
	frame.ServeSegmentSize = 1000
	c := setup(t, frame)

	got, err := c.GetMedia(testCtx(t), frameo.Fetch{ID: 7})
	if err != nil {
		t.Fatalf("GetMedia: %v", err)
	}
	if !bytes.Equal(got.Data, want) {
		t.Errorf("the photo came back as %d bytes and differs", len(got.Data))
	}
}

// TestGetMediaThumbnail checks the layout the protocol notes were least sure
// of: the thumbnail following the photo in the same stream, described only by
// a byte count in the header.
func TestGetMediaThumbnail(t *testing.T) {
	want := photoBytes(20000)
	thumb := photoBytes(3000)
	frame := servingFrame(7, want)
	frame.Servable[7] = frameotest.Servable{
		Data: want, Extension: "jpg",
		Thumbnail: thumb, ThumbnailExtension: "jpg",
	}
	c := setup(t, frame)

	got, err := c.GetMedia(testCtx(t), frameo.Fetch{ID: 7})
	if err != nil {
		t.Fatalf("GetMedia: %v", err)
	}
	if !bytes.Equal(got.Data, want) {
		t.Errorf("the photo is not the %d bytes that were sent", len(want))
	}
	if !bytes.Equal(got.Thumbnail, thumb) {
		t.Errorf("the thumbnail is %d bytes, want %d", len(got.Thumbnail), len(thumb))
	}
}

// TestGetMediaAsksForTheSizeItWasGiven checks that a bound reaches the wire
// untouched, in both width and height. The client must not interpret the
// number: which copy it selects is the frame's decision, and a frame that
// divides them somewhere other than this one did is reached by passing a bare
// number through.
func TestGetMediaAsksForTheSizeItWasGiven(t *testing.T) {
	for _, want := range []frameo.Size{frameo.SizeFull, frameo.SizePreview, 499, 500, 1024} {
		frame := servingFrame(7, photoBytes(5000))
		c := setup(t, frame)

		if _, err := c.GetMedia(testCtx(t), frameo.Fetch{ID: 7, Size: want}); err != nil {
			t.Fatalf("GetMedia at %d: %v", want, err)
		}
		reqs := frame.Requests()
		if len(reqs) != 1 {
			t.Fatalf("the frame saw %d requests, want 1", len(reqs))
		}
		if got := frameo.Size(reqs[0].GetWidth()); got != want {
			t.Errorf("asked for a width of %d, want %d", got, want)
		}
		if got := frameo.Size(reqs[0].GetHeight()); got != want {
			t.Errorf("asked for a height of %d, want %d", got, want)
		}
	}
}

// TestGetMediaPreviewIsADifferentCopy is what no test could reach before: the
// frame keeps two copies of a photo and the bound chooses between them, so the
// bytes that come back differ by which was asked for. Until the stand-in frame
// could do this, nothing about size selection was exercised at all.
func TestGetMediaPreviewIsADifferentCopy(t *testing.T) {
	photo := frameotest.WebPExtended(2880, 1920, 40000)
	preview := frameotest.WebP(570, 380, 5000)
	for _, c := range []struct {
		size  frameo.Size
		want  []byte
		w, h  int
		which string
	}{
		{frameo.SizeFull, photo, 2880, 1920, "the photo"},
		{frameo.SizePreview, preview, 570, 380, "the preview"},
		// The boundary itself, from either side.
		{499, preview, 570, 380, "the preview"},
		{500, photo, 2880, 1920, "the photo"},
	} {
		frame := frameotest.New()
		frame.GetMediaType = frameo.TypeGetMedia
		frame.PreviewBelow = 500
		frame.Servable = map[int64]frameotest.Servable{
			7: {Data: photo, Extension: "webp", Preview: preview},
		}
		client := setup(t, frame)

		got, err := client.GetMedia(testCtx(t), frameo.Fetch{ID: 7, Size: c.size})
		if err != nil {
			t.Fatalf("GetMedia at %d: %v", c.size, err)
		}
		if !bytes.Equal(got.Data, c.want) {
			t.Errorf("a bound of %d brought back %d bytes, want %s at %d",
				c.size, len(got.Data), c.which, len(c.want))
		}
		if got.Width != c.w || got.Height != c.h {
			t.Errorf("a bound of %d measured %dx%d, want %dx%d", c.size, got.Width, got.Height, c.w, c.h)
		}
	}
}

// TestGetMediaMeasuresWhatArrived covers the dimensions being read from the
// photo itself, since the frame states them nowhere. A format that cannot be
// measured says nothing and must still download: the measurement is for one
// line of output and is not allowed to fail a transfer.
func TestGetMediaMeasuresWhatArrived(t *testing.T) {
	for _, c := range []struct {
		name string
		ext  string
		data []byte
		w, h int
	}{
		{"lossy webp", "webp", frameotest.WebP(570, 380, 6000), 570, 380},
		{"extended webp", "webp", frameotest.WebPExtended(1620, 1080, 6000), 1620, 1080},
		{"baseline jpeg", "jpg", frameotest.JPEG(1067, 712, 6000), 1067, 712},
		{"progressive jpeg", "jpg", frameotest.ProgressiveJPEG(1067, 1600, 6000), 1067, 1600},
		// Bytes in a format nothing here can read. The photo is whole and the
		// download succeeds; only the dimensions are unknown.
		{"something else", "webp", photoBytes(6000), 0, 0},
	} {
		frame := frameotest.New()
		frame.GetMediaType = frameo.TypeGetMedia
		frame.Servable = map[int64]frameotest.Servable{7: {Data: c.data, Extension: c.ext}}
		client := setup(t, frame)

		got, err := client.GetMedia(testCtx(t), frameo.Fetch{ID: 7})
		if err != nil {
			t.Fatalf("%s: GetMedia: %v", c.name, err)
		}
		if got.Width != c.w || got.Height != c.h {
			t.Errorf("%s measured %dx%d, want %dx%d", c.name, got.Width, got.Height, c.w, c.h)
		}
	}
}

// TestGetMediaRefusesANegativeSize keeps a bound that cannot mean anything
// away from the frame, rather than leaving the frame to decide what it means.
func TestGetMediaRefusesANegativeSize(t *testing.T) {
	frame := servingFrame(7, photoBytes(2000))
	c := setup(t, frame)

	if _, err := c.GetMedia(testCtx(t), frameo.Fetch{ID: 7, Size: -1}); err == nil {
		t.Error("a bound of -1 was accepted")
	} else {
		t.Logf("reported: %v", err)
	}
	if got := frame.Served(); len(got) != 0 {
		t.Errorf("the request reached the frame anyway: %v", got)
	}
}

// TestGetMediaReportsFrameError also checks that a refusal is not asked again:
// the frame answered, so a second ask would only spend the timeout over.
func TestGetMediaReportsFrameError(t *testing.T) {
	frame := servingFrame(7, photoBytes(5000))
	frame.ServeError = pb.Error_MISSING_MEDIA_ITEM
	c := setup(t, frame)

	_, err := c.GetMedia(testCtx(t), frameo.Fetch{ID: 7})
	if err == nil {
		t.Fatal("want an error when the frame refuses")
	}
	var refused *frameo.FrameError
	if !errors.As(err, &refused) {
		t.Fatalf("want a FrameError, got %T: %v", err, err)
	}
	if refused.Code != pb.Error_MISSING_MEDIA_ITEM {
		t.Errorf("code = %v", refused.Code)
	}
	if got := frame.Served(); len(got) != 1 {
		t.Errorf("the frame was asked %d times, want 1: a refusal is an answer", len(got))
	}
	t.Logf("reported: %v", err)
}

// TestGetMediaRetriesAfterASilentFrame is the test for the retry rule. The
// frame stops part way through the first request and then, when asked again,
// sends the bytes it still owed in front of the new reply. Those bytes have to
// be dropped for want of a header, and the new header has to start the transfer
// over rather than adding to what was already there.
func TestGetMediaRetriesAfterASilentFrame(t *testing.T) {
	want := photoBytes(20000)
	frame := servingFrame(7, want)
	frame.StallFirst = 1
	frame.StallAfter = 1000
	frame.FlushStalled = true
	c := setup(t, frame)

	got, err := c.GetMedia(testCtx(t), frameo.Fetch{ID: 7, Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("GetMedia: %v", err)
	}
	if !bytes.Equal(got.Data, want) {
		t.Errorf("the photo came back as %d bytes and differs; the abandoned attempt bled into it",
			len(got.Data))
	}
	if n := len(frame.Served()); n != 2 {
		t.Errorf("the frame was asked %d times, want 2", n)
	}
}

// TestGetMediaSurvivesARestartedReply covers the other way two headers can land
// in one attempt: the frame abandons its own reply part way through and begins
// it again. Both name the photo that was asked for, so the id check is no help
// here; only treating a header as the start of a transfer rather than a note in
// the middle of one gets the whole photo back.
func TestGetMediaSurvivesARestartedReply(t *testing.T) {
	want := photoBytes(20000)
	frame := servingFrame(7, want)
	frame.RestartAfter = 4000
	c := setup(t, frame)

	got, err := c.GetMedia(testCtx(t), frameo.Fetch{ID: 7, Attempts: 1})
	if err != nil {
		t.Fatalf("GetMedia: %v", err)
	}
	if !bytes.Equal(got.Data, want) {
		t.Errorf("the photo came back as %d bytes and differs; the abandoned start was counted in",
			len(got.Data))
	}
}

// TestGetMediaIgnoresAnotherPhotosReply is the hazard the batch policy creates:
// having given up on one photo and moved to the next, a complete reply for the
// one abandoned arrives first. Taken for the answer it would fill the transfer,
// the byte count would add up, and the wrong photo would come back under the
// right id.
func TestGetMediaIgnoresAnotherPhotosReply(t *testing.T) {
	seven := photoBytes(20000)
	eight := photoBytes(9000)
	frame := servingFrame(7, seven)
	frame.Servable[8] = frameotest.Servable{Data: eight, Extension: "jpg"}
	frame.StallFirst = 1
	frame.StallAfter = 1000
	frame.FlushStalled = true
	frame.ResendStalledHeader = true
	c := setup(t, frame)
	ctx := testCtx(t)

	// Give up on 7 without retrying, so the next request is for another photo.
	if _, err := c.GetMedia(ctx, frameo.Fetch{ID: 7, Attempts: 1, Timeout: 200 * time.Millisecond}); err == nil {
		t.Fatal("want a timeout while the frame is stalled")
	}
	got, err := c.GetMedia(ctx, frameo.Fetch{ID: 8, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("GetMedia(8): %v", err)
	}
	if !bytes.Equal(got.Data, eight) {
		t.Error("photo 8 came back as something else, most likely photo 7")
	}
	if got.Media.GetId() != 8 {
		t.Errorf("the header names photo %d, want 8", got.Media.GetId())
	}
}

// TestGetMediaGivesUpAfterTheRetry also checks the client is still usable
// afterwards: one photo that never arrives must not poison the rest of a batch.
func TestGetMediaGivesUpAfterTheRetry(t *testing.T) {
	frame := servingFrame(7, photoBytes(20000))
	frame.StallFirst = 2
	frame.StallAfter = 1000
	c := setup(t, frame)
	ctx := testCtx(t)

	_, err := c.GetMedia(ctx, frameo.Fetch{ID: 7, Timeout: 200 * time.Millisecond})
	if !errors.Is(err, frameo.ErrMediaTimeout) {
		t.Fatalf("want ErrMediaTimeout, got %v", err)
	}
	if n := len(frame.Served()); n != 2 {
		t.Errorf("the frame was asked %d times, want 2", n)
	}
	info, err := c.GetInfo(ctx)
	if err != nil {
		t.Fatalf("the client did not recover: %v", err)
	}
	if info.GetName() != "Test Frame" {
		t.Errorf("name = %q", info.GetName())
	}
}

// TestGetMediaRejectsMoreBytesThanAnnounced guards the assumption the whole
// transfer rests on. The header's count is the only thing that says when a
// photo has arrived, so a stream that overruns it has to be refused rather than
// trimmed to fit: trimming is how a wrong photo gets written to disk.
func TestGetMediaRejectsMoreBytesThanAnnounced(t *testing.T) {
	frame := servingFrame(7, photoBytes(5000))
	frame.SizeDelta = -100
	c := setup(t, frame)

	if _, err := c.GetMedia(testCtx(t), frameo.Fetch{ID: 7, Attempts: 1}); err == nil {
		t.Fatal("want an error when more arrives than was announced")
	} else {
		t.Logf("reported: %v", err)
	}
}

func TestGetMediaHonoursTypeOverride(t *testing.T) {
	want := photoBytes(5000)
	frame := servingFrame(7, want)
	frame.GetMediaType = 99
	c := setup(t, frame)

	got, err := c.GetMedia(testCtx(t), frameo.Fetch{ID: 7, Type: 99})
	if err != nil {
		t.Fatalf("GetMedia: %v", err)
	}
	if !bytes.Equal(got.Data, want) {
		t.Error("the photo sent on the override number did not come back")
	}
}

// TestGetMediaRejectsANegativeExtra guards a header that describes a thumbnail
// as fewer than no bytes. The total would then be smaller than the photo, and
// the photo would be taken from past the end of what arrived.
func TestGetMediaRejectsANegativeExtra(t *testing.T) {
	frame := servingFrame(7, photoBytes(5000))
	frame.Servable[7] = frameotest.Servable{
		Data: photoBytes(5000), Extension: "jpg",
		Thumbnail: photoBytes(1000), ThumbnailExtension: "jpg",
	}
	// The photo is 5000 bytes and the thumbnail is described as -2000, so the
	// total the header asks for is 3000: fewer than the photo. Stopping at
	// exactly that many is what makes the shortfall reachable, since more than
	// the total is refused as an overrun before it can do any harm.
	frame.ExtraSizeDelta = -3000
	frame.StallFirst = 1
	frame.StallAfter = 3000
	c := setup(t, frame)

	if _, err := c.GetMedia(testCtx(t), frameo.Fetch{ID: 7, Attempts: 1, Timeout: 200 * time.Millisecond}); err == nil {
		t.Fatal("want an error for an extra stream of negative size")
	} else {
		t.Logf("reported: %v", err)
	}
}

// TestGetMediaTakesThePhotoWhenTheThumbnailNeverComes covers the assumption the
// protocol notes could not settle. If a frame describes a thumbnail it does not
// then send, waiting for the full count would make every photo that has one
// impossible to fetch. The photo itself is complete, so it is taken.
func TestGetMediaTakesThePhotoWhenTheThumbnailNeverComes(t *testing.T) {
	want := photoBytes(8000)
	frame := servingFrame(7, want)
	frame.Servable[7] = frameotest.Servable{
		Data: want, Extension: "jpg",
		Thumbnail: photoBytes(2000), ThumbnailExtension: "jpg",
	}
	frame.WithholdThumbnail = true
	c := setup(t, frame)

	got, err := c.GetMedia(testCtx(t), frameo.Fetch{ID: 7, Attempts: 1, Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("GetMedia: %v", err)
	}
	if !bytes.Equal(got.Data, want) {
		t.Error("the photo did not come back whole")
	}
	if len(got.Thumbnail) != 0 {
		t.Errorf("got %d thumbnail bytes, want none: the frame never sent it", len(got.Thumbnail))
	}
}

// TestGetMediaAcceptsAJPEGWithTrailingBytes checks the end-marker rule is a
// note and not a refusal: real JPEGs carry data after the end marker, and the
// byte count has already proved the photo arrived whole.
func TestGetMediaAcceptsAJPEGWithTrailingBytes(t *testing.T) {
	want := append(photoBytes(5000), 0x00, 0x11, 0x22)
	frame := servingFrame(7, want)
	c := setup(t, frame)

	got, err := c.GetMedia(testCtx(t), frameo.Fetch{ID: 7})
	if err != nil {
		t.Fatalf("GetMedia: %v", err)
	}
	if !bytes.Equal(got.Data, want) {
		t.Error("the photo did not come back as it was sent")
	}
}

// TestGetMediaAcceptsAFormatItCannotCheck makes sure the JPEG check is only
// asked of photos the frame called JPEGs. The frame serves other formats, and
// one of those must not be refused for failing to look like a JPEG.
func TestGetMediaAcceptsAFormatItCannotCheck(t *testing.T) {
	want := []byte("RIFF....WEBPVP8 and then some payload bytes")
	frame := servingFrame(7, want)
	frame.Servable[7] = frameotest.Servable{Data: want, Extension: "webp"}
	c := setup(t, frame)

	got, err := c.GetMedia(testCtx(t), frameo.Fetch{ID: 7})
	if err != nil {
		t.Fatalf("GetMedia: %v", err)
	}
	if got.Extension() != "webp" {
		t.Errorf("extension = %q, want webp", got.Extension())
	}
	if !bytes.Equal(got.Data, want) {
		t.Error("the photo did not come back as it was sent")
	}
}

// TestGetMediaRefusesADestructiveMessageNumber covers the sharp edge of the one
// override that is left. A fetch and a deletion serialise to the same bytes, so
// a number borrowed from the wrong command destroys the photo it was meant to
// copy.
func TestGetMediaRefusesADestructiveMessageNumber(t *testing.T) {
	for _, msgType := range []int32{frameo.TypeDeleteMedia, frameo.TypeChangeMediaVisibility} {
		frame := servingFrame(7, photoBytes(2000))
		frame.DeleteType = frameo.TypeDeleteMedia
		frame.VisibilityType = frameo.TypeChangeMediaVisibility
		frame.Library = []*pb.MediaMetaData{{MediaId: 7, IsVisible: true}}
		c := setup(t, frame)

		if _, err := c.GetMedia(testCtx(t), frameo.Fetch{ID: 7, Type: msgType}); err == nil {
			t.Errorf("Fetch.Type = %d was accepted", msgType)
		}
		if got := frame.Deleted(); len(got) != 0 {
			t.Errorf("Fetch.Type = %d had the frame delete %v", msgType, got)
		}
		if got := frame.Served(); len(got) != 0 {
			t.Errorf("Fetch.Type = %d reached the frame at all: %v", msgType, got)
		}
	}
}

// A real frame opens every connection by asking who is calling, and the name
// it gets is what it shows beside the photos. The answer goes out on its own
// goroutine, so the test waits for the frame to hear it.
func TestClientAnswersWhoIsCallingWithItsName(t *testing.T) {
	frame := frameotest.New()
	frame.AsksWhoIsCalling = true
	c := setupWith(t, frame, &frameo.Options{Name: "bege@box"})

	if _, err := c.GetInfo(testCtx(t)); err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	waitFor(t, "the introduction", func() bool { return len(frame.ClientNames()) > 0 })
	if got := frame.ClientNames(); len(got) != 1 || got[0] != "bege@box" {
		t.Errorf("frame heard %q, want [bege@box]", got)
	}
	if got := frame.UnknownTypes(); len(got) != 0 {
		t.Errorf("frame saw unknown message numbers %v", got)
	}
}

// Without a name there is nothing to say, and the frame's question must not
// be mistaken by a caller for a reply.
func TestClientWithoutANameStaysSilent(t *testing.T) {
	frame := frameotest.New()
	frame.AsksWhoIsCalling = true
	c := setup(t, frame)

	info, err := c.GetInfo(testCtx(t))
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if info.GetName() != "Test Frame" {
		t.Errorf("GetInfo returned %q, want the frame's own description", info.GetName())
	}
	if got := frame.ClientNames(); len(got) != 0 {
		t.Errorf("frame heard %q, want nothing", got)
	}
}

// A client with no name passes the frame's question on, which is what raw's
// probes are read against.
func TestClientWithoutANameShowsTheFramesQuestion(t *testing.T) {
	frame := frameotest.New()
	frame.AsksWhoIsCalling = true
	c := setup(t, frame)

	select {
	case f := <-c.Frames():
		if f.Type != frameo.TypeGetInfo {
			t.Errorf("first message is type %d, want the frame's GetInfo", f.Type)
		}
	case <-testCtx(t).Done():
		t.Fatal("the frame's GetInfo never reached the caller")
	}
}

func TestRequestPermissionReturnsAtOnceWhenAlreadyGranted(t *testing.T) {
	frame := frameotest.New() // grants both by default
	frame.PermissionType = frameo.TypeRequestPermission
	c := setup(t, frame)

	if err := c.RequestPermission(testCtx(t), frameo.PermissionManage); err != nil {
		t.Fatalf("RequestPermission: %v", err)
	}
	if got := frame.PermissionRequests(); len(got) != 0 {
		t.Errorf("the client asked for %v although it already had the permission", got)
	}
}

func TestRequestPermissionWaitsForApproval(t *testing.T) {
	frame := frameotest.New()
	frame.Info.HasPermissionViewPhotos = false
	frame.Info.HasPermissionManagePhotos = false
	frame.PermissionType = frameo.TypeRequestPermission
	frame.GrantOnRequest = true
	c := setup(t, frame)

	if err := c.RequestPermission(testCtx(t), frameo.PermissionManage); err != nil {
		t.Fatalf("RequestPermission: %v", err)
	}
	if got := frame.PermissionRequests(); len(got) != 1 || got[0] != 3 {
		t.Errorf("frame saw permission requests %v, want [3]", got)
	}
	info, err := c.GetInfo(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if !frameo.Granted(info, frameo.PermissionManage) {
		t.Error("the frame does not report the permission as granted")
	}
}

// Nobody at the frame means the request stands unanswered for as long as the
// caller cares to wait, and no longer.
func TestRequestPermissionGivesUp(t *testing.T) {
	frame := frameotest.New()
	frame.Info.HasPermissionViewPhotos = false
	frame.PermissionType = frameo.TypeRequestPermission
	c := setup(t, frame)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.RequestPermission(ctx, frameo.PermissionView)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
	if got := frame.PermissionRequests(); len(got) != 1 || got[0] != 1 {
		t.Errorf("frame saw permission requests %v, want [1]", got)
	}
}

// An owner who taps Deny has answered, and the client must say so rather
// than wait out its deadline.
func TestRequestPermissionReportsARefusal(t *testing.T) {
	frame := frameotest.New()
	frame.Info.HasPermissionViewPhotos = false
	frame.PermissionType = frameo.TypeRequestPermission
	frame.DeclineOnRequest = true
	c := setup(t, frame)

	err := c.RequestPermission(testCtx(t), frameo.PermissionView)
	var fe *frameo.FrameError
	if !errors.As(err, &fe) || fe.Code != pb.Error_DECLINED {
		t.Fatalf("err = %v, want the frame's refusal", err)
	}
}

// waitFor polls until cond holds, for a message that has no reply to wait on.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("%s never arrived at the frame", what)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
