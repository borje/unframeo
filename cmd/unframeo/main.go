// SPDX-License-Identifier: GPL-3.0-or-later

// Command unframeo sends photos to a Frameo digital photo frame.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/borje/unframeo/internal/config"
	"github.com/borje/unframeo/internal/frameo"
	"github.com/borje/unframeo/internal/frameo/pb"
	"github.com/borje/unframeo/internal/mdns"
	"github.com/borje/unframeo/internal/sdg"
)

const usage = `unframeo sends photos to a Frameo digital photo frame.

Usage:
  unframeo [options] <command> [arguments]

Commands:
  pair <code>        pair with the frame showing this code
  info               describe the frame
  send <file|url>... send photos, from disk or from the web
  list               list the photos on the frame
  get <id>... | all  copy photos off the frame into files
  hide <id>...       hide photos without removing them
  show <id>...       show photos that were hidden
  delete <id>...     remove photos from the frame
  permission view|manage
                     ask the frame's owner to let this client view, or
                     manage, its photos
  discover           find frames on the local network
  frames             list paired frames
  forget <name>      forget a paired frame
  name [<name>]      show, or change, the name the frame knows this client by
  whoami             print this client's own identity
  raw <number>       send an empty message with the given number and report replies

Options:
  -frame <name>      which paired frame to use (default: the first one paired)
  -net <how>         local, relay or auto: whether to reach the frame directly
                     over the local network, through the relay, or try the
                     local network first and fall back (default auto)
  -discover <dur>    how long to look for the frame on the local network
                     (default 2s)
  -caption <text>    caption to send with a photo
  -out <path>        where get writes: a file for one photo, a directory for many
  -size <which>      which stored copy get asks for: full, preview, or a
                     pixel bound for a frame that divides them elsewhere
                     (default full)
  -wait <dur>        how long get waits for one photo before asking again (default 6s)
  -fit               fit the whole photo on screen instead of cropping to fill
  -single-segment    send each photo as one message instead of a series
  -timeout <dur>     give up after this long, covering the whole run (default 15m)
  -config <path>     configuration file (default: $UNFRAMEO_CONFIG, or
                     unframeo/config.json under the user config dir)
  -server <host:port>  use this grid server instead of Frameo's, which also
                     means the relay unless -net says otherwise
  -v                 log the protocol exchange

send takes an http or https URL wherever it takes a path. Every photo is
fetched before the frame is connected to, so a link that does not answer
costs nothing, and the format is read off the photo itself, falling back to
what the server called it and then to the URL's own extension.

get writes each photo as <date>_<time>_<id>.<extension> in the current
directory unless -out says otherwise, so a directory of them sorts into the
order the photos were taken. A frame that reports no capture date leaves the
photo named by its id alone. get keeps going past a photo it cannot fetch, so
one missing id does not cost the rest.

The frame does not scale a photo to order. It keeps two copies of each -- the
original, and a preview whose long side is 570 pixels -- and the size asked for
only chooses between them. -size preview asks for the small one and -size full
for the original; a bare number is sent to the frame as it stands, for a frame
that draws the line somewhere other than this one does. A preview is saved as
<date>_<time>_<id>_preview.<extension> and a bare number as ..._<n>px, so the
copies of one photo sit beside each other instead of the second overwriting the
first. get prints the dimensions of what actually arrived, which is the only
place they are ever stated: the frame does not say, and neither does the
listing.

A frame on the same network is reached directly, which skips the relay
entirely and is much faster for a large photo. It is found by the name it
advertises over mDNS; discover shows what that finds. A network that blocks
multicast, or a frame that is elsewhere, falls back to the relay without
saying anything unless -v is given.

delete removes a photo for good; hide keeps it on the frame and stops it
being displayed. See internal/frameo/types.go for what is known of the
protocol, including the one message number still missing.

A frame lets a newly paired client send photos and nothing more. Listing,
fetching, hiding and deleting need its owner's say-so, which permission asks
for: the frame shows an Allow prompt on its screen, so someone has to be
standing at it, and the command waits until they answer or -timeout runs out.
manage includes view.

The frame shows photos as coming from a name, and asks each client for one
when it connects. name shows what this client answers -- user@host unless it
has been changed -- and name <name> changes it, in the configuration file, for
every frame at once.

The configuration file holds a private key, and that key is this client's
identity: a frame is paired to it, so it cannot be recreated and a frame
paired to a lost one has to be paired again at the frame itself. Only pair
creates it. Every other command says where it looked and stops, because a
configuration that is not there is as often a path this run did not have --
UNFRAMEO_CONFIG unset in a cron job, a mistyped -config, another user -- as a
file that is really gone. Back the file up somewhere encrypted: anyone holding
it can send and delete photos as this client.
`

