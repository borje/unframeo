# unframeo

A standalone client that sends photos to a Frameo digital photo frame.

Frameo frames do not accept photos over any cloud API. Photos travel
peer-to-peer over Trifork's SecureDeviceGrid, an encrypted relay network, with
Frameo's own message protocol on top. This program implements both, so a frame
can be fed from a script or a server instead of from the phone app.

    unframeo pair 12345678      # once, with the code the frame is showing
    unframeo send photo.jpg     # thereafter, from anywhere

## Commands

    pair <code>          pair with the frame showing this code
    info                 describe the frame and how big its screen is
    send <file|url>...   send photos, from disk or from the web
    list                 list the photos on the frame
    get <id>... | all    copy photos off the frame into files
    hide <id>...         hide photos without removing them
    show <id>...         show photos that were hidden
    delete <id>...       remove photos from the frame
    permission view|manage
                         ask the frame's owner to let this client view, or
                         manage, its photos
    discover             find frames on the local network
    frames               list the frames already paired
    forget <name>        forget a paired frame
    name [<name>]        show, or change, the name the frame knows this
                         client by
    whoami               print this client's own identity
    raw <number>         send an empty message with that number, report replies
    help                 print the full usage, which says more than this does

Pairing is needed once per frame; the address is saved and works from anywhere
afterwards. `send` takes an http or https URL wherever it takes a path, and
fetches every photo before connecting to the frame, so a dead link costs
nothing. `get` writes `<date>_<time>_<id>.<extension>` in the current
directory, so a directory of them sorts into the order the photos were taken,
and keeps going past a photo it cannot fetch. `delete` is final; `hide` only
stops a photo being displayed.

A newly paired client may send photos and nothing more. `list`, `get`, `hide`,
`show` and `delete` need the owner's say-so, which `permission` asks for: the
frame shows an Allow prompt on its screen, so someone has to be standing at it,
and the command waits until they answer or `-timeout` runs out. `manage`
includes `view`. A command refused for want of permission says to run it.

The frame shows photos as coming from a name, and asks each client for one
when it connects. `pair` saves `user@host` as this client's; `name` shows it and
`name <name>` changes it, for every frame at once.

## Options

    -frame <name>        which paired frame to use (default: the first paired)
    -net <how>           local, relay or auto (default auto)
    -discover <dur>      how long to look for the frame locally (default 2s)
    -caption <text>      caption to send with a photo
    -fit                 fit the whole photo on screen instead of cropping
    -out <path>          where get writes: a file for one photo, a directory
                         for many
    -size <which>        which stored copy get asks for (default full)
    -wait <dur>          how long get waits for one photo before asking again
                         (default 6s)
    -timeout <dur>       give up after this long, covering the whole run
                         (default 15m)
    -single-segment      send each photo as one message instead of a series
    -config <path>       configuration file (default: under the user config dir)
    -server <host:port>  use this grid server instead of Frameo's
    -v                   log the protocol exchange

A frame on the same network is reached directly, which skips the relay and is
several times faster; one elsewhere is reached through it. `-net local` and
`-net relay` force the choice, and a network that blocks multicast falls back
to the relay silently unless `-v` is given.

The frame does not scale a photo to order: it keeps the original and a preview
whose long side is 570 pixels, and `-size` only chooses between them -- though
a bare number is passed through as it stands, for a frame that draws the line
elsewhere. A preview is a twentieth of the bytes, which is what makes fetching
a whole library over the relay practical. Downloads are named after which copy
they are, so both can sit in one directory, and `get` prints the dimensions it
measured off the photo itself -- the frame states them nowhere.

## The configuration file

One file, `unframeo/config.json` under the user config directory --
`$XDG_CONFIG_HOME` or `~/.config` on Linux -- or wherever `$UNFRAMEO_CONFIG` or
`-config` points instead. It is written for its owner alone, through a
temporary file so an interrupted write cannot leave an unusable identity
behind.

It also holds the client name, which is only a label and can be changed at
will with `name`.

It holds a private key, and that key is not a credential that can be reissued:
it *is* this client's identity. The public half is the address the frame knows,
pairing registers it on the frame, and every later connection proves possession
of the private half. Nothing else stores it and nothing can derive it again.

