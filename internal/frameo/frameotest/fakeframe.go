// SPDX-License-Identifier: GPL-3.0-or-later

// Package frameotest provides a stand-in Frameo frame, so the client can be
// tested end to end without a device.
//
// It implements the frame's side of the protocol from the protocol
// description rather than by reusing the client's code, so a mistake shared by
// both sides cannot pass unnoticed.
package frameotest

import (
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/borje/unframeo/internal/frameo/pb"
	"google.golang.org/protobuf/proto"
)

// MessageRW is a message pipe to the client. A test tunnel satisfies it.
type MessageRW interface {
	Send([]byte) error
	Recv() ([]byte, error)
}

// Photo is a completed transfer.
type Photo struct {
	Media *pb.Media
	Data  []byte
}

// Frame is a fake photo frame.
type Frame struct {
	// Info is what the frame reports when asked to describe itself.
	Info *pb.FrameInfo
	// RefuseTransfers makes the frame report an error instead of accepting a
	// photo, so the failure path can be tested.
	RefuseTransfers bool
	// DropAck makes the frame accept a photo but never confirm it.
	DropAck bool
	// ListType is the message number this frame answers a listing request on.
	// Configurable rather than fixed to frameo.TypeGetAllMediaMetaData so this
	// package stays independent of that client-side constant.
	ListType int32
	// DeleteType is the message number this frame accepts deletions on.
	DeleteType int32
	// VisibilityType is the message number this frame accepts visibility
	// changes on. Configurable for the same reason as ListType.
	VisibilityType int32
	// ListError makes every listing answer with this code instead of the
	// library, which is how a frame refuses a client it has not been told to
	// trust.
	ListError pb.Error_Code
	// PermissionType is the message number this frame accepts permission
	// requests on. Configurable for the same reason as ListType.
	PermissionType int32
	// GrantOnRequest makes a permission request succeed: the frame's owner
	// taps Allow at once, and Info reports the permission from then on. Unset,
	// the request is recorded and nothing changes, as when nobody is at the
	// frame.
	GrantOnRequest bool
	// DeclineOnRequest makes the owner tap Deny instead. What a real frame
	// sends then is not known; this one answers with an AcknowledgeReceipt
	// carrying the DECLINED code, the refusal the client is built to catch.
	DeclineOnRequest bool
	// AsksWhoIsCalling makes the frame open the conversation with a GetInfo
	// of its own, as a real frame does, so the client's answer can be seen.
	AsksWhoIsCalling bool
	// Library is what a listing reports.
	Library []*pb.MediaMetaData

	// GetMediaType is the message number this frame serves photos on.
	// Configurable for the same reason as ListType.
	GetMediaType int32
	// Servable is what the frame will hand out, by media id. A request for
	// anything else is refused as a missing item.
	Servable map[int64]Servable
	// ServeError makes every request answer with this code instead of a photo.
	ServeError pb.Error_Code
	// ServeSegmentSize is how much photo data goes in one segment. Zero puts
	// the whole photo in a single message, which makes the frame split it into
	// transport-sized parts instead.
	ServeSegmentSize int
	// StallFirst leaves this many requests unfinished: the frame sends the
	// header and StallAfter bytes and then stops talking about it. It is how a
	// test drives the client's timeout without waiting for anything.
	StallFirst int
	// StallAfter is how many bytes a stalled request sends before stopping.
	StallAfter int
	// FlushStalled sends an abandoned request's remaining bytes in front of the
	// next reply, which is the ordering a client that retries has to survive.
	FlushStalled bool
	// ResendStalledHeader makes the flush a complete reply rather than the
	// remaining bytes: the abandoned request's header and its whole stream,
	// ahead of the reply that was actually asked for. It is how a client is
	// shown a finished answer to a question it has stopped waiting for.
	ResendStalledHeader bool
	// RestartAfter makes the frame give up on its own reply part way through
	// and begin it again: the header and this many bytes, then the header once
	// more and the whole photo. Both headers name the photo that was asked
	// for, so nothing but starting afresh on the second one gets a client
	// through it.
	RestartAfter int
	// ExtraSizeDelta is added to the byte count the header declares for the
	// thumbnail, so a client can be shown a count it has no reason to expect.
	ExtraSizeDelta int
	// WithholdThumbnail describes a thumbnail in the header and then never
	// sends it, which is the behaviour the protocol notes could not rule out.
	WithholdThumbnail bool
	// SizeDelta is added to the byte count the header declares, so a client can
	// be shown a count that does not match what arrives.
	SizeDelta int
	// PreviewBelow is where this frame stops handing out the preview and
	// starts handing out the photo. A real frame does not scale to order: it
	// keeps two copies and the bound only chooses between them, so a bound
	// from 1 to PreviewBelow-1 gets Servable.Preview and a bound of zero, or
	// of PreviewBelow or more, gets Servable.Data. Zero leaves the frame
	// indifferent to the bound, which is what every test written before the
	// two copies were discovered expects.
	PreviewBelow int32

	mu       sync.Mutex
	deleted  []int64
	photos   []Photo
	partial  map[int64]*transfer
	multi    map[int64]*multipart
	unknown  []int32
	segments int
	requests []*pb.GetMedia
	// names is what the client called itself, once per introduction.
	names []string
	// permissions is what the client asked to be allowed, in order.
	permissions []int32
	// stalled is what an abandoned transfer still owes; stalledHeader and
	// stalledStream are the header it announced and the whole reply it would
	// have sent. The last two are kept rather than looked up again because the
	// frame may have chosen the preview, and rebuilding from Servable.Data
	// would flush the wrong copy back at a client that is already confused.
	stalled       []byte
	stalledHeader *pb.Media
	stalledStream []byte
	nextMulti     int64
}