type options struct {
	out           io.Writer
	frame         string
	caption       string
	outPath       string
	sizeFlag      string
	size          photoSize
	wait          time.Duration
	fit           bool
	singleSegment bool
	timeout       time.Duration
	network       string
	discover      time.Duration
	configPath    string
	server        string
	verbose       bool
	// anonymous leaves the frame's opening GetInfo unanswered and visible,
	// for raw, whose probes are read against that baseline.
	anonymous bool
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "unframeo:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	o := options{out: stdout}
	fs := flag.NewFlagSet("unframeo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.StringVar(&o.frame, "frame", "", "which paired frame to use")
	fs.StringVar(&o.caption, "caption", "", "caption to send with a photo")
	fs.StringVar(&o.outPath, "out", "", "where get writes")
	fs.StringVar(&o.sizeFlag, "size", "", "which stored copy get asks for")
	fs.DurationVar(&o.wait, "wait", 0, "how long get waits for one photo before asking again")
	fs.BoolVar(&o.fit, "fit", false, "fit the whole photo on screen")
	fs.BoolVar(&o.singleSegment, "single-segment", false, "send each photo as one message")
	fs.DurationVar(&o.timeout, "timeout", 15*time.Minute, "give up after this long")
	fs.StringVar(&o.network, "net", networkAuto, "local, relay or auto")
	fs.DurationVar(&o.discover, "discover", 2*time.Second, "how long to look for the frame on the local network")
	fs.StringVar(&o.configPath, "config", "", "configuration file")
	fs.StringVar(&o.server, "server", "", "grid server to use")
	fs.BoolVar(&o.verbose, "v", false, "log the protocol exchange")
	if err := fs.Parse(args); err != nil {
		return errors.New("run \"unframeo\" with no arguments for usage")
	}

	switch o.network {
	case networkAuto, networkLocal, networkRelay:
	default:
		return fmt.Errorf("-net %q: expected %s, %s or %s", o.network, networkLocal, networkRelay, networkAuto)
	}
	// A window of zero or less is already over before the first query goes
	// out, which silently turns every run into a relay run: the same
	// wrong-route-without-saying-so that -net is checked to prevent.
	if o.discover <= 0 {
		return fmt.Errorf("-discover %v: expected a positive duration", o.discover)
	}
	var err error
	if o.size, err = parseSize(o.sizeFlag); err != nil {
		return err
	}

	args = fs.Args()
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command given")
	}

	cmdName := args[0]
	// Handled before the configuration is touched, so a typo or a request for
	// help does not create an identity file as a side effect.
	switch cmdName {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	}
	if !knownCommands[cmdName] {
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmdName)
	}

	cfg, err := loadConfig(cmdName, o.configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()

	cmd, rest := args[0], args[1:]
	return hintAtPermission(dispatch(ctx, cfg, &o, cmd, rest))
}

// hintAtPermission names the command that fixes a refusal for lack of
// permission, which is otherwise the least self-explanatory failure a new
// pairing meets: the frame understood perfectly well and said no.
func hintAtPermission(err error) error {
	var fe *frameo.FrameError
	if errors.As(err, &fe) && fe.Code == pb.Error_MISSING_PERMISSION {
		return fmt.Errorf("%w\n  ask for it with \"unframeo permission view\" or \"unframeo permission manage\", "+
			"and approve it on the frame", err)
	}
	return err
}

