// SPDX-License-Identifier: GPL-3.0-or-later

// Package config stores this client's identity and the frames it has paired
// with.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/borje/unframeo/internal/sdg"
)

// Config is the on-disk state. The private key is the client's identity: a
// frame is paired to it, so losing the file means pairing every frame again.
type Config struct {
	PrivateKey string `json:"private_key"`
	// ClientName is what the frame shows as the sender of this client's
	// photos. Empty, in a file written before the field existed, means the
	// default; see Name.
	ClientName   string           `json:"client_name,omitempty"`
	DefaultFrame string           `json:"default_frame,omitempty"`
	Frames       map[string]Frame `json:"frames,omitempty"`

	path string
}

// DefaultClientName is user@host, which says whose machine a photo came from
// without anyone having to choose a name before the first pairing.
func DefaultClientName() string {
	name := os.Getenv("USER")
	if name == "" {
		if u, err := user.Current(); err == nil && u.Username != "" {
			name = u.Username
		} else {
			name = "unframeo"
		}
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		name += "@" + host
	}
	return name
}

// Name is the client name to introduce ourselves with: the saved one, or the
// default where a configuration predates the field. The default is not
// written back, since a configuration may be read from places a command has
// no business writing to.
func (c *Config) Name() string {
	if c.ClientName != "" {
		return c.ClientName
	}
	return DefaultClientName()
}

// SetClientName saves a new client name.
func (c *Config) SetClientName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("config: the client name cannot be empty")
	}
	c.ClientName = name
	return c.Save()
}

// Frame is one paired device.
type Frame struct {
	PeerID   string    `json:"peer_id"`
	Name     string    `json:"name,omitempty"`
	PairedAt time.Time `json:"paired_at,omitempty"`
}

// DefaultPath is where the configuration lives, honouring the UNFRAMEO_CONFIG
// override.
func DefaultPath() (string, error) {
	if p := os.Getenv("UNFRAMEO_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: cannot locate a configuration directory: %w", err)
	}
	return filepath.Join(dir, "unframeo", "config.json"), nil
}

// Missing reports that there is no configuration at Path. Orphaned
// distinguishes the two cases that need different answers: a first run, where
// creating one is right, and a configuration that was written here before and
// has since gone, where the identity it held is unrecoverable.
type Missing struct {
	Path string
	// Orphaned is set when Path's directory exists. Only Save creates that
	// directory, so its presence means a configuration was written here once.
	Orphaned bool
}

func (m *Missing) Error() string {
	if m.Orphaned {
		return fmt.Sprintf("config: no configuration at %s, but its directory exists, "+
			"so one was written there and has since been removed. The private key it "+
			"held was this client's identity and cannot be recreated: restore the file "+
			"from a backup if you have one, or pair again at the frame", m.Path)
	}
	return fmt.Sprintf("config: no configuration at %s; run \"unframeo pair <code>\" to create one", m.Path)
}

// Is reports a Missing as os.ErrNotExist, so callers can test for it either way.
func (m *Missing) Is(target error) bool { return target == os.ErrNotExist }

// missingAt builds the Missing for a path, looking at the directory to tell a
// first run from a configuration that has been lost.
//
// The directory is evidence only where it is ours. Save is the only thing that
// creates the unframeo directory under the user config dir, so finding it without
// a file in it means one was written and removed. A path given with -config or
// UNFRAMEO_CONFIG sits in a directory that exists for its own reasons, and says
// nothing either way.
func missingAt(path string) *Missing {
	m := &Missing{Path: path}
	dir := filepath.Dir(path)
	if ours, err := ownedDir(); err != nil || dir != ours {
		return m
	}
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		m.Orphaned = true
	}
	return m
}

// ownedDir is the directory this program creates for itself.
func ownedDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "unframeo"), nil
}

// resolve fills in the default path when none was given.
func resolve(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	return DefaultPath()
}

// Create writes a configuration with a fresh identity, and reports what was
// there before so the caller can say so. It is for the pairing path alone:
// the key it generates is what a frame is paired to, and a frame paired to the
// previous one no longer knows this client.
func Create(path string) (*Config, *Missing, error) {
	path, err := resolve(path)
	if err != nil {
		return nil, nil, err
	}
	was := missingAt(path)
	id, err := sdg.NewIdentity()
	if err != nil {
		return nil, nil, err
	}
	c := &Config{
		PrivateKey: id.Private.String(),
		ClientName: DefaultClientName(),
		Frames:     map[string]Frame{},
		path:       path,
	}
	if err := c.Save(); err != nil {
		return nil, nil, err
	}
	return c, was, nil
}