So a frame paired to a lost key has to be paired again -- and pairing needs the
code the frame shows on its screen, which means standing in front of it. If the
frame lives at a relative's house, that is what losing this file costs. Back it
up, but somewhere encrypted: anyone who holds a copy is this client as far as
the frame is concerned, and can send and delete photos with it.

Only `pair` creates the file, and it says so when it does. Every other command
stops and names the path it looked at, because a configuration that is not
there is as often a path this particular run did not have -- `UNFRAMEO_CONFIG`
unset in a cron job, a mistyped `-config`, a different user, a container
without the volume -- as a file that is really gone, and re-pairing is the
wrong answer to a wrong path. Where the directory is ours and empty, a
configuration was written there once and has since been removed, and the
message says that instead.

## Layout

- `internal/sdg` speaks SecureDeviceGrid: identity keys, the grid connection,
  pairing, and peer connections both relayed and direct. It is a pure-Go port
  of the [opensdg](https://github.com/Sonic-Amiga/opensdg) C library, verified
  against that implementation's own output byte for byte; the direct local
  connection is not in opensdg and was worked out against a real frame.
- `internal/mdns` finds frames on the local network, by browsing for the
  DNS-SD service they advertise.
- `internal/frameo` speaks the Frameo message protocol that rides on top.
- `cmd/unframeo` is the command line.

## Status and provenance

The protocol description this is built from was recovered by reverse
engineering the Frameo Android app. SecureDeviceGrid and the Frameo protocol
are both proprietary, and neither is documented; the transport handshake is a
variant of CurveCP, in the shape CurveZMQ gives it, and the rest was read off
the app and settled against a real frame. This is an interoperability project:
it exists because the frames accept photos from nothing but the phone app.

## Licence

GNU General Public License, version 3 or later. The full text is in `LICENSE`
and the attributions are in `NOTICE`.

The choice was not a free one. `internal/sdg` is a port of the
[opensdg](https://github.com/Sonic-Amiga/opensdg) C library, which is GPLv3,
and a port is a derivative work, so the whole program goes out on those terms.
Everything else it depends on is 3-clause BSD and asks for nothing.

opensdg's README asks that the code not be used commercially. That is not in
its licence file, and section 7 of the GPL lets a recipient drop such a term,
so this program does not carry it as a condition -- but `NOTICE` passes the
request on, because it is the author's and it is reasonable.

Reverse engineering the app is a separate question from the licence, and in
the EU it has its own answer: article 6 of directive 2009/24/EC allows
decompiling a program where that is indispensable to make an independently
written one interoperate with it, and article 8 makes any contract term to the
contrary void. A photo-sending command line is not "substantially similar in
its expression" to the phone app, which is the limit article 6 sets.

Frameo is a product of Frameo ApS and SecureDeviceGrid is Trifork's; this
program is not affiliated with or endorsed by either. None of the above is
legal advice. It is a note about what the code is made of.

## Building

    go build ./cmd/unframeo

or, without a clone,

    go install github.com/borje/unframeo/cmd/unframeo@latest

Regenerating the protobuf bindings additionally needs `protoc` and
`protoc-gen-go`, but the generated files are checked in, so an ordinary build
does not.

## Testing

    go test ./...

Everything runs offline, against a stand-in frame and a fake grid in
`internal/frameo/frameotest` and `internal/sdg/sdgtest`. That is the catch: the
stand-in was built from the same protocol notes as the client, so a mistake in
those notes passes every offline test. Only a real frame settles a question
about the protocol.

A second set of tests talks to one, and is kept behind a build tag so an
ordinary run cannot start pairing or reach the network:

    go test -tags live ./internal/frameo -v    # needs a real paired frame
    go test -tags live ./internal/sdg -run TestLiveEveryServerIsFrameo

They use the configuration this client already has, so they need a frame paired
first and powered on; with nothing paired they skip rather than fail. Set
`UNFRAMEO_FRAME` to choose between several paired frames, and `UNFRAMEO_PHOTO` to a
file to run the tests that actually send one -- they skip without it, since a
test that sends leaves a photo on a real frame. `UNFRAMEO_ASK_PERMISSION=1` runs
the test that asks the frame for permission, which needs someone at the frame
to tap Allow. The `internal/sdg` and
`internal/mdns` live tests reach Frameo's own grid servers and the local
network respectively.