func dispatch(ctx context.Context, cfg *config.Config, o *options, cmd string, rest []string) error {
	switch cmd {
	case "pair":
		return cmdPair(ctx, cfg, o, rest)
	case "info":
		return cmdInfo(ctx, cfg, o)
	case "send":
		return cmdSend(ctx, cfg, o, rest)
	case "list":
		return cmdList(ctx, cfg, o)
	case "get":
		return cmdGet(ctx, cfg, o, rest)
	case "hide":
		return cmdSetVisible(ctx, cfg, o, rest, false)
	case "show":
		return cmdSetVisible(ctx, cfg, o, rest, true)
	case "delete":
		return cmdDelete(ctx, cfg, o, rest)
	case "permission":
		return cmdPermission(ctx, cfg, o, rest)
	case "discover":
		return cmdDiscover(ctx, cfg, o)
	case "frames":
		return cmdFrames(cfg, o)
	case "forget":
		return cmdForget(cfg, o, rest)
	case "name":
		return cmdName(cfg, o, rest)
	case "whoami":
		return cmdWhoami(cfg, o)
	case "raw":
		return cmdRaw(ctx, cfg, o, rest)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// loadConfig reads the configuration for a command, and decides what a missing
// one means for that command.
//
// Only pair creates an identity, because only pair has a reason to: the key is
// what the frame is paired to, so minting one is the start of a pairing and
// not a thing to do on the way to listing photos. Everything else fails
// instead, which is what turns a mistyped -config or an unset UNFRAMEO_CONFIG
// into a message about the path rather than a client the frame does not know.
// discover needs no identity at all -- it browses the network and pairs with
// nothing -- so it runs on a configuration that is never written.
func loadConfig(cmdName, path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	var missing *config.Missing
	if !errors.As(err, &missing) {
		return cfg, err
	}

	switch cmdName {
	case "pair":
		cfg, was, err := config.Create(path)
		if err != nil {
			return nil, err
		}
		if was.Orphaned {
			fmt.Fprintf(os.Stderr, "unframeo: created a new identity at %s, replacing the one "+
				"that was there before: any frame paired with the old identity no longer "+
				"knows this client and has to be paired again.\n", cfg.Path())
		} else {
			fmt.Fprintf(os.Stderr, "unframeo: created a new identity at %s\n", cfg.Path())
		}
		return cfg, nil
	case "discover":
		return config.Unsaved(path), nil
	}
	return nil, err
}

// knownCommands is checked before anything is read or written, so an unknown
// command has no side effects.
var knownCommands = map[string]bool{
	"pair": true, "info": true, "send": true, "list": true, "delete": true,
	"get":  true,
	"hide": true, "show": true,
	"permission": true,
	"discover":   true,
	"frames":     true, "forget": true, "name": true, "whoami": true, "raw": true,
}

// How to reach a frame. The names are the ones -net takes.
const (
	networkAuto  = "auto"
	networkLocal = "local"
	networkRelay = "relay"
)

// logger builds the protocol logger. Quiet by default, because the ordinary
// output of these commands is the answer, not a trace.
func (o *options) logger() *slog.Logger {
	level := slog.LevelWarn
	if o.verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// endpoints returns the grid servers to try.
func (o *options) endpoints() ([]sdg.Endpoint, []sdg.Key, error) {
	if o.server == "" {
		return sdg.FrameoServers, []sdg.Key{sdg.FrameoServerKey}, nil
	}
	host, portStr, err := splitHostPort(o.server)
	if err != nil {
		return nil, nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, nil, fmt.Errorf("bad port in %q", o.server)
	}
	// A server named explicitly is a test server, so its key is not pinned.
	return []sdg.Endpoint{{Host: host, Port: port}}, nil, nil
}

func splitHostPort(s string) (string, string, error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", "", fmt.Errorf("expected host:port, got %q", s)
	}
	return strings.Trim(s[:i], "[]"), s[i+1:], nil
}

// dialGrid opens a grid connection using the stored identity.
func dialGrid(ctx context.Context, cfg *config.Config, o *options) (*sdg.Grid, error) {
	id, err := cfg.Identity()
	if err != nil {
		return nil, err
	}
	servers, keys, err := o.endpoints()
	if err != nil {
		return nil, err
	}
	return sdg.Dial(ctx, servers, id, &sdg.Options{
		Logger:     o.logger(),
		ServerKeys: keys,
	})
}

// link is the frame a command is talking to and how it was reached. It prints
// as the frame's name, which is what the commands report.
type link struct {
	name string
	via  string
}

func (l *link) String() string { return l.name }

// connect opens a conversation with a paired frame. Unless -net says
// otherwise it looks for the frame on the local network first and talks to it
// directly, which keeps a photo off the relay entirely; a frame that is not
// found there is reached the long way round.
func connect(ctx context.Context, cfg *config.Config, o *options) (*frameo.Client, *link, error) {
	name, peer, err := cfg.Resolve(o.frame)
	if err != nil {
		return nil, nil, err
	}

	// Looking for the frame on this network costs a moment, and finding
	// nothing costs the whole discovery window. Dialling the grid at the same
	// time means a frame that is somewhere else is no slower to reach than it
	// was before: by the time discovery gives up, the relay is usually already
	// waiting.
	var grid <-chan gridDial
	if o.network == networkAuto {
		grid = startGrid(ctx, cfg, o)
	}

	if tryLocally(o) {
		p, ep, err := connectLocal(ctx, cfg, o, peer)
		switch {
		case err == nil:
			if grid != nil {
				go closeGrid(grid)
			}
			return newClient(p, cfg, o), &link{name: name, via: "the local network at " + ep.String()}, nil
		case o.network == networkLocal:
			return nil, nil, fmt.Errorf("frame %q could not be reached on the local network: %w", name, err)
		default:
			// The relay is the fallback for every local failure: the frame is
			// elsewhere, or multicast does not cross this network, or it is
			// there but not answering.
			o.logger().Debug("not reachable locally, falling back to the relay", "frame", name, "err", err)
		}
	}

	g, err := awaitGrid(ctx, cfg, o, grid)
	if err != nil {
		return nil, nil, err
	}
	p, err := g.Connect(ctx, peer, sdg.FrameoProtocol)
	if err != nil {
		_ = g.Close()
		if errors.Is(err, sdg.ErrPeerTimeout) {
			return nil, nil, fmt.Errorf("frame %q did not answer: it may be switched off or off the network", name)
		}
		if errors.Is(err, sdg.ErrRefused) {
			return nil, nil, fmt.Errorf("frame %q refused the connection: the pairing may have been removed on the frame", name)
		}
		return nil, nil, err
	}
	// The peer connection stands on its own, but the grid connection is no
	// longer needed once it is up.
	go func() {
		<-p.Done()
		_ = g.Close()
	}()
	return newClient(p, cfg, o), &link{name: name, via: "the relay"}, nil
}

// newClient starts the conversation over an open connection, introducing this
// client by the name the configuration gives it unless o asks it to stay
// anonymous.
func newClient(p frameo.Transport, cfg *config.Config, o *options) *frameo.Client {
	name := cfg.Name()
	if o.anonymous {
		name = ""
	}
	return frameo.NewClient(p, &frameo.Options{Logger: o.logger(), Name: name})
}

// tryLocally reports whether to look for the frame on this network. A grid
// server named with -server is a deliberate choice of route -- in practice a
// test grid -- so it is taken to mean the relay unless -net says otherwise,
// which also keeps the tests off the network.
func tryLocally(o *options) bool {
	if o.network == networkLocal {
		return true
	}
	return o.network == networkAuto && o.server == ""
}

// gridDial is one grid connection attempt, finished.
type gridDial struct {
	g   *sdg.Grid
	err error
}

// startGrid begins dialling the grid in the background.
func startGrid(ctx context.Context, cfg *config.Config, o *options) <-chan gridDial {
	ch := make(chan gridDial, 1)
	go func() {
		g, err := dialGrid(ctx, cfg, o)
		ch <- gridDial{g, err}
	}()
	return ch
}

// awaitGrid takes the grid connection already being dialled, or dials one now.
func awaitGrid(ctx context.Context, cfg *config.Config, o *options, started <-chan gridDial) (*sdg.Grid, error) {
	if started == nil {
		return dialGrid(ctx, cfg, o)
	}
	r := <-started
	return r.g, r.err
}

// closeGrid disposes of a grid connection nobody waited for, once it arrives.
func closeGrid(started <-chan gridDial) {
	if r := <-started; r.g != nil {
		_ = r.g.Close()
	}
}

// localDialTimeout bounds the direct connection once the frame has been
// found. A frame on this network is a few milliseconds away -- a TCP connect
// and three handshake round trips -- so this is generous; what it rules out is
// a frame that advertises itself but does not accept, which would otherwise
// hold the run for the dialler's own 15 seconds and then a handshake step at a
// time before the relay was tried.
const localDialTimeout = 5 * time.Second

// connectLocal finds the frame on the local network and connects straight to
// it. Discovery is bounded by -discover rather than by the run's timeout: a
// frame that is not on this network will never answer, and waiting out a long
// timeout before falling back to the relay would be the worst of both. The
// connection that follows is bounded by localDialTimeout for the same reason:
// being found is not the same as being reachable.
func connectLocal(ctx context.Context, cfg *config.Config, o *options, peer sdg.PeerID) (*sdg.Peer, sdg.Endpoint, error) {
	var none sdg.Endpoint
	id, err := cfg.Identity()
	if err != nil {
		return nil, none, err
	}

	look, cancel := context.WithTimeout(ctx, o.discover)
	defer cancel()
	found, err := mdns.Lookup(look, sdg.LocalService, func(instance string) bool {
		return sdg.IsLocalInstance(instance, peer)
	}, &mdns.Options{Logger: o.logger()})
	if err != nil {
		return nil, none, err
	}
	addr, ok := found.Addr()
	if !ok {
		return nil, none, fmt.Errorf("%s advertises itself on this network but gave no address", found.Instance)
	}

	ep := sdg.Endpoint{Host: addr.String(), Port: found.Port}
	dial, cancelDial := context.WithTimeout(ctx, localDialTimeout)
	defer cancelDial()
	p, err := sdg.DialLocal(dial, ep, peer, id, sdg.LocalProtocol, &sdg.Options{Logger: o.logger()})
	if err != nil {
		// Our own budget running out reads as a bare deadline error, which
		// says nothing about which of the two waits ended the attempt.
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return nil, ep, fmt.Errorf("%s answered discovery at %s but did not finish connecting within %v",
				found.Instance, ep, localDialTimeout)
		}
		return nil, ep, err
	}
	return p, ep, nil
}

// cmdDiscover reports the frames advertising themselves on this network,
// whether or not they are paired with this client. It is the thing to run when
// a direct connection is not happening: nothing listed means the frame is not
// visible here, which is a question about the network rather than about
// pairing.
func cmdDiscover(ctx context.Context, cfg *config.Config, o *options) error {
	look, cancel := context.WithTimeout(ctx, o.discover)
	defer cancel()

	found, err := mdns.Browse(look, sdg.LocalService, &mdns.Options{Logger: o.logger()})
	if err != nil {
		return err
	}
	if len(found) == 0 {
		fmt.Fprintf(o.out, "No frame answered on this network within %v.\n", o.discover)
		return nil
	}
	for _, s := range found {
		addr := s.Host
		if a, ok := s.Addr(); ok {
			addr = a.String()
		}
		fmt.Fprintln(o.out, sdg.Endpoint{Host: addr, Port: s.Port})
		fmt.Fprintf(o.out, "  Advertised  %s\n", s.Instance)
		fmt.Fprintf(o.out, "  Paired as   %s\n", pairedAs(cfg, s.Instance))
	}
	return nil
}

// pairedAs names the paired frame an advertised instance belongs to. The
// advertised name is the frame's peer id with its last digit cut off, so it is
// matched against each known frame rather than looked up.
func pairedAs(cfg *config.Config, instance string) string {
	for _, name := range cfg.Names() {
		_, peer, err := cfg.Resolve(name)
		if err == nil && sdg.IsLocalInstance(instance, peer) {
			return name
		}
	}
	return "not paired with this client"
}

func cmdPair(ctx context.Context, cfg *config.Config, o *options, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: unframeo pair <code>")
	}
	g, err := dialGrid(ctx, cfg, o)
	if err != nil {
		return err
	}
	defer g.Close()

	peer, err := g.Pair(ctx, args[0])
	if err != nil {
		if errors.Is(err, sdg.ErrRefused) {
			return errors.New("the grid did not recognise that code: check it is current, since codes expire")
		}
		if errors.Is(err, sdg.ErrPairingFailed) {
			return errors.New("that code was not accepted: check the digits and try again")
		}
		return err
	}
	name, err := cfg.AddFrame(o.frame, peer)
	if err != nil {
		return err
	}
	fmt.Fprintf(o.out, "Paired with %s.\n", name)
	fmt.Fprintf(o.out, "Its address is %s, saved in %s.\n", peer, cfg.Path())
	fmt.Fprintf(o.out, "The frame will show this client as %q; \"unframeo name <name>\" changes that.\n", cfg.Name())
	return nil
}

