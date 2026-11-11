// SPDX-License-Identifier: GPL-3.0-or-later

package frameo

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/borje/unframeo/internal/frameo/pb"
	"google.golang.org/protobuf/proto"
)

// Photo describes one photo to send.
type Photo struct {
	// Path is the file to read.
	Path string
	// Extension is the format the frame files the photo under, without a dot.
	// Empty means the extension of Path, which is how a file on disk says what
	// it is; a photo that reached this program by some other route has to say
	// so here rather than through the name of whatever it was written to.
	Extension string
	// Caption is shown with the photo on the frame. Optional.
	Caption string
	// Taken is the capture date the frame files the photo under. Zero means
	// the file's modification time.
	Taken time.Time
	// Fit selects scaling: false crops to fill the screen around the centre
	// point, true fits the whole photo inside it.
	Fit bool
	// Scale chooses how the frame fits the photo to its screen, and when it
	// is set it overrides Fit. ScaleDefault leaves the choice to Fit.
	Scale Scale
	// Center places the crop focus as x and y from 0 to 1. Nil means the
	// middle of the photo, which is different from {0, 0}: that is its
	// top-left corner.
	Center *[2]float32
	// SingleSegment sends the file as one message and lets the transport split
	// it, instead of sending a series of segments. The app sends segments; this
	// exists to try the other shape against a frame that rejects them.
	SingleSegment bool
}

// Scale is how the frame fits a photo to its screen, one of the three scale
// types the Media header carries.
type Scale int

const (
	// ScaleDefault defers to Photo.Fit: fill unless Fit is set.
	ScaleDefault Scale = iota
	// ScaleFill crops the photo to fill the screen around the focus point.
	ScaleFill
	// ScaleFit shows the whole photo inside the screen.
	ScaleFit
	// ScaleNone sends the protocol's NONE and leaves scaling to the frame.
	// What a frame does with it has not been observed.
	ScaleNone
)

// SendPhoto transfers one photo and waits for the frame to confirm it.
//
// The frame is told the total size up front and then appends whatever arrives
// until it has that many bytes, so the data must be sent in order and nothing
// else may be interleaved into the same transfer.
func (c *Client) SendPhoto(ctx context.Context, p Photo) (int64, error) {
	data, err := os.ReadFile(p.Path)
	if err != nil {
		return 0, fmt.Errorf("frameo: %w", err)
	}
	if len(data) == 0 {
		return 0, fmt.Errorf("frameo: %s is empty", p.Path)
	}
	// The frame is told the size in a 32-bit field and waits for exactly that
	// many bytes, so a file it cannot describe must be refused here rather
	// than announced as a smaller one.
	if len(data) > math.MaxInt32 {
		return 0, fmt.Errorf("frameo: %s is %d bytes, larger than the protocol can describe", p.Path, len(data))
	}

	taken := p.Taken
	if taken.IsZero() {
		if st, err := os.Stat(p.Path); err == nil {
			taken = st.ModTime()
		} else {
			taken = time.Now()
		}
	}
	cx, cy := float32(0.5), float32(0.5)
	if p.Center != nil {
		cx, cy = p.Center[0], p.Center[1]
		// A NaN fails both comparisons, so it is refused along with the rest.
		if !(cx >= 0 && cx <= 1 && cy >= 0 && cy <= 1) {
			return 0, fmt.Errorf("frameo: focus point %v,%v is outside 0 to 1", cx, cy)
		}
	}
	scale := pb.Media_CENTER_POINT_CROP
	if p.Fit {
		scale = pb.Media_FIT_INSIDE
	}
	switch p.Scale {
	case ScaleDefault:
	case ScaleFill:
		scale = pb.Media_CENTER_POINT_CROP
	case ScaleFit:
		scale = pb.Media_FIT_INSIDE
	case ScaleNone:
		scale = pb.Media_NONE
	default:
		return 0, fmt.Errorf("frameo: unknown scale %d", p.Scale)
	}

	id := c.newID()
	media := &pb.Media{
		Id:            id,
		ContentId:     id,
		Size:          int32(len(data)),
		FileExtension: fileExtension(p.Path, p.Extension),
		Type:          pb.Media_PICTURE,
		ScaleType:     scale,
		CenterPointX:  cx,
		CenterPointY:  cy,
		CaptureDate:   taken.UnixMilli(),
		Caption:       p.Caption,
	}
	if err := c.send(ctx, TypeMedia, media); err != nil {
		return 0, err
	}

	ackID := c.newID()
	segments := segmentCount(len(data), p.SingleSegment)
	c.log.Debug("sending photo", "path", p.Path, "bytes", len(data), "id", id, "segments", segments)

	for i, chunk := range chunks(data, p.SingleSegment) {
		seg := &pb.MediaDataSegment{Data: chunk}
		if i == segments-1 {
			seg.RequiresAcknowledgeReceiptId = ackID
		}
		if err := c.send(ctx, TypeMediaDataSegment, seg); err != nil {
			return 0, err
		}
	}

	if err := c.awaitAck(ctx, ackID, "the photo to be accepted", "the photo"); err != nil {
		return 0, err
	}
	return id, nil
}

