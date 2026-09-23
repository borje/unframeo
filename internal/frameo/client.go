// SPDX-License-Identifier: GPL-3.0-or-later

package frameo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/borje/unframeo/internal/frameo/pb"
	"google.golang.org/protobuf/proto"
)

// segmentSize is how much file data goes in one transfer segment. It leaves
// room for the segment's own encoding inside one transport message, so photo
// data never has to be split a second time.
const segmentSize = 16000

// AckTimeout is how long to wait for the frame to confirm a transfer. It is
// generous because the frame writes the file to storage before answering.
const AckTimeout = 120 * time.Second

// Transport is the message pipe to the frame, satisfied by a peer connection.
type Transport interface {
	Send(ctx context.Context, msg []byte) error
	Recv(ctx context.Context) ([]byte, error)
	Close() error
}

// Client talks to one frame.
type Client struct {
	t    Transport
	log  *slog.Logger
	name string
	r    *reassembler

	frames chan Frame
	nextID atomic.Int64

	mu        sync.Mutex
	err       error
	done      chan struct{}
	closeOnce sync.Once
	// intro counts introductions still being sent, so Close can let one
	// finish rather than cut it off: a short command would otherwise close
	// the connection before the frame had heard the name.
	intro sync.WaitGroup
}

// Options configures a Client. The zero value is usable.
type Options struct {
	// Logger receives the protocol exchange at debug level. Nil discards it.
	Logger *slog.Logger
	// Name is what the frame shows as the sender of this client's photos. It
	// is given in answer to the frame's own GetInfo, which opens every
	// connection; empty leaves that question unanswered, as earlier versions
	// did.
	Name string
}

// NewClient starts talking to a frame over an established connection. It takes
// ownership of the connection: closing the client closes it. A nil o means
// the defaults.
func NewClient(t Transport, o *Options) *Client {
	if o == nil {
		o = &Options{}
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	c := &Client{
		t:      t,
		log:    log,
		name:   o.Name,
		r:      newReassembler(64 << 20),
		frames: make(chan Frame, 16),
		done:   make(chan struct{}),
	}
	c.nextID.Store(time.Now().UnixMilli())
	go c.readLoop()
	return c
}

// Close ends the conversation and the underlying connection.
func (c *Client) Close() error {
	c.intro.Wait()
	c.shutdown(nil)
	return c.t.Close()
}

func (c *Client) shutdown(cause error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.err = cause
		c.mu.Unlock()
		close(c.done)
	})
}

func (c *Client) closedErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return errors.New("frameo: connection closed")
}

// newID mints an identifier for a transfer or an acknowledgement. The frame
// keys transfers by it, so it only has to be unique among those in flight;
// counting up from the current time keeps it unique across runs too.
func (c *Client) newID() int64 { return c.nextID.Add(1) }

func (c *Client) readLoop() {
	defer close(c.frames)
	defer c.r.reset()
	for {
		msg, err := c.t.Recv(context.Background())
		if err != nil {
			c.shutdown(err)
			return
		}
		f, err := decodeFrame(msg)
		if err != nil {
			c.log.Debug("dropping an unreadable message", "err", err, "len", len(msg))
			continue
		}
		if f.Type == TypeMultiPartMessage {
			whole, err := c.r.add(f.Payload)
			if err != nil {
				c.log.Debug("dropping a bad message part", "err", err)
				continue
			}
			if whole == nil {
				continue
			}
			if f, err = decodeFrame(whole); err != nil {
				c.log.Debug("dropping an unreadable reassembled message", "err", err)
				continue
			}
		}
		c.log.Debug("received", "message", f.String())
		if f.Type == TypeGetInfo && c.name != "" {
			// The frame asking who is calling. It is a question, not a reply
			// anyone is waiting for, so it is answered here and goes no
			// further. A client with no name to give passes it on like any
			// other message, as earlier versions did. It is answered from its own goroutine: a send can wait
			// behind a busy upload for as long as the window stays full, and
			// the read loop has to keep draining replies meanwhile or it
			// stalls the very transfer it is waiting behind.
			c.intro.Add(1)
			go func() {
				defer c.intro.Done()
				c.introduce()
			}()
			continue
		}
		select {
		case c.frames <- f:
		case <-c.done:
			return
		}
	}
}

// introduce answers the frame's GetInfo with this client's name. A failure is
// logged and otherwise ignored: the introduction is a courtesy, and a
// connection that cannot carry it will fail the command in progress on its
// own account.
func (c *Client) introduce() {
	if err := c.send(context.Background(), TypeClientInfo, &pb.ClientInfo{Name: c.name}); err != nil {
		c.log.Debug("could not introduce this client", "err", err)
	}
}