// cmdPermission asks the frame's owner to let this client do more than send.
func cmdPermission(ctx context.Context, cfg *config.Config, o *options, args []string) error {
	var p frameo.Permission
	switch {
	case len(args) == 1 && args[0] == "view":
		p = frameo.PermissionView
	case len(args) == 1 && args[0] == "manage":
		p = frameo.PermissionManage
	default:
		return errors.New("usage: unframeo permission view|manage")
	}

	c, frame, err := connect(ctx, cfg, o)
	if err != nil {
		return err
	}
	defer c.Close()

	info, err := c.GetInfo(ctx)
	if err != nil {
		return err
	}
	if frameo.Granted(info, p) {
		fmt.Fprintf(o.out, "%s already lets this client %s.\n", frame, p)
		return nil
	}
	fmt.Fprintf(o.out, "Asked %s for permission to %s. Approve it on the frame; waiting up to %s.\n",
		frame, p, o.timeout)
	if err := c.RequestPermission(ctx, p); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("nobody approved the request within %s; run it again with someone at the frame, "+
				"or a longer -timeout", o.timeout)
		}
		return err
	}
	fmt.Fprintf(o.out, "Granted.\n")
	return nil
}

// cmdName shows or changes the name the frame knows this client by.
func cmdName(cfg *config.Config, o *options, args []string) error {
	switch len(args) {
	case 0:
		if cfg.ClientName == "" {
			fmt.Fprintf(o.out, "%s (the default; nothing is saved in %s)\n", cfg.Name(), cfg.Path())
		} else {
			fmt.Fprintf(o.out, "%s\n", cfg.ClientName)
		}
		return nil
	case 1:
		if err := cfg.SetClientName(args[0]); err != nil {
			return err
		}
		fmt.Fprintf(o.out, "This client is now %q to every frame, saved in %s.\n", cfg.ClientName, cfg.Path())
		return nil
	default:
		return errors.New("usage: unframeo name [<name>]")
	}
}

