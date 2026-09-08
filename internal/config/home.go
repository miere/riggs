package config

// The App Home tab's own settings. There is one, and it is the banner.
//
// It lives in its own section rather than beside the emojis, even though one
// modal edits both. `reactions` is how Riggs answers a click in a channel;
// `home` is what its own tab looks like, and the two are pointed at different
// surfaces by different people for different reasons. That is the same call §10
// made about `review-request` and `sme-assistance`: a shared section would mean
// a future setting has to pick a side, and the wrong side is only discovered
// when changing one silently moves the other.

// Home holds the App Home tab's appearance.
type Home struct {
	// ShowBanner draws the portrait at the top of the tab.
	//
	// A POINTER, so "unset" is distinguishable from "set to false". The default
	// is on, and a plain bool would make an untouched config indistinguishable
	// from one that had deliberately turned the banner off — which is the same
	// distinction the prompt registry keeps, and for the same reason: a reset
	// has to be able to delete the key rather than write today's default into
	// the file.
	ShowBanner *bool `yaml:"show-banner"`
}

// DefaultShowBanner is what an unconfigured install draws. On: the portrait is
// the one thing on the tab that says what this app is, and a new workspace
// member opening it is exactly who needs telling.
const DefaultShowBanner = true

// ShowBanner reports whether the Home tab draws the portrait.
func (c *Config) ShowBanner() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.HomeTab.ShowBanner == nil {
		return DefaultShowBanner
	}
	return *c.HomeTab.ShowBanner
}

// BannerOverridden reports whether the setting is written down rather than
// defaulted.
func (c *Config) BannerOverridden() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.HomeTab.ShowBanner != nil
}

// SetShowBanner records the choice in the config file and in this Config.
//
// Unlike a prompt or an emoji there is no "reset": a boolean with two states
// has nothing a third would mean, and an admin who picks the default is making
// the same decision as one who picks the other. So it is always written, and
// the value goes in UNQUOTED — `show-banner: "false"` is a string, and a string
// is not a bool, which the load would then refuse.
func (c *Config) SetShowBanner(show bool) error {
	value := "false"
	if show {
		value = "true"
	}
	return c.setScalarSetting([]string{"home", "show-banner"}, value, false, func(cfg *Config) {
		v := show
		cfg.HomeTab.ShowBanner = &v
	})
}