// awaitAck waits for the frame to confirm the operation with the given id. A
// confirmation can carry a failure, so the answer is examined rather than
// merely counted.
//
// what describes what is being waited for and subject names the thing itself,
// because the two read differently: one completes "waiting for ...", the other
// "the frame refused ...".
func (c *Client) awaitAck(ctx context.Context, ackID int64, what, subject string) error {
	ctx, cancel := context.WithTimeout(ctx, AckTimeout)
	defer cancel()

	f, err := c.await(ctx, what, func(f Frame) bool {
		if f.Type != TypeAcknowledgeReceipt {
			return false
		}
		var ack pb.AcknowledgeReceipt
		if err := proto.Unmarshal(f.Payload, &ack); err != nil {
			return false
		}
		return ack.GetAcknowledgeId() == ackID
	})
	if err != nil {
		return err
	}
	var ack pb.AcknowledgeReceipt
	if err := proto.Unmarshal(f.Payload, &ack); err != nil {
		return fmt.Errorf("frameo: malformed confirmation: %w", err)
	}
	return frameError(subject, ack.GetError())
}

// chunks yields the pieces the file is sent in.
func chunks(data []byte, single bool) [][]byte {
	if single {
		return [][]byte{data}
	}
	var out [][]byte
	for off := 0; off < len(data); off += segmentSize {
		out = append(out, data[off:min(off+segmentSize, len(data))])
	}
	return out
}

func segmentCount(n int, single bool) int {
	if single {
		return 1
	}
	return (n + segmentSize - 1) / segmentSize
}

// fileExtension returns the extension the frame should store the file under:
// what the caller stated, failing that what the path says, failing that jpg.
func fileExtension(path, stated string) string {
	if ext := normalExtension(stated); ext != "" {
		return ext
	}
	if ext := normalExtension(filepath.Ext(path)); ext != "" {
		return ext
	}
	return "jpg"
}

// normalExtension puts an extension in the form the frame files photos under:
// lowercase, without a dot, and with jpeg spelled jpg. An empty extension stays
// empty, because the two callers disagree about what to do with one.
func normalExtension(ext string) string {
	ext = strings.TrimPrefix(strings.ToLower(ext), ".")
	if ext == "jpeg" {
		return "jpg"
	}
	return ext
}

// MediaTimeout bounds one attempt at reading a photo back, from the request to
// the last byte. It matches the app, which gives up six seconds after asking.
//
// It is far shorter than AckTimeout, and the two measure different things: an
// upload is confirmed only once the frame has written the file to storage,
// while this covers the whole of a transfer. A photo too large to arrive within
// six seconds over a relay will therefore fail rather than finish slowly, and
// will fail on the retry too. Fetch.Timeout raises the bound where that turns
// out to matter.
const MediaTimeout = 6 * time.Second

// MediaAttempts is how many times GetMedia asks before giving up: the request,
// and one retry after a frame that went quiet.
const MediaAttempts = 2

// maxMediaBytes caps what one download may accumulate. The frame announces the
// size and this client believes it, so without a ceiling a mistaken header
// could ask this process to allocate gigabytes.
const maxMediaBytes = 256 << 20

// ErrMediaTimeout reports that the frame stopped sending. It is the only
// failure worth asking about again, so it is distinguishable from the rest.
var ErrMediaTimeout = errors.New("frameo: the frame stopped sending the photo")