// Servable is a photo this frame will hand out when asked for it.
type Servable struct {
	Data               []byte
	Extension          string
	CaptureDate        int64  // epoch millis; zero means the frame reports none
	Thumbnail          []byte // appended after the photo and described in extra
	ThumbnailExtension string
	// Preview is the smaller stored copy, handed out for a bound this frame
	// reads as a request for one. Empty means this photo has a single copy,
	// which is served whatever the bound says.
	Preview []byte
	// PreviewExtension is the preview's format. Empty means the same as the
	// photo's, which is the ordinary case.
	PreviewExtension string
}

type transfer struct {
	media *pb.Media
	data  []byte
}

type multipart struct {
	size  int32
	parts map[int32][]byte
	have  int
}

// New creates a frame with plausible defaults.
func New() *Frame {
	return &Frame{
		Info: &pb.FrameInfo{
			Name:                      "Test Frame",
			Placement:                 "Living room",
			ScreenWidth:               1280,
			ScreenHeight:              800,
			HasPermissionViewPhotos:   true,
			HasPermissionManagePhotos: true,
		},
		partial: map[int64]*transfer{},
		multi:   map[int64]*multipart{},
	}
}

// Photos returns the transfers that completed.
func (f *Frame) Photos() []Photo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Photo(nil), f.photos...)
}

// Deleted returns the ids the frame was asked to remove.
func (f *Frame) Deleted() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.deleted...)
}

// Segments reports how many data segments arrived, which distinguishes a
// segmented transfer from a single-message one.
func (f *Frame) Segments() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.segments
}

// Served reports the ids the frame was asked for, in order, which is how a test
// sees that a request was made twice.
func (f *Frame) Served() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]int64, 0, len(f.requests))
	for _, r := range f.requests {
		ids = append(ids, r.GetMediaId())
	}
	return ids
}

// Requests reports the download requests as they arrived, so a test can check
// what was asked for as well as how often.
func (f *Frame) Requests() []*pb.GetMedia {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*pb.GetMedia(nil), f.requests...)
}

// UnknownTypes lists message numbers the frame did not recognise.
func (f *Frame) UnknownTypes() []int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int32(nil), f.unknown...)
}

// ClientNames lists the names the client introduced itself with.
func (f *Frame) ClientNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.names...)
}

// PermissionRequests lists the permissions the client asked for, as the
// numbers it sent.
func (f *Frame) PermissionRequests() []int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int32(nil), f.permissions...)
}

// Serve runs the frame until the connection ends.
func (f *Frame) Serve(rw MessageRW) error {
	if f.AsksWhoIsCalling {
		if err := send(rw, 1, &pb.GetInfo{}); err != nil {
			return err
		}
	}
	for {
		msg, err := rw.Recv()
		if err != nil {
			return nil
		}
		if err := f.handle(rw, msg); err != nil {
			return err
		}
	}
}