func cmdInfo(ctx context.Context, cfg *config.Config, o *options) error {
	c, frame, err := connect(ctx, cfg, o)
	if err != nil {
		return err
	}
	defer c.Close()

	info, err := c.GetInfo(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(o.out, "%s\n", frame)
	fmt.Fprintf(o.out, "  Reached     over %s\n", frame.via)
	fmt.Fprintf(o.out, "  Name        %s\n", info.GetName())
	if p := info.GetPlacement(); p != "" {
		fmt.Fprintf(o.out, "  Placement   %s\n", p)
	}
	fmt.Fprintf(o.out, "  Screen      %d by %d\n", info.GetScreenWidth(), info.GetScreenHeight())
	fmt.Fprintf(o.out, "  May view    %t\n", info.GetHasPermissionViewPhotos())
	fmt.Fprintf(o.out, "  May manage  %t\n", info.GetHasPermissionManagePhotos())
	if cap := info.GetFrameCapabilities(); cap != nil && cap.GetMaxVideoWidth() > 0 {
		fmt.Fprintf(o.out, "  Max video   %d by %d\n", cap.GetMaxVideoWidth(), cap.GetMaxVideoHeight())
	}
	return nil
}

func cmdSend(ctx context.Context, cfg *config.Config, o *options, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: unframeo send <file|url>...")
	}
	// Every argument becomes a readable file before anything is connected to,
	// so an unreadable file or a URL that does not answer is reported without
	// a round trip rather than part way through a batch.
	sources, cleanup, err := resolveSources(ctx, args)
	defer cleanup()
	if err != nil {
		return err
	}

	c, frame, err := connect(ctx, cfg, o)
	if err != nil {
		return err
	}
	defer c.Close()

	for _, s := range sources {
		id, err := c.SendPhoto(ctx, frameo.Photo{
			Path:          s.path,
			Extension:     s.extension,
			Taken:         s.taken,
			Caption:       o.caption,
			Fit:           o.fit,
			SingleSegment: o.singleSegment,
		})
		if err != nil {
			return fmt.Errorf("sending %s: %w", s.name, err)
		}
		fmt.Fprintf(o.out, "Sent %s to %s as %d.\n", s.name, frame, id)
	}
	return nil
}