// Size is the bound a fetch puts in GetMedia's width and height, and its value
// is what goes on the wire: SizeFull is the zero the protocol reads as no bound
// at all, and anything else is that many pixels. It is not the byte count
// Media.size means -- that one describes what arrived, this one describes what
// to ask for.
//
// It is not a scaling knob, however much width and height suggest one. The
// frame this was measured against does not scale to order. It holds exactly two
// copies of a photo -- the original, and a stored preview whose long side is
// 570 pixels -- and the bound only chooses between them. Bounds of 64, 256,
// 380, 480, 490, 496 and 499 all returned the byte-identical preview; 500, 511,
// 512, 570, 571 and 1024 all returned the byte-identical original. The cutoff
// fell in the same place for a photo of the other orientation, so it belongs to
// the frame rather than to the photo. Asking for 380 does not get a 380 pixel
// photo.
//
// Nothing in the reply says which copy arrived or how large it is: the header
// carries a byte count, an extension and a capture date, and the listing
// carries less than that. A caller that needs the pixels reads Download.Width
// and Download.Height, which this client measures from the bytes themselves
// because there is nowhere else to read them.
type Size int32

const (
	// SizeFull asks for the photo as the frame stores it. It is the zero
	// value, so a Fetch that says nothing about size gets the original.
	SizeFull Size = 0

	// SizePreview asks for the small stored copy, which is what to want when
	// the point is a contact sheet rather than the photo: 27,394 bytes at
	// 570x380 against 510,734 at 2880x1920, off the same photo.
	//
	// 256 is a bound and not a promise of 256 pixels; what comes back is
	// whatever the frame stored. The number sits at about half the 500 pixel
	// cutoff measured here, so it still selects the preview on a frame that
	// divides the two somewhat lower, and it is an ordinary thumbnail bound
	// rather than a degenerate one like 1, which a frame is likelier to have a
	// special case for. A frame that divides them somewhere else entirely is
	// reached with a plain Size(n).
	SizePreview Size = 256
)

// Fetch describes one photo to read back from the frame.
type Fetch struct {
	// ID is the photo's id on the frame, as ListMedia reports it.
	ID int64
	// Size chooses which stored copy to ask for. Zero, the useful default, is
	// SizeFull.
	Size Size
	// Attempts is how many times to ask before giving up. Zero means
	// MediaAttempts.
	Attempts int
	// Timeout bounds each attempt separately. Zero means MediaTimeout.
	Timeout time.Duration
	// Type overrides the message number. Zero means TypeGetMedia.
	//
	// It is the last override of its kind: the other message numbers were
	// settled against a real frame, and TypeGetMedia was read off a decompile
	// and has not been. If it turns out wrong, this is how another candidate
	// gets tried with a real media id in the payload, which SendRaw cannot do.
	Type int32
}

// Download is one photo read back from the frame.
type Download struct {
	// Media is the header the frame sent, carrying the caption, the capture
	// date and the description of any extra streams.
	Media *pb.Media
	// Data is the photo itself: exactly Media.size bytes.
	Data []byte
	// Thumbnail is whatever followed the photo, described by Media.extra. It is
	// empty when the frame appended none.
	Thumbnail []byte
	// Width and Height are the photo's pixel dimensions, read out of the bytes
	// that arrived. The frame states them nowhere -- not in the header, not in
	// the listing -- and the bound the request carried chose between stored
	// copies rather than describing either of them, so measuring here is the
	// only way a caller can report what it actually received.
	//
	// Both are zero when the format is not one this client can measure. That
	// is not a failure: the photo arrived whole, and anything printing these
	// has to be ready to say nothing.
	Width, Height int
}

// Extension is the extension to file the photo under: what the frame reported,
// or jpg when it reported nothing.
func (d *Download) Extension() string {
	if ext := d.reportedExtension(); ext != "" {
		return ext
	}
	return "jpg"
}

// reportedExtension is what the frame actually said, which is not the same
// question as what to call the file. A frame that said nothing has told us
// nothing about the format, and must not be treated as having said jpg.
func (d *Download) reportedExtension() string {
	return normalExtension(d.Media.GetFileExtension())
}