func (f *Frame) handle(rw MessageRW, msg []byte) error {
	msgType, payload, err := decode(msg)
	if err != nil {
		return err
	}

	if msgType == 30 {
		whole, err := f.reassemble(payload)
		if err != nil || whole == nil {
			return err
		}
		if msgType, payload, err = decode(whole); err != nil {
			return err
		}
	}

	if f.ListType != 0 && msgType == f.ListType {
		if f.ListError != pb.Error_NONE {
			return send(rw, 32, &pb.AllMediaMetaData{Error: &pb.Error{Code: f.ListError}})
		}
		f.mu.Lock()
		items := append([]*pb.MediaMetaData(nil), f.Library...)
		f.mu.Unlock()
		return send(rw, 32, &pb.AllMediaMetaData{MediaMetaDataItems: items})
	}
	if f.PermissionType != 0 && msgType == f.PermissionType {
		var req pb.RequestPermission
		if err := proto.Unmarshal(payload, &req); err != nil {
			return fmt.Errorf("frameotest: malformed permission request: %w", err)
		}
		f.mu.Lock()
		f.permissions = append(f.permissions, req.GetPermission())
		if f.GrantOnRequest {
			// The owner taps Allow. A real frame says nothing over the
			// wire; the client sees the change in its next FrameInfo.
			switch req.GetPermission() {
			case 1:
				f.Info.HasPermissionViewPhotos = true
			case 3:
				f.Info.HasPermissionViewPhotos = true
				f.Info.HasPermissionManagePhotos = true
			}
		}
		decline := f.DeclineOnRequest
		f.mu.Unlock()
		if decline {
			return send(rw, 6, &pb.AcknowledgeReceipt{Error: &pb.Error{Code: pb.Error_DECLINED}})
		}
		return nil
	}
	if f.DeleteType != 0 && msgType == f.DeleteType {
		var req pb.DeleteMedia
		if err := proto.Unmarshal(payload, &req); err != nil {
			return fmt.Errorf("frameotest: malformed deletion request: %w", err)
		}
		f.mu.Lock()
		f.deleted = append(f.deleted, req.GetMediaIds()...)
		f.mu.Unlock()
		if id := req.GetRequiresAcknowledgeReceiptId(); id != 0 {
			return send(rw, 6, &pb.AcknowledgeReceipt{AcknowledgeId: id})
		}
		return nil
	}

	if f.GetMediaType != 0 && msgType == f.GetMediaType {
		var req pb.GetMedia
		if err := proto.Unmarshal(payload, &req); err != nil {
			return fmt.Errorf("frameotest: malformed media request: %w", err)
		}
		return f.serveMedia(rw, &req)
	}

	if f.VisibilityType != 0 && msgType == f.VisibilityType {
		var req pb.ChangeMediaVisibility
		if err := proto.Unmarshal(payload, &req); err != nil {
			return fmt.Errorf("frameotest: malformed visibility request: %w", err)
		}
		// A real frame keeps a hidden photo and reports it as hidden in the
		// listing, so the change is applied to the library rather than
		// recorded separately.
		f.mu.Lock()
		for _, id := range req.GetMediaIds() {
			for _, m := range f.Library {
				if m.GetMediaId() == id {
					m.IsVisible = req.GetIsVisible()
				}
			}
		}
		f.mu.Unlock()
		if id := req.GetRequiresAcknowledgeReceiptId(); id != 0 {
			return send(rw, 6, &pb.AcknowledgeReceipt{AcknowledgeId: id})
		}
		return nil
	}

	switch msgType {
	case 1: // GetInfo
		f.mu.Lock()
		info := proto.Clone(f.Info).(*pb.FrameInfo)
		f.mu.Unlock()
		return send(rw, 2, info)

	case 3: // ClientInfo: the client saying who it is
		var who pb.ClientInfo
		if err := proto.Unmarshal(payload, &who); err != nil {
			return fmt.Errorf("frameotest: malformed client introduction: %w", err)
		}
		f.mu.Lock()
		f.names = append(f.names, who.GetName())
		f.mu.Unlock()
		return nil

	case 4: // Media
		var media pb.Media
		if err := proto.Unmarshal(payload, &media); err != nil {
			return fmt.Errorf("frameotest: malformed media header: %w", err)
		}
		f.mu.Lock()
		f.partial[media.GetId()] = &transfer{media: &media}
		f.mu.Unlock()
		return nil

	case 5: // MediaDataSegment
		var seg pb.MediaDataSegment
		if err := proto.Unmarshal(payload, &seg); err != nil {
			return fmt.Errorf("frameotest: malformed media segment: %w", err)
		}
		return f.appendSegment(rw, &seg)

	default:
		f.mu.Lock()
		f.unknown = append(f.unknown, msgType)
		f.mu.Unlock()
		return nil
	}
}

