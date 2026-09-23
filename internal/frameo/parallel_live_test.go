// SPDX-License-Identifier: GPL-3.0-or-later

//go:build live

// These tests answer two questions about a real frame that no decompile does:
// whether it serves several photos at once, and what a photo's header carries
// when the photo itself is not wanted.
//
//	go test -tags live ./internal/frameo -run TestLiveParallel -v
//	go test -tags live ./internal/frameo -run TestLiveTwoTransfersAtOnce -v
//	go test -tags live ./internal/frameo -run TestLiveHeaderProbe -v
//
// They run over the local network when the frame answers mDNS and fall back to
// the relay when it does not, since parallelism matters most over the relay.
// Set UNFRAMEO_NET=relay to force the relay.
//
// What they found, against a real frame over the local network:
//
// Connections are not the limit. Four opened at once all came up, in about
// 160ms each, and all four answered GetInfo.
//
// Transfers are. Two photos fetched at the same moment over two connections
// collided: one stalled outright, and the other was announced as 118,106 bytes
// and 130,528 arrived. The surplus is the other photo's data, delivered into
// this connection's stream. Eight photos over four connections went the same
// way -- seven of the eight failed, one of them with 114,212 bytes against
// 109,932 announced -- and took 4m0s against 1.417s for the same eight fetched
// one after another. Parallelism here is not slower, it is wrong: without the
// byte count in the header the wrong bytes would have been written to disk as a
// photo.
//
// This is what GETMEDIA.md section 4 describes from the decompile: the receiver
// is keyed by peer rather than by media id. Neither side can sort the streams
// out afterwards, because a data segment carries no identifier at all. So a
// frame is asked for one photo at a time, and the client offers no way to do
// otherwise.
//
// The frame also stops serving photos afterwards, and comes back on its own:
// 38 seconds after one collision, on the second attempt, a fresh connection got
// the photo whole again. Whether anything else the frame does keeps working
// while it will not send a photo is untested. TestLiveTwoTransfersAtOnce waits
// for the recovery and reports how long it took rather than failing, since the
// wedge is the finding and a probe that leaves the frame unusable would be a
// worse one.
//
// Which way a collision falls varies. One run had the surplus bytes above;
// another had one photo arrive whole while the other stalled, with no
// mismatch at all. What does not vary is that two at once do not both work.
package frameo_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/borje/unframeo/internal/config"
	"github.com/borje/unframeo/internal/frameo"
	"github.com/borje/unframeo/internal/mdns"
	"github.com/borje/unframeo/internal/sdg"
)

// opener makes one fresh connection to the frame. Each call is a separate
// peer connection, which is what parallel work would need.
type opener func(ctx context.Context) (*frameo.Client, error)

// connector returns a way to open connections, and says which route it took.
func connector(t *testing.T) (opener, string, *slog.Logger) {
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
	level := slog.LevelInfo
	if testing.Verbose() {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if os.Getenv("UNFRAMEO_NET") != "relay" {
		look, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		found, err := mdns.Lookup(look, sdg.LocalService, func(instance string) bool {
			return sdg.IsLocalInstance(instance, peer)
		}, &mdns.Options{Logger: log})
		if addr, ok := found.Addr(); err == nil && ok {
			ep := sdg.Endpoint{Host: addr.String(), Port: found.Port}
			t.Logf("%s answers directly at %s", name, ep)
			return func(ctx context.Context) (*frameo.Client, error) {
				p, err := sdg.DialLocal(ctx, ep, peer, id, sdg.LocalProtocol, &sdg.Options{Logger: log})
				if err != nil {
					return nil, err
				}
				return frameo.NewClient(p, &frameo.Options{Logger: log, Name: "unframeo live test"}), nil
			}, "the local network", log
		}
		t.Logf("%s did not answer mDNS; going through the relay", name)
	}

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
	return func(ctx context.Context) (*frameo.Client, error) {
		p, err := g.Connect(ctx, peer, sdg.FrameoProtocol)
		if err != nil {
			return nil, err
		}
		return frameo.NewClient(p, &frameo.Options{Logger: log, Name: "unframeo live test"}), nil
	}, "the relay", log
}

// openN opens n connections at once and returns the ones that came up.
// Opening them concurrently is the point: a frame that serves one caller at a
// time is most likely to show it here.
func openN(ctx context.Context, t *testing.T, open opener, n int) []*frameo.Client {
	t.Helper()
	var mu sync.Mutex
	var clients []*frameo.Client
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			c, err := open(ctx)
			took := time.Since(start).Round(time.Millisecond)
			if err != nil {
				t.Logf("connection %d failed after %v: %v", i, took, err)
				return
			}
			t.Logf("connection %d up in %v", i, took)
			mu.Lock()
			clients = append(clients, c)
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, c := range clients {
		t.Cleanup(func() { _ = c.Close() })
	}
	return clients
}

// TestLiveParallelConnections asks whether the frame accepts several
// connections at the same time, and whether every one of them answers.
func TestLiveParallelConnections(t *testing.T) {
	open, via, _ := connector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const want = 4
	clients := openN(ctx, t, open, want)
	t.Logf("%d of %d connections over %s came up", len(clients), want, via)
	if len(clients) == 0 {
		t.Fatal("no connection came up at all")
	}

	// A connection that completes the handshake and then answers nothing is
	// how a frame refuses, so each one is asked something.
	answered := make([]bool, len(clients))
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ask, cancelAsk := context.WithTimeout(ctx, 20*time.Second)
			defer cancelAsk()
			info, err := c.GetInfo(ask)
			if err != nil {
				t.Logf("connection %d did not answer: %v", i, err)
				return
			}
			answered[i] = true
			t.Logf("connection %d answered: %q", i, info.GetName())
		}()
	}
	wg.Wait()
	n := 0
	for _, ok := range answered {
		if ok {
			n++
		}
	}
	t.Logf("%d of %d simultaneous connections answered over %s", n, len(clients), via)
}