// GetMedia reads one photo back from the frame.
//
// A frame that falls silent is asked again, up to Attempts times. A frame that
// answers is not, whatever it answered: a refusal is a settled reply and asking
// again only spends the timeout over, and a connection that has ended will not
// recover by being spoken to.
func (c *Client) GetMedia(ctx context.Context, f Fetch) (*Download, error) {
	// A fetch is one varint in field 1, and a repeated scalar accepts that same
	// encoding, so a fetch sent on the deletion number arrives as a deletion of
	// the very photo it asked for. Type is for trying numbers nobody has
	// identified; aimed at one of these it destroys what it was copying.
	switch f.Type {
	case TypeDeleteMedia:
		return nil, fmt.Errorf("frameo: %d is the deletion number: a fetch sent on it would delete photo %d, not copy it",
			f.Type, f.ID)
	case TypeChangeMediaVisibility:
		return nil, fmt.Errorf("frameo: %d is the visibility number: a fetch sent on it would hide photo %d, not copy it",
			f.Type, f.ID)
	}
	// A bound is a count of pixels, so there is nothing a negative one could
	// ask for. Refusing here rather than sending it keeps a frame from having
	// to decide what it means.
	if f.Size < 0 {
		return nil, fmt.Errorf("frameo: a bound of %d is not a number of pixels", f.Size)
	}
	attempts := f.Attempts
	if attempts <= 0 {
		attempts = MediaAttempts
	}
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		var got *Download
		if got, err = c.getMediaOnce(ctx, f, attempt > 1); err == nil {
			return got, nil
		}
		if !errors.Is(err, ErrMediaTimeout) || ctx.Err() != nil {
			return nil, err
		}
		if attempt < attempts {
			c.log.Debug("the frame stopped sending; asking again", "id", f.ID, "attempt", attempt)
		}
	}
	return nil, err
}

// getMediaOnce makes one attempt. retry says whether an earlier attempt was
// abandoned, which is the only circumstance in which stray bytes can still be
// on their way.
func (c *Client) getMediaOnce(parent context.Context, f Fetch, retry bool) (*Download, error) {
	msgType := f.Type
	if msgType == 0 {
		msgType = TypeGetMedia
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = MediaTimeout
	}

	// Only on a retry, and only as housekeeping: what is queued is the tail of
	// the attempt just abandoned, and dropping it bounds memory and frees the
	// reassembler's slots. It is not a defence -- bytes still on the wire
	// arrive after it and it cannot touch them -- and it costs any other
	// operation's queued reply, so the first attempt leaves the queue alone.
	if retry {
		c.discardPending()
	}

	req := &pb.GetMedia{MediaId: f.ID, Width: int32(f.Size), Height: int32(f.Size)}
	if err := c.send(parent, msgType, req); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	var header *pb.Media
	var size, want int
	var acc []byte
	for {
		fr, err := c.await(ctx, "the photo", func(fr Frame) bool {
			return fr.Type == TypeMedia || fr.Type == TypeMediaDataSegment
		})
		if err != nil {
			// Our own deadline is worth another attempt. The caller's decision
			// to give up, and a connection that has ended, are not.
			if parent.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			// The photo may be complete even though the transfer is not: the
			// count includes extra streams the frame described and may not
			// send, and whether it sends them is the one thing about this
			// exchange nobody has watched a real frame do. Waiting for bytes
			// that are not coming would make every photo with a thumbnail
			// unfetchable, so the photo is taken and the shortfall reported.
			if header != nil && len(acc) >= size {
				c.log.Warn("the frame stopped after the photo and did not send everything it described",
					"id", f.ID, "photo", size, "announced", want, "received", len(acc))
				return c.finish(header, acc, size, want, retry)
			}
			return nil, fmt.Errorf("%w: photo %d, nothing further for %s", ErrMediaTimeout, f.ID, timeout)
		}

		switch fr.Type {
		case TypeMedia:
			var m pb.Media
			if err := proto.Unmarshal(fr.Payload, &m); err != nil {
				return nil, fmt.Errorf("frameo: malformed photo header: %w", err)
			}
			// The frame echoes back the id it was asked for, so a header naming
			// a different photo belongs to a request this one has moved on
			// from. Letting it through would be the serious failure: its
			// segments would fill this transfer, the byte count would add up,
			// and the wrong photo would be returned under the right id. A frame
			// that sends no id at all still gets the benefit of the doubt.
			if id := m.GetId(); id != 0 && id != f.ID {
				c.log.Debug("ignoring a header for another photo", "want", f.ID, "got", id)
				continue
			}
			if err := frameError(fmt.Sprintf("photo %d", f.ID), m.GetError()); err != nil {
				return nil, err
			}
			if m.GetSize() <= 0 {
				return nil, fmt.Errorf("frameo: the frame offered photo %d as %d bytes", f.ID, m.GetSize())
			}
			size = int(m.GetSize())
			// What the header says is worth seeing: whether the frame echoes
			// the id, whether it dates the photo, and whether it describes a
			// thumbnail after it are all answered here and nowhere else.
			c.log.Debug("photo header", "asked", f.ID, "id", m.GetId(), "size", m.GetSize(),
				"extras", len(m.GetExtra()), "captureDate", m.GetCaptureDate())
			extra, err := extraBytes(&m)
			if err != nil {
				return nil, fmt.Errorf("frameo: photo %d: %w", f.ID, err)
			}
			want = size + extra
			if want > maxMediaBytes {
				return nil, fmt.Errorf("frameo: the frame offered %d bytes for photo %d, over the %d byte limit",
					want, f.ID, maxMediaBytes)
			}
			// A header begins a transfer, so a second one replaces the first
			// rather than adding to it. That is the frame's own model -- the
			// app builds a fresh receiver on every header -- and it is what
			// makes asking twice safe. Messages keep the order the frame sent
			// them in, all the way from the peer's reader to this queue, so
			// everything the abandoned attempt produced is already behind this
			// header and is thrown away with the accumulator.
			header, acc = &m, acc[:0]

		case TypeMediaDataSegment:
			if header == nil {
				// Bytes with no header in front of them belong to a transfer
				// this call never asked for.
				continue
			}
			var seg pb.MediaDataSegment
			if err := proto.Unmarshal(fr.Payload, &seg); err != nil {
				return nil, fmt.Errorf("frameo: malformed photo data: %w", err)
			}
			acc = append(acc, seg.GetData()...)
		}

		// There is no terminator and no length prefix. The count the header
		// announced is the only thing that says when the photo has arrived.
		if header == nil || len(acc) < want {
			continue
		}
		if len(acc) > want {
			// More bytes than were announced cannot be a harmless surplus: the
			// frame said how many it would send, so the extra ones came from
			// somewhere else. Refusing is how a wrong assumption here gets
			// noticed instead of being written to disk as a photo.
			return nil, fmt.Errorf("frameo: photo %d was announced as %d bytes and %d arrived",
				f.ID, want, len(acc))
		}
		c.log.Debug("received photo", "id", f.ID, "bytes", size, "announced", want)
		return c.finish(header, acc, size, want, retry)
	}
}