// appendSegment appends file data to whichever transfer is open. The real
// frame has no per-segment addressing either: it appends to the transfer it is
// currently receiving, which is why segments must arrive in order.
func (f *Frame) appendSegment(rw MessageRW, seg *pb.MediaDataSegment) error {
	f.mu.Lock()
	f.segments++
	var t *transfer
	var id int64
	for k, v := range f.partial {
		t, id = v, k
		break
	}
	if t == nil {
		f.mu.Unlock()
		return fmt.Errorf("frameotest: file data arrived with no transfer open")
	}
	t.data = append(t.data, seg.GetData()...)
	done := len(t.data) >= int(t.media.GetSize())
	if done {
		delete(f.partial, id)
		if len(t.data) == int(t.media.GetSize()) && !f.RefuseTransfers {
			f.photos = append(f.photos, Photo{Media: t.media, Data: t.data})
		}
	}
	size := t.media.GetSize()
	got := len(t.data)
	f.mu.Unlock()

	ackID := seg.GetRequiresAcknowledgeReceiptId()
	if ackID == 0 {
		return nil
	}
	if !done {
		return fmt.Errorf("frameotest: transfer was acknowledged after %d of %d bytes", got, size)
	}
	if f.DropAck {
		return nil
	}
	ack := &pb.AcknowledgeReceipt{AcknowledgeId: ackID}
	if f.RefuseTransfers {
		ack.Error = &pb.Error{Code: pb.Error_Code(7)} // failed to receive the item
	}
	return send(rw, 6, ack)
}

// serveMedia answers a request for one photo: a Media header, then the file
// itself in MediaDataSegments, then whatever extra streams were described.
func (f *Frame) serveMedia(rw MessageRW, req *pb.GetMedia) error {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	n := len(f.requests)
	item, ok := f.Servable[req.GetMediaId()]
	tail, oldHeader, oldStream := f.stalled, f.stalledHeader, f.stalledStream
	if f.FlushStalled {
		f.stalled, f.stalledHeader, f.stalledStream = nil, nil, nil
	} else {
		tail = nil
	}
	f.mu.Unlock()

	// A real frame interrupted part way through a transfer still has those
	// bytes to be rid of, and they land in front of the next reply.
	if len(tail) > 0 {
		if f.ResendStalledHeader {
			// A whole reply, not the remainder: a client that took it for the
			// answer would find the byte count adding up exactly. It is the
			// reply that was abandoned, down to which copy it was of.
			if err := f.sendInner(rw, 4, oldHeader); err != nil {
				return err
			}
			tail = oldStream
		}
		if err := f.sendSegments(rw, tail); err != nil {
			return err
		}
	}

	if f.ServeError != 0 || !ok {
		code := f.ServeError
		if code == 0 {
			code = pb.Error_MISSING_MEDIA_ITEM
		}
		return f.sendInner(rw, 4, &pb.Media{Id: req.GetMediaId(), Error: &pb.Error{Code: code}})
	}

	// Which copy the bound asks for is settled once, here, so the header, the
	// bytes and anything kept for a later flush all describe the same one.
	data, ext := f.copyFor(item, req.GetWidth())
	header := f.headerFor(req.GetMediaId(), data, ext)
	if err := f.sendInner(rw, 4, header); err != nil {
		return err
	}

	stream := append([]byte(nil), data...)
	if !f.WithholdThumbnail {
		stream = append(stream, item.Thumbnail...)
	}
	if cut := min(f.RestartAfter, len(stream)); cut > 0 {
		if err := f.sendSegments(rw, stream[:cut]); err != nil {
			return err
		}
		if err := f.sendInner(rw, 4, header); err != nil {
			return err
		}
	}
	if n <= f.StallFirst {
		cut := min(f.StallAfter, len(stream))
		f.mu.Lock()
		f.stalled, f.stalledHeader, f.stalledStream = stream[cut:], header, stream
		f.mu.Unlock()
		return f.sendSegments(rw, stream[:cut])
	}
	return f.sendSegments(rw, stream)
}

// copyFor picks the stored copy a bound is asking for.
//
// A real frame does not scale to order. It keeps the photo and a smaller
// preview beside it, and the bound in the request only chooses between them --
// measured on a real frame, every bound from 1 to 499 fetched the identical
// preview and every one from 500 up the identical original. Zero is not a
// small bound but the absence of one, which is how the original is asked for.
func (f *Frame) copyFor(item Servable, bound int32) (data []byte, ext string) {
	if f.PreviewBelow <= 0 || len(item.Preview) == 0 || bound <= 0 || bound >= f.PreviewBelow {
		return item.Data, item.Extension
	}
	if ext = item.PreviewExtension; ext == "" {
		ext = item.Extension
	}
	return item.Preview, ext
}

