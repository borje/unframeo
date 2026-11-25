// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/borje/unframeo/internal/config"
	"github.com/borje/unframeo/internal/frameo"
	"github.com/borje/unframeo/internal/frameo/frameotest"
	"github.com/borje/unframeo/internal/sdg"
	"github.com/borje/unframeo/internal/sdg/sdgtest"
)

func TestDirectAddressListsWithoutDiscovery(t *testing.T) {
	path := withConfig(t)
	frame := frameotest.New()
	frame.ListType = frameo.TypeGetAllMediaMetaData
	device, err := sdgtest.NewLocalDevice(func(tun *sdgtest.Tunnel) {
		if err := frame.Serve(tun); err != nil {
			t.Errorf("frame: %v", err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddFrame("direct", sdg.PeerID(device.Long.Public)); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, "-net", "local", "-addr", fmt.Sprintf("%s:%d", device.Host(), device.Port()), "-discover", "1h", "-timeout", "3s", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "direct holds no photos.") {
		t.Fatalf("unexpected listing: %s", out)
	}
}

func TestDirectAddressRejectsInvalidRoutes(t *testing.T) {
	for _, args := range [][]string{
		{"-addr", "127.0.0.1:34541", "info"},
		{"-net", "relay", "-addr", "127.0.0.1:34541", "info"},
		{"-net", "local", "-addr", "127.0.0.1", "info"},
		{"-net", "local", "-addr", ":34541", "info"},
		{"-net", "local", "-addr", "127.0.0.1:0", "info"},
		{"-net", "local", "-addr", "127.0.0.1:65536", "info"},
		{"-net", "local", "-addr", "127.0.0.1:nope", "info"},
		{"-net", "local", "-addr", "127.0.0.1:-1", "info"},
	} {
		if _, err := runCLI(t, args...); err == nil || !strings.Contains(err.Error(), "-addr") {
			t.Errorf("%v: want address error, got %v", args, err)
		}
	}
}