func cmdList(ctx context.Context, cfg *config.Config, o *options) error {
	c, frame, err := connect(ctx, cfg, o)
	if err != nil {
		return err
	}
	defer c.Close()

	items, err := c.ListMedia(ctx)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Fprintf(o.out, "%s holds no photos.\n", frame)
		return nil
	}
	fmt.Fprintf(o.out, "%s holds %s.\n", frame, summarise(items))
	fmt.Fprintf(o.out, "  %-20s  %-8s  %-16s  %-16s  %s\n", "id", "kind", "taken", "added", "visibility")
	for _, m := range items {
		shown := "hidden"
		if m.GetIsVisible() {
			shown = "shown"
		}
		fmt.Fprintf(o.out, "  %-20d  %-8s  %-16s  %-16s  %s\n",
			m.GetMediaId(), mediaKind(m.GetType()),
			listedTime(m.GetCaptureDate()), listedTime(m.GetReceiveDate()), shown)
	}
	return nil
}

// listedTime renders one of the two dates the listing carries. A frame that
// reports none says nothing about the photo, which is not the same as saying
// 1970, so it is left blank rather than formatted.
//
// The clock time comes with the date because it is what separates photos taken
// on the same day, and the listing is otherwise in the frame's own order.
func listedTime(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04")
}

// mediaKind names what a listed item is. An unrecognised number is printed as
// itself: the three known kinds came from a decompile, and a frame that returns
// a fourth should say so rather than be filed under one of them.
func mediaKind(t pb.MediaMetaData_Type) string {
	switch t {
	case pb.MediaMetaData_Type_MEDIAMETADATA_TYPE_PICTURE:
		return "photo"
	case pb.MediaMetaData_Type_MEDIAMETADATA_TYPE_VIDEO:
		return "video"
	case pb.MediaMetaData_Type_MEDIAMETADATA_TYPE_GREETING:
		return "greeting"
	default:
		return strconv.FormatInt(int64(t), 10)
	}
}

// summarise counts the listing by kind, and separately how much of it is
// hidden, since a hidden photo is still one of its kind.
func summarise(items []*pb.MediaMetaData) string {
	byKind := map[string]int{}
	var order []string
	hidden := 0
	for _, m := range items {
		kind := mediaKind(m.GetType())
		if byKind[kind] == 0 {
			order = append(order, kind)
		}
		byKind[kind]++
		if !m.GetIsVisible() {
			hidden++
		}
	}
	parts := make([]string, 0, len(order))
	for _, kind := range order {
		parts = append(parts, fmt.Sprintf("%d %s", byKind[kind], plural(kind, byKind[kind])))
	}
	// A frame holding one kind of thing is described by that kind alone:
	// "86 photos" says everything "86 items: 86 photos" does.
	out := strings.Join(parts, ", ")
	if len(parts) > 1 {
		out = fmt.Sprintf("%d %s: %s", len(items), plural("item", len(items)), out)
	}
	if hidden > 0 {
		out += fmt.Sprintf("; %d hidden", hidden)
	}
	return out
}

func plural(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// cmdGet copies photos off the frame. A photo that cannot be fetched is
// reported and the run carries on: the others are still worth having, and a
// listing naming a photo that has since been removed is an ordinary thing to
// meet. The exit status still says something went wrong.
func cmdGet(ctx context.Context, cfg *config.Config, o *options, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: unframeo get <id>... | unframeo get all")
	}
	all := len(args) == 1 && args[0] == "all"
	var ids []int64
	if !all {
		var err error
		if ids, err = parseIDs(args); err != nil {
			return err
		}
	}
	// Settle where the photos will go before opening a connection, so an
	// unusable -out costs nothing to discover.
	dir, file, err := getTarget(o.outPath, all || len(ids) > 1)
	if err != nil {
		return err
	}

	c, frame, err := connect(ctx, cfg, o)
	if err != nil {
		return err
	}
	defer c.Close()

	if all {
		items, err := c.ListMedia(ctx)
		if err != nil {
			return err
		}
		for _, m := range items {
			ids = append(ids, m.GetMediaId())
		}
		if len(ids) == 0 {
			fmt.Fprintf(o.out, "%s holds no photos.\n", frame)
			return nil
		}
	}

	var failed int
	var noted bool
	for _, id := range ids {
		d, err := c.GetMedia(ctx, frameo.Fetch{
			ID:      id,
			Size:    o.size.bound,
			Timeout: o.wait,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "unframeo: photo %d: %v\n", id, err)
			failed++
			continue
		}
		// Said once, on the first photo that shows it. A bare number is the
		// one form of -size that implies a pixel count the frame never
		// promised, and the dimensions alone do not explain themselves:
		// someone who asks for 256 and reads 570x380 concludes the flag is
		// broken rather than that the frame chose a stored copy.
		if !noted && quantised(o.size, d) {
			noted = true
			fmt.Fprintf(o.out, "Note: -size %d asked for a %d pixel bound and the frame sent %dx%d. "+
				"It chooses between two stored copies rather than scaling; -size preview and -size full name them.\n",
				o.size.bound, o.size.bound, d.Width, d.Height)
		}
		path := file
		if path == "" {
			path = filepath.Join(dir, photoFileName(id, d.Media.GetCaptureDate(), o.size.variant, d.Extension()))
		}
		// A file that cannot be written is reported like a photo that cannot be
		// fetched, for the same reason: the rest of the batch is still worth
		// having, and the count at the end says how much was lost.
		if err := writeWhole(path, d.Data); err != nil {
			fmt.Fprintf(os.Stderr, "unframeo: photo %d: %v\n", id, err)
			failed++
			continue
		}
		fmt.Fprintf(o.out, "Saved %s from %s%s, %d bytes.\n", path, frame, pixels(d), len(d.Data))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d photo(s) could not be fetched", failed, len(ids))
	}
	return nil
}

