// SPDX-License-Identifier: GPL-3.0-or-later

//go:build live

// These tests reach a real frame over the local network rather than through
// the relay. They need the frame powered on and on the same network as this
// machine, and an existing pairing:
//
//	go test -tags live ./internal/frameo -run TestLiveLocal -v
//
// Set UNFRAMEO_FRAME to choose between several paired frames.
package frameo_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/borje/unframeo/internal/config"
	"github.com/borje/unframeo/internal/frameo"
	"github.com/borje/unframeo/internal/mdns"
	"github.com/borje/unframeo/internal/sdg"
)

// findLocal discovers the paired frame on this network.
func findLocal(t *testing.T) (sdg.Endpoint, sdg.PeerID, *sdg.Identity, *slog.Logger) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	found, err := mdns.Lookup(ctx, sdg.LocalService, func(instance string) bool {
		return sdg.IsLocalInstance(instance, peer)
	}, &mdns.Options{Logger: log})
	if err != nil {
		t.Skipf("frame %s (%s) is not visible on this network: %v", name, peer, err)
	}
	addr, ok := found.Addr()
	if !ok {
		t.Fatalf("%s advertised no address", found.Instance)
	}
	t.Logf("found %s in %v: %s advertises %s:%d",
		name, time.Since(start).Round(time.Millisecond), found.Instance, addr, found.Port)

	// The advertised name is the peer id cut to a DNS label's 63 bytes, so it
	// is one character short of the id and matching has to allow for that.
	if found.Instance == peer.String() {
		t.Logf("this frame advertises its whole peer id, unlike the one this was written against")
	}
	return sdg.Endpoint{Host: addr.String(), Port: found.Port}, peer, id, log
}

func TestLiveLocalInfo(t *testing.T) {
	ep, peer, id, log := findLocal(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	p, err := sdg.DialLocal(ctx, ep, peer, id, sdg.LocalProtocol, &sdg.Options{Logger: log})
	if err != nil {
		t.Fatalf("connecting directly to %s: %v", ep, err)
	}
	connected := time.Since(start)
	c := frameo.NewClient(p, &frameo.Options{Logger: log, Name: "unframeo live test"})
	defer c.Close()

	info, err := c.GetInfo(ctx)
	if err != nil {
		t.Fatalf("GetInfo over the local network: %v", err)
	}
	t.Logf("connected in %v; %q at %q, screen %dx%d, may view %t, may manage %t",
		connected.Round(time.Millisecond), info.GetName(), info.GetPlacement(),
		info.GetScreenWidth(), info.GetScreenHeight(),
		info.GetHasPermissionViewPhotos(), info.GetHasPermissionManagePhotos())

	if info.GetScreenWidth() == 0 || info.GetScreenHeight() == 0 {
		t.Error("the frame reported no screen size, which suggests the reply was misread")
	}
}

// TestLiveLocalServiceName records how a frame treats the service name in the
// VOCH trailer, which is the one thing the direct path has to get right and
// the one thing no reference implementation documents.
func TestLiveLocalServiceName(t *testing.T) {
	ep, peer, id, log := findLocal(t)

	cases := []struct {
		protocol string
		answers  bool
	}{
		{sdg.LocalProtocol, true},  // what the app uses on the LAN
		{sdg.FrameoProtocol, true}, // what it uses through the relay
		{"not-a-service", false},   // a name the frame does not know
	}
	for _, tc := range cases {
		t.Run(tc.protocol, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			p, err := sdg.DialLocal(ctx, ep, peer, id, tc.protocol, &sdg.Options{Logger: log})
			if err != nil {
				t.Fatalf("handshake with protocol %q: %v", tc.protocol, err)
			}
			c := frameo.NewClient(p, &frameo.Options{Logger: log, Name: "unframeo live test"})
			defer c.Close()

			// A frame that does not recognise the name still finishes the
			// handshake: the tunnel comes up with no service behind it, and
			// the silence is the refusal.
			ask, cancelAsk := context.WithTimeout(ctx, 6*time.Second)
			defer cancelAsk()
			info, err := c.GetInfo(ask)
			switch {
			case tc.answers && err != nil:
				t.Errorf("protocol %q was expected to work, but: %v", tc.protocol, err)
			case tc.answers:
				t.Logf("protocol %q answered: %q", tc.protocol, info.GetName())
			case err == nil:
				t.Errorf("protocol %q was expected to go unanswered, but the frame replied %q",
					tc.protocol, info.GetName())
			default:
				t.Logf("protocol %q went unanswered, as expected: %v", tc.protocol, err)
			}
		})
	}
}