// headerFor describes one servable photo the way a real frame announces it.
// The copy is passed in rather than looked up, because a frame that has chosen
// the preview must announce the preview's byte count and not the photo's.
func (f *Frame) headerFor(id int64, data []byte, ext string) *pb.Media {
	item := f.Servable[id]
	header := &pb.Media{
		Id:            id,
		Size:          int32(len(data) + f.SizeDelta),
		FileExtension: ext,
		Type:          pb.Media_PICTURE,
		CaptureDate:   item.CaptureDate,
	}
	if len(item.Thumbnail) > 0 {
		header.Extra = []*pb.Extra{{
			Size:          int32(len(item.Thumbnail) + f.ExtraSizeDelta),
			FileExtension: item.ThumbnailExtension,
		}}
	}
	return header
}

// sendSegments hands over file data. ServeSegmentSize picks how much goes in
// each message; a whole photo in one message leaves the splitting to sendInner,
// which is the other shape a real frame might use.
func (f *Frame) sendSegments(rw MessageRW, data []byte) error {
	step := f.ServeSegmentSize
	if step <= 0 {
		step = len(data)
	}
	for off := 0; off < len(data); off += step {
		chunk := data[off:min(off+step, len(data))]
		if err := f.sendInner(rw, 5, &pb.MediaDataSegment{Data: chunk}); err != nil {
			return err
		}
	}
	return nil
}

// sendInner writes one message, splitting it into parts when it is larger than
// the transport carries. Written out from the protocol description like the
// rest of this package, rather than sharing the client's splitting, so a
// mistake there is not repeated here and hidden.
func (f *Frame) sendInner(rw MessageRW, msgType int32, m proto.Message) error {
	payload, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	body := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(body[0:4], 18)
	binary.BigEndian.PutUint32(body[4:8], uint32(msgType))
	copy(body[8:], payload)

	if len(body) <= 16416 {
		return rw.Send(body)
	}
	f.mu.Lock()
	f.nextMulti++
	id := f.nextMulti
	f.mu.Unlock()
	for i := 0; i*16316 < len(body); i++ {
		end := min((i+1)*16316, len(body))
		part := &pb.MultiPartMessage{
			MessageId:   id,
			MessageSize: int32(len(body)),
			DataIndex:   int32(i),
			MessageData: body[i*16316 : end],
		}
		if err := send(rw, 30, part); err != nil {
			return err
		}
	}
	return nil
}

func (f *Frame) reassemble(payload []byte) ([]byte, error) {
	var part pb.MultiPartMessage
	if err := proto.Unmarshal(payload, &part); err != nil {
		return nil, fmt.Errorf("frameotest: malformed message part: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	m := f.multi[part.GetMessageId()]
	if m == nil {
		m = &multipart{size: part.GetMessageSize(), parts: map[int32][]byte{}}
		f.multi[part.GetMessageId()] = m
	}
	if _, seen := m.parts[part.GetDataIndex()]; !seen {
		m.parts[part.GetDataIndex()] = part.GetMessageData()
		m.have += len(part.GetMessageData())
	}
	if m.have < int(m.size) {
		return nil, nil
	}
	delete(f.multi, part.GetMessageId())

	out := make([]byte, 0, m.size)
	for i := int32(0); ; i++ {
		chunk, ok := m.parts[i]
		if !ok {
			break
		}
		out = append(out, chunk...)
	}
	if len(out) != int(m.size) {
		return nil, fmt.Errorf("frameotest: reassembled %d bytes of a %d byte message", len(out), m.size)
	}
	return out, nil
}

func decode(msg []byte) (int32, []byte, error) {
	if len(msg) < 8 {
		return 0, nil, fmt.Errorf("frameotest: message of %d bytes is too short", len(msg))
	}
	if prefix := binary.BigEndian.Uint32(msg[0:4]); prefix != 18 {
		return 0, nil, fmt.Errorf("frameotest: message prefix is %d, want 18", prefix)
	}
	return int32(binary.BigEndian.Uint32(msg[4:8])), msg[8:], nil
}

func send(rw MessageRW, msgType int32, m proto.Message) error {
	payload, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	buf := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], 18)
	binary.BigEndian.PutUint32(buf[4:8], uint32(msgType))
	copy(buf[8:], payload)
	return rw.Send(buf)
}