// pixels describes what arrived, for the line get prints, and says nothing at
// all when the format is not one this client can measure. It carries its own
// leading comma so the sentence closes up around it rather than leaving a gap.
func pixels(d *frameo.Download) string {
	if d.Width == 0 || d.Height == 0 {
		return ""
	}
	return fmt.Sprintf(", %dx%d", d.Width, d.Height)
}

// quantised reports whether the frame answered with a copy of its own choosing
// rather than with the bound it was asked for, which is the thing a bare -size
// hides: the request names a number of pixels and the frame picks a stored
// file.
//
// Only the measured dimensions settle it, so a photo that could not be
// measured says nothing -- and, as much to the point, neither does a frame that
// really did scale to the bound, which is what keeps this from lecturing a
// frame it does not apply to.
func quantised(size photoSize, d *frameo.Download) bool {
	if !size.probe || d.Width == 0 || d.Height == 0 {
		return false
	}
	return frameo.Size(max(d.Width, d.Height)) != size.bound
}

// photoFileName is what one photo is saved as: when it was taken, then the
// frame's own id for it, then which copy it is, then the format.
//
// The date leads so that a directory of photos sorts into the order they were
// taken, which is the order anyone looking through them wants. It is in UTC,
// matching what `unframeo list` prints, so one photo reads the same in both
// places. The id stays because it is what every other command takes -- delete,
// hide and show all name a photo by it -- so a file can be acted on without
// going back to the listing for its number.
//
// A frame that reports no capture date leaves the photo named by its id alone.
// A date of zero would say 1970 and mean nothing.
//
// The variant is there because one photo is two files. A frame keeps a preview
// alongside the original, and both come back under the same id with the same
// capture date, so without it a second fetch silently overwrites the first in
// the same directory. It names what was asked for rather than what arrived,
// because the reply says nothing about which copy it is: _preview and _256px
// are both records of a request, and the dimensions get prints are the record
// of the answer.
func photoFileName(id, captureDate int64, variant, ext string) string {
	name := strconv.FormatInt(id, 10)
	// The frame files photos under its own identifiers, which the protocol
	// allows to be negative, and a file whose name begins with a dash is read
	// as an option by most of the tools that would go on to handle it. Only
	// reachable when there is no date in front, but that is exactly the case
	// this falls back to.
	if rest, negative := strings.CutPrefix(name, "-"); negative {
		name = "n" + rest
	}
	if captureDate > 0 {
		name = time.UnixMilli(captureDate).UTC().Format("2006-01-02_150405") + "_" + name
	}
	if variant != "" {
		name += "_" + variant
	}
	return name + "." + ext
}

// getTarget works out where the photos go. One photo may be named directly;
// a batch cannot all be the same file, so -out has to be a directory then.
func getTarget(out string, batch bool) (dir, file string, err error) {
	if out == "" {
		return ".", "", nil
	}
	if batch {
		if err := os.MkdirAll(out, 0o755); err != nil {
			return "", "", err
		}
		return out, "", nil
	}
	// A single photo written into an existing directory keeps its own name
	// there, which is what naming a directory is asking for.
	if st, err := os.Stat(out); err == nil && st.IsDir() {
		return out, "", nil
	}
	if d := filepath.Dir(out); d != "" {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", "", err
		}
	}
	return "", out, nil
}