// TestLiveParallelDownloads times the same photos fetched one at a time and
// then over several connections at once.
func TestLiveParallelDownloads(t *testing.T) {
	open, via, _ := connector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	first := openN(ctx, t, open, 1)
	if len(first) != 1 {
		t.Fatal("could not connect")
	}
	items, err := first[0].ListMedia(ctx)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	const batch = 8
	var ids []int64
	for _, m := range items {
		if len(ids) == batch {
			break
		}
		ids = append(ids, m.GetMediaId())
	}
	if len(ids) < 2 {
		t.Skipf("the frame holds %d photos, too few to time", len(ids))
	}

	start := time.Now()
	var serialBytes int
	for _, id := range ids {
		d, err := first[0].GetMedia(ctx, frameo.Fetch{ID: id, Timeout: 60 * time.Second})
		if err != nil {
			t.Fatalf("serial fetch of %d: %v", id, err)
		}
		serialBytes += len(d.Data)
	}
	serial := time.Since(start)
	t.Logf("serial over %s: %d photos, %d bytes in %v", via, len(ids), serialBytes, serial.Round(time.Millisecond))
	_ = first[0].Close()

	const workers = 4
	clients := openN(ctx, t, open, workers)
	if len(clients) < 2 {
		t.Skipf("only %d connection(s) came up, so there is nothing to compare", len(clients))
	}

	start = time.Now()
	var mu sync.Mutex
	var parallelBytes, failed int
	work := make(chan int64)
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range work {
				d, err := c.GetMedia(ctx, frameo.Fetch{ID: id, Timeout: 60 * time.Second})
				mu.Lock()
				if err != nil {
					t.Logf("parallel fetch of %d: %v", id, err)
					failed++
				} else {
					parallelBytes += len(d.Data)
				}
				mu.Unlock()
			}
		}()
	}
	for _, id := range ids {
		work <- id
	}
	close(work)
	wg.Wait()
	parallel := time.Since(start)

	t.Logf("parallel over %d connections on %s: %d bytes in %v, %d failed",
		len(clients), via, parallelBytes, parallel.Round(time.Millisecond), failed)
	t.Logf("speed-up %.2fx", float64(serial)/float64(parallel))
	if failed == 0 && parallelBytes != serialBytes {
		t.Errorf("parallel fetched %d bytes, serial %d: the same photos should weigh the same",
			parallelBytes, serialBytes)
	}
}