// finish assembles the result once the photo itself has arrived. The photo is
// the first size bytes and anything after it belongs to the extra streams the
// header described.
func (c *Client) finish(header *pb.Media, acc []byte, size, want int, retry bool) (*Download, error) {
	got := &Download{Media: header, Data: acc[:size]}
	if len(acc) > size {
		got.Thumbnail = acc[size:min(len(acc), want)]
	}
	if err := c.checkWhole(got, retry); err != nil {
		return nil, err
	}
	// Measured rather than believed: the frame chose which copy to send and
	// said nothing about it, so this is the only description of what arrived.
	got.Width, got.Height = imageSize(got.Data)
	return got, nil
}

// discardPending throws away messages that arrived for no operation, along with
// the parts of messages still being reassembled. Only a download needs it: every
// other reply is recognised by its type or its acknowledgement id, so await can
// drop a stray one harmlessly, while a data segment carries nothing at all to
// say which request it belongs to.
func (c *Client) discardPending() {
	// The reassembler first, so a message that completes from what is already
	// queued is still taken off the queue below. An abandoned part-message
	// would otherwise sit in it for good, counting against the limit on how
	// many may be part-received at once.
	c.r.reset()
	for {
		select {
		case f, ok := <-c.frames:
			if !ok {
				return
			}
			c.log.Debug("discarding a message left over from an abandoned request", "message", f.String())
		default:
			return
		}
	}
}