// writeWhole writes the photo, and writes it whole or not at all: an
// interrupted run must not leave a truncated file under a name that looks
// finished.
func writeWhole(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".unframeo-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// cmdSetVisible hides or shows photos, which is the reversible alternative to
// deleting them: the frame keeps the photo and stops displaying it.
func cmdSetVisible(ctx context.Context, cfg *config.Config, o *options, args []string, visible bool) error {
	verb := "hide"
	if visible {
		verb = "show"
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: unframeo %s <id>...", verb)
	}
	ids, err := parseIDs(args)
	if err != nil {
		return err
	}

	c, frame, err := connect(ctx, cfg, o)
	if err != nil {
		return err
	}
	defer c.Close()

	if err := c.SetMediaVisible(ctx, ids, visible); err != nil {
		return err
	}
	shown := "Hid"
	if visible {
		shown = "Showed"
	}
	fmt.Fprintf(o.out, "%s %d item(s) on %s.\n", shown, len(ids), frame)
	return nil
}

// photoSize is what -size settled on: the bound to send, and how the copy it
// brings back is told apart from the others on disk.
type photoSize struct {
	// bound is what goes in the request.
	bound frameo.Size
	// variant is the word worked into a downloaded photo's name, or empty for
	// the original, which keeps the name it has always had.
	variant string
	// probe says the bound was typed as a bare number, which is the one case
	// where nobody -- not the person who typed it, not this program, not the
	// reply -- can say in advance which copy will come back.
	probe bool
}

// parseSize reads -size. full and preview name the two copies a frame keeps; a
// bare number is a bound sent as it stands, which is how to find where a frame
// that divides them somewhere else puts its own boundary.
//
// A bare number gets a name of its own on disk, taken from the request rather
// than from what arrives. The reply does not say which copy it is, so _256px
// records the only thing actually known in advance, and two runs asking
// different things can never write the same file. What arrived is reported in
// pixels on the line get prints, which is where the truth about the bytes
// belongs.
func parseSize(s string) (photoSize, error) {
	switch s {
	case "", "full":
		return photoSize{}, nil
	case "preview":
		return photoSize{bound: frameo.SizePreview, variant: "preview"}, nil
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil || n < 0 {
		return photoSize{}, fmt.Errorf("-size %q: expected full, preview or a number of pixels", s)
	}
	// Zero is the very same request full makes -- the protocol reads it as no
	// bound at all -- so giving it a name of its own would file two identical
	// copies under different names.
	if n == 0 {
		return photoSize{}, nil
	}
	return photoSize{bound: frameo.Size(n), variant: fmt.Sprintf("%dpx", n), probe: true}, nil
}

func parseIDs(args []string) ([]int64, error) {
	ids := make([]int64, 0, len(args))
	for _, a := range args {
		id, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a photo id", a)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func cmdDelete(ctx context.Context, cfg *config.Config, o *options, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: unframeo delete <id>...")
	}
	ids, err := parseIDs(args)
	if err != nil {
		return err
	}

	c, frame, err := connect(ctx, cfg, o)
	if err != nil {
		return err
	}
	defer c.Close()

	if err := c.DeleteMedia(ctx, ids); err != nil {
		return err
	}
	fmt.Fprintf(o.out, "Removed %d item(s) from %s.\n", len(ids), frame)
	return nil
}

func cmdFrames(cfg *config.Config, o *options) error {
	names := cfg.Names()
	if len(names) == 0 {
		fmt.Fprintln(o.out, "No frames are paired yet.")
		return nil
	}
	for _, n := range names {
		marker := " "
		if n == cfg.DefaultFrame {
			marker = "*"
		}
		f := cfg.Frames[n]
		fmt.Fprintf(o.out, "%s %-12s %s  paired %s\n", marker, n, f.PeerID, f.PairedAt.Format("2006-01-02"))
	}
	return nil
}

func cmdForget(cfg *config.Config, o *options, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: unframeo forget <name>")
	}
	if err := cfg.RemoveFrame(args[0]); err != nil {
		return err
	}
	fmt.Fprintf(o.out, "Forgot %s. The frame still lists this client until it is removed there too.\n", args[0])
	return nil
}

func cmdWhoami(cfg *config.Config, o *options) error {
	id, err := cfg.Identity()
	if err != nil {
		return err
	}
	fmt.Fprintf(o.out, "Address  %s\n", id.Public)
	fmt.Fprintf(o.out, "Name     %s\n", cfg.Name())
	fmt.Fprintf(o.out, "Config   %s\n", cfg.Path())
	return nil
}

// cmdRaw sends an arbitrary message and prints whatever comes back. It exists
// to identify the message numbers that are not known yet: send a candidate and
// see whether the frame answers.
func cmdRaw(ctx context.Context, cfg *config.Config, o *options, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: unframeo raw <number>")
	}
	n, err := strconv.ParseInt(args[0], 10, 32)
	if err != nil {
		return fmt.Errorf("%q is not a message number", args[0])
	}

	// A probe is read against what every connection gets anyway, so the
	// frame's own GetInfo has to stay in view and nothing else may be sent:
	// an introduction would hide the one and could draw replies credited to
	// the number under test.
	o.anonymous = true
	c, frame, err := connect(ctx, cfg, o)
	if err != nil {
		return err
	}
	defer c.Close()

	if err := c.SendRaw(ctx, int32(n), nil); err != nil {
		return err
	}
	fmt.Fprintf(o.out, "Sent message %d to %s. Listening for replies until the timeout.\n", n, frame)
	for {
		select {
		case f, ok := <-c.Frames():
			if !ok {
				return nil
			}
			fmt.Fprintf(o.out, "  reply: %s\n", f)
		case <-ctx.Done():
			return nil
		}
	}
}
