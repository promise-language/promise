package module

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Update channels (T0825). The update channel — which release stream
// `promise update` follows — is orthogonal to the active epoch (which compiler
// runs builds). It is persisted separately in <PromiseHome>/channel.
const (
	ChannelStable = "stable" // latest tagged epoch-* release
	ChannelNext   = "next"   // rolling epoch-next pre-release
)

// UpdateChannel reads the persisted update channel from <PromiseHome>/channel.
// Defaults to "stable" when the file is absent or empty.
func UpdateChannel() (string, error) {
	home, err := PromiseHome()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(home, "channel"))
	if err != nil {
		if os.IsNotExist(err) {
			return ChannelStable, nil
		}
		return "", err
	}
	ch := strings.TrimSpace(string(data))
	if ch == "" {
		return ChannelStable, nil
	}
	return ch, nil
}

// WriteUpdateChannel validates name ∈ {stable, next} and persists it to
// <PromiseHome>/channel.
func WriteUpdateChannel(name string) error {
	if name != ChannelStable && name != ChannelNext {
		return fmt.Errorf("invalid update channel %q (must be %q or %q)", name, ChannelStable, ChannelNext)
	}
	home, err := PromiseHome()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(home, "channel"), []byte(name+"\n"), 0644)
}