// extraBytes totals the streams the frame appends after the photo itself. The
// thumbnail is extra[0]; the protocol allows more, and only the total matters
// here, since the stream carries no boundaries of its own.
//
// A negative count is refused rather than added. It would make the total less
// than the photo, and the photo is then taken from beyond the end of what
// arrived -- a panic, or somebody else's memory, from one bad number.
func extraBytes(m *pb.Media) (int, error) {
	n := 0
	for i, e := range m.GetExtra() {
		if e.GetSize() < 0 {
			return 0, fmt.Errorf("extra stream %d is described as %d bytes", i, e.GetSize())
		}
		n += int(e.GetSize())
	}
	return n, nil
}

// checkWhole catches a photo that did not arrive as itself.
//
// The byte count already proves the right number of bytes arrived, so what is
// left to catch is substitution: the right number of the wrong bytes. Data that
// does not begin like a JPEG is not the photo, whatever else happened.
//
// Only the start is checked. A JPEG's end marker is not the end of the file --
// real ones carry trailing data after it -- so a missing one is noted and not
// refused; refusing would throw away photos that arrived perfectly well. And
// this is only asked of a photo the frame itself called a JPEG: a frame that
// named no format has said nothing to check against, and one that named another
// format is not owed a JPEG's shape.
//
// None of this would notice segments from two transfers interleaved without a
// header between them. Nothing in the protocol would; a data segment carries no
// identifier, and the app has the same blind spot.
func (c *Client) checkWhole(d *Download, retry bool) error {
	if d.reportedExtension() != "jpg" {
		return nil
	}
	b := d.Data
	if len(b) < 4 || b[0] != 0xff || b[1] != 0xd8 || b[2] != 0xff {
		return fmt.Errorf("frameo: photo %d does not begin like a JPEG, so it is not the photo that was asked for",
			d.Media.GetId())
	}
	if b[len(b)-2] != 0xff || b[len(b)-1] != 0xd9 {
		c.log.Debug("the photo does not end with a JPEG's end marker",
			"id", d.Media.GetId(), "retry", retry)
	}
	return nil
}

// ListMedia asks the frame what it is holding.
func (c *Client) ListMedia(ctx context.Context) ([]*pb.MediaMetaData, error) {
	if err := c.send(ctx, TypeGetAllMediaMetaData, &pb.GetAllMediaMetaData{}); err != nil {
		return nil, err
	}
	f, err := c.await(ctx, "the media list", expectType(TypeAllMediaMetaData))
	if err != nil {
		return nil, err
	}
	var list pb.AllMediaMetaData
	if err := proto.Unmarshal(f.Payload, &list); err != nil {
		return nil, fmt.Errorf("frameo: malformed media list: %w", err)
	}
	if err := frameError("the media listing", list.GetError()); err != nil {
		return nil, err
	}
	return list.GetMediaMetaDataItems(), nil
}

// DeleteMedia removes photos from the frame and waits for confirmation.
func (c *Client) DeleteMedia(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	ackID := c.newID()
	req := &pb.DeleteMedia{MediaIds: ids, RequiresAcknowledgeReceiptId: ackID}
	if err := c.send(ctx, TypeDeleteMedia, req); err != nil {
		return err
	}
	return c.awaitAck(ctx, ackID, "the deletion to be confirmed", "the deletion")
}

// SetMediaVisible shows or hides photos on the frame without deleting them.
func (c *Client) SetMediaVisible(ctx context.Context, ids []int64, visible bool) error {
	if len(ids) == 0 {
		return nil
	}
	ackID := c.newID()
	req := &pb.ChangeMediaVisibility{MediaIds: ids, IsVisible: visible, RequiresAcknowledgeReceiptId: ackID}
	if err := c.send(ctx, TypeChangeMediaVisibility, req); err != nil {
		return err
	}
	return c.awaitAck(ctx, ackID, "the visibility change to be confirmed", "the visibility change")
}

// SendRaw sends an arbitrary message and is the tool for identifying the
// message numbers that are still unknown. Everything the frame says in reply
// arrives through Frames.
func (c *Client) SendRaw(ctx context.Context, msgType int32, payload []byte) error {
	body := encodeFrameBytes(msgType, payload)
	parts, err := split(body, c.newID())
	if err != nil {
		return err
	}
	for _, part := range parts {
		if err := c.t.Send(ctx, part); err != nil {
			return err
		}
	}
	return nil
}

// Frames exposes messages the frame sends. It is used by the raw command and
// must not be read from while another operation is in progress, since both
// take from the same queue.
func (c *Client) Frames() <-chan Frame { return c.frames }