// Unsaved is a configuration that exists only in memory and is never written.
// It holds no identity, and is for the commands that need none and should
// still work before anything has been paired.
func Unsaved(path string) *Config {
	path, _ = resolve(path)
	return &Config{Frames: map[string]Frame{}, path: path}
}

// Load reads the configuration. It never creates one: a missing file is
// reported as *Missing, because minting a new identity is not a side effect
// any command but pairing should have. A path that is merely wrong -- a
// mistyped -config, a UNFRAMEO_CONFIG set in one shell and not another, a
// different user, a container without the volume -- then says so, instead of
// quietly becoming a second client that no frame has ever heard of.
func Load(path string) (*Config, error) {
	path, err := resolve(path)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, missingAt(path)
	}
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("config: %s is not readable: %w", path, err)
	}
	c.path = path
	if c.Frames == nil {
		c.Frames = map[string]Frame{}
	}
	if _, err := c.Identity(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Path is where this configuration was loaded from.
func (c *Config) Path() string { return c.path }

// Identity returns the client's key pair.
func (c *Config) Identity() (*sdg.Identity, error) {
	k, err := sdg.ParseKey(c.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("config: %s holds an unusable private key: %w", c.path, err)
	}
	return sdg.IdentityFromPrivate(k), nil
}

// Save writes the configuration back. The file holds a private key, so it is
// written only for its owner, and through a temporary file so a failure
// part-way cannot leave an unusable identity behind.
func (c *Config) Save() error {
	if c.path == "" {
		return errors.New("config: no path to save to")
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	data = append(data, '\n')

	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

// AddFrame records a pairing and returns the name it was filed under. Pairing
// a frame that is already known updates it in place rather than adding a
// second entry for the same device.
func (c *Config) AddFrame(name string, peer sdg.PeerID) (string, error) {
	if name == "" {
		name = c.nameFor(peer)
	}
	// Two names for one device would make the address ambiguous, so drop any
	// other entry pointing at it.
	for n, f := range c.Frames {
		if f.PeerID == peer.String() && n != name {
			delete(c.Frames, n)
			if c.DefaultFrame == n {
				c.DefaultFrame = name
			}
		}
	}
	c.Frames[name] = Frame{PeerID: peer.String(), Name: name, PairedAt: time.Now().UTC()}
	if c.DefaultFrame == "" {
		c.DefaultFrame = name
	}
	return name, c.Save()
}

// RemoveFrame forgets a pairing.
func (c *Config) RemoveFrame(name string) error {
	if _, ok := c.Frames[name]; !ok {
		return fmt.Errorf("config: no frame named %q", name)
	}
	delete(c.Frames, name)
	if c.DefaultFrame == name {
		c.DefaultFrame = ""
		if names := c.Names(); len(names) > 0 {
			c.DefaultFrame = names[0]
		}
	}
	return c.Save()
}

// Names lists the paired frames in a stable order.
func (c *Config) Names() []string { return slices.Sorted(maps.Keys(c.Frames)) }

// Resolve finds a frame by name, or the default when no name is given.
func (c *Config) Resolve(name string) (string, sdg.PeerID, error) {
	var zero sdg.PeerID
	if name == "" {
		name = c.DefaultFrame
	}
	if name == "" {
		if len(c.Frames) == 0 {
			return "", zero, errors.New("no frame is paired yet: run \"unframeo pair\" with the code the frame is showing")
		}
		return "", zero, fmt.Errorf("no default frame is set: name one of %v", c.Names())
	}
	f, ok := c.Frames[name]
	if !ok {
		return "", zero, fmt.Errorf("no frame named %q: known frames are %v", name, c.Names())
	}
	peer, err := sdg.ParseKey(f.PeerID)
	if err != nil {
		return "", zero, fmt.Errorf("frame %q has an unusable peer id: %w", name, err)
	}
	return name, peer, nil
}

// nameFor picks a name for a frame the user did not name: the one it already
// has if it is known, otherwise the next unused one.
func (c *Config) nameFor(peer sdg.PeerID) string {
	for n, f := range c.Frames {
		if f.PeerID == peer.String() {
			return n
		}
	}
	for i := 1; ; i++ {
		name := fmt.Sprintf("frame%d", i)
		if _, taken := c.Frames[name]; !taken {
			return name
		}
	}
}