// send encodes and transmits one message, splitting it if it is too large.
func (c *Client) send(ctx context.Context, msgType int32, m proto.Message) error {
	body, err := encodeFrame(msgType, m)
	if err != nil {
		return err
	}
	parts, err := split(body, c.newID())
	if err != nil {
		return err
	}
	c.log.Debug("sending", "message", typeName(msgType), "type", msgType, "bytes", len(body), "parts", len(parts))
	for _, part := range parts {
		if err := c.t.Send(ctx, part); err != nil {
			return fmt.Errorf("frameo: send %s: %w", typeName(msgType), err)
		}
	}
	return nil
}

// await waits for a message the predicate accepts. Anything else is logged and
// dropped: a frame volunteers status messages at any time, and none of them
// should derail an operation in progress.
//
// It waits only on the message queue, never on the connection's end directly,
// so a reply that arrived just before the connection dropped is still seen
// rather than lost to a race between the two.
func (c *Client) await(ctx context.Context, what string, accept func(Frame) bool) (Frame, error) {
	for {
		select {
		case f, ok := <-c.frames:
			if !ok {
				return Frame{}, fmt.Errorf("frameo: waiting for %s: %w", what, c.closedErr())
			}
			if accept(f) {
				return f, nil
			}
			c.log.Debug("ignoring an unrelated message while waiting", "for", what, "got", f.String())
		case <-ctx.Done():
			return Frame{}, fmt.Errorf("frameo: waiting for %s: %w", what, ctx.Err())
		}
	}
}

func expectType(t int32) func(Frame) bool {
	return func(f Frame) bool { return f.Type == t }
}

// GetInfo asks the frame to describe itself.
func (c *Client) GetInfo(ctx context.Context) (*pb.FrameInfo, error) {
	if err := c.send(ctx, TypeGetInfo, &pb.GetInfo{}); err != nil {
		return nil, err
	}
	f, err := c.await(ctx, "frame information", expectType(TypeFrameInfo))
	if err != nil {
		return nil, err
	}
	var info pb.FrameInfo
	if err := proto.Unmarshal(f.Payload, &info); err != nil {
		return nil, fmt.Errorf("frameo: malformed frame information: %w", err)
	}
	return &info, nil
}

// permissionPoll is how often RequestPermission asks the frame whether the
// owner has answered yet. The frame sends nothing when they do; the change is
// visible only in FrameInfo.
const permissionPoll = 2 * time.Second

// RequestPermission asks the frame's owner to grant p, and waits until they
// do, they refuse, or ctx ends. The frame shows a prompt on its screen, so
// someone has to be standing at it; the wait is bounded only by ctx. A refusal
// is a *FrameError. A permission already held is reported as granted without
// asking.
func (c *Client) RequestPermission(ctx context.Context, p Permission) error {
	granted, err := c.pollPermission(ctx, p)
	if err != nil || granted {
		return err
	}
	if err := c.send(ctx, TypeRequestPermission, &pb.RequestPermission{Permission: int32(p)}); err != nil {
		return err
	}
	for {
		select {
		case <-time.After(permissionPoll):
		case <-ctx.Done():
			return fmt.Errorf("frameo: waiting for permission to %s: %w", p, ctx.Err())
		}
		granted, err := c.pollPermission(ctx, p)
		if err != nil || granted {
			return err
		}
	}
}

// pollPermission asks the frame for its FrameInfo and reports whether it
// shows p as held. A refusal arriving meanwhile ends the wait instead.
//
// What a refusal looks like on the wire is not known: a frame whose owner taps
// Allow says nothing and changes FrameInfo, and nobody has yet watched one
// whose owner taps Deny. Rather than wait out ctx on a request that has
// already been answered, any reply carrying an Error in the place
// AcknowledgeReceipt keeps it -- the shape most refusals in this protocol
// take -- is taken as the answer, whether it comes typed as an
// AcknowledgeReceipt or as the number after the request's own.
func (c *Client) pollPermission(ctx context.Context, p Permission) (bool, error) {
	if err := c.send(ctx, TypeGetInfo, &pb.GetInfo{}); err != nil {
		return false, err
	}
	var refused error
	f, err := c.await(ctx, "frame information", func(f Frame) bool {
		if f.Type == TypeFrameInfo {
			return true
		}
		refused = permissionRefusal(f, p)
		return refused != nil
	})
	if err != nil {
		return false, err
	}
	if refused != nil {
		return false, refused
	}
	var info pb.FrameInfo
	if err := proto.Unmarshal(f.Payload, &info); err != nil {
		return false, fmt.Errorf("frameo: malformed frame information: %w", err)
	}
	return Granted(&info, p), nil
}

// permissionRefusal reads f as a refusal of a request for p, or returns nil
// when it is not one.
func permissionRefusal(f Frame, p Permission) error {
	if f.Type != TypeAcknowledgeReceipt && f.Type != TypeRequestPermission+1 {
		return nil
	}
	var ack pb.AcknowledgeReceipt
	if err := proto.Unmarshal(f.Payload, &ack); err != nil {
		return nil
	}
	return frameError("the request to "+p.String(), ack.GetError())
}