// TestLiveHeaderProbe records what a photo's header says and what the cheapest
// useful request costs, since that is the price an enriched listing would pay
// for each photo.
func TestLiveHeaderProbe(t *testing.T) {
	open, via, _ := connector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	clients := openN(ctx, t, open, 1)
	if len(clients) != 1 {
		t.Fatal("could not connect")
	}
	c := clients[0]

	items, err := c.ListMedia(ctx)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(items) == 0 {
		t.Skip("the frame holds no photos")
	}
	id := items[0].GetMediaId()

	// A bound decides which file the frame hands over, not how it scales it:
	// the answer is either the stored photo or a ready-made thumbnail.
	for _, bound := range []int32{0, 64, 480, 500, 2000} {
		start := time.Now()
		d, err := c.GetMedia(ctx, frameo.Fetch{ID: id, Size: frameo.Size(bound), Timeout: 60 * time.Second})
		if err != nil {
			t.Errorf("bound %d: %v", bound, err)
			continue
		}
		m := d.Media
		t.Logf("bound %-5d %7d bytes in %-8v ext=%q caption=%q type=%v captureDate=%d extras=%d",
			bound, len(d.Data), time.Since(start).Round(time.Millisecond),
			m.GetFileExtension(), m.GetCaption(), m.GetType(), m.GetCaptureDate(), len(m.GetExtra()))
	}

	// Whether the header carries the caption decides whether a listing can
	// show captions at all, since the listing message has no field for one.
	for i, it := range items {
		if i == 5 {
			break
		}
		start := time.Now()
		d, err := c.GetMedia(ctx, frameo.Fetch{ID: it.GetMediaId(), Size: frameo.SizePreview, Timeout: 60 * time.Second})
		if err != nil {
			t.Errorf("thumbnail of %d: %v", it.GetMediaId(), err)
			continue
		}
		t.Logf("thumbnail of %d over %s: %6d bytes in %-8v ext=%q caption=%q type=%v",
			it.GetMediaId(), via, len(d.Data), time.Since(start).Round(time.Millisecond),
			d.Media.GetFileExtension(), d.Media.GetCaption(), d.Media.GetType())
	}
}

// TestLiveTwoTransfersAtOnce is the smallest form of the question: two photos,
// two connections, both fetched at the same moment. Each one is compared
// against the same photo fetched on its own, so a transfer that completes with
// the wrong bytes is caught as well as one that fails outright.
func TestLiveTwoTransfersAtOnce(t *testing.T) {
	open, via, _ := connector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	first := openN(ctx, t, open, 1)
	if len(first) != 1 {
		t.Fatal("could not connect")
	}
	items, err := first[0].ListMedia(ctx)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(items) < 2 {
		t.Skip("the frame holds too few photos")
	}
	ids := [2]int64{items[0].GetMediaId(), items[1].GetMediaId()}

	// What each photo weighs when nothing else is going on.
	var want [2][32]byte
	for i, id := range ids {
		d, err := first[0].GetMedia(ctx, frameo.Fetch{ID: id, Timeout: 60 * time.Second})
		if err != nil {
			t.Fatalf("fetching %d on its own: %v", id, err)
		}
		want[i] = sha256.Sum256(d.Data)
		t.Logf("photo %d on its own: %d bytes", id, len(d.Data))
	}
	_ = first[0].Close()

	clients := openN(ctx, t, open, 2)
	if len(clients) < 2 {
		t.Skipf("only %d connection(s) came up", len(clients))
	}

	start := time.Now()
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := clients[i].GetMedia(ctx, frameo.Fetch{ID: ids[i], Timeout: 60 * time.Second})
			if err != nil {
				results[i] = err
				return
			}
			if got := sha256.Sum256(d.Data); got != want[i] {
				results[i] = fmt.Errorf("photo %d arrived as %d bytes that are not the photo", ids[i], len(d.Data))
				return
			}
			t.Logf("photo %d came back whole alongside the other transfer", ids[i])
		}()
	}
	wg.Wait()
	t.Logf("two transfers at once over %s took %v", via, time.Since(start).Round(time.Millisecond))
	for i, err := range results {
		if err != nil {
			t.Logf("transfer %d failed: %v", i, err)
		}
	}

	// A collision leaves the frame serving nothing for a while, so how long it
	// takes to come back is part of what this measures. Each attempt gets a
	// fresh connection: the point is whether the frame is serving photos
	// again, not whether one connection survived.
	recovering := time.Now()
	for attempt := 1; ; attempt++ {
		after := openN(ctx, t, open, 1)
		if len(after) == 1 {
			d, err := after[0].GetMedia(ctx, frameo.Fetch{
				ID:       ids[0],
				Attempts: 1,
				Timeout:  30 * time.Second,
			})
			switch {
			case err == nil && sha256.Sum256(d.Data) == want[0]:
				t.Logf("the frame served photos again %v after the collision, on attempt %d",
					time.Since(recovering).Round(time.Second), attempt)
				return
			case err == nil:
				t.Errorf("the photo fetched %v afterwards is not the one fetched before",
					time.Since(recovering).Round(time.Second))
				return
			default:
				t.Logf("attempt %d, %v after the collision: %v",
					attempt, time.Since(recovering).Round(time.Second), err)
			}
			_ = after[0].Close()
		}
		// A failed attempt that took no time at all -- a connection that would
		// not open -- must not turn this into a spin against a frame that is
		// already struggling.
		select {
		case <-ctx.Done():
			t.Errorf("the frame was still not serving photos %v after the collision",
				time.Since(recovering).Round(time.Second))
			return
		case <-time.After(5 * time.Second):
		}
	}
}
