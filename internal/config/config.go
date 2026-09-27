// Package config loads and validates the gateway configuration.
//
// Design rule: a Config that passed Validate() is safe to use everywhere else
// in the program without re-checking. All "is this URL valid?" style questions
// are answered exactly once, here.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Duration wraps time.Duration so that JSON can carry human-readable values
// like "5s" or "250ms" instead of raw nanosecond integers.
type Duration struct {
	time.Duration
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"5s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

// Server holds listener and lifecycle settings.
type Server struct {
	Listen            string   `json:"listen"`
	ReadHeaderTimeout Duration `json:"read_header_timeout"`
	ReadTimeout       Duration `json:"read_timeout"`
	WriteTimeout      Duration `json:"write_timeout"`
	IdleTimeout       Duration `json:"idle_timeout"`
	ShutdownTimeout   Duration `json:"shutdown_timeout"`
}

// Route maps a URL path prefix to an upstream service.
type Route struct {
	Name        string `json:"name"`
	PathPrefix  string `json:"path_prefix"`
	Upstream    string `json:"upstream"`
	StripPrefix bool   `json:"strip_prefix"`

	// upstreamURL is populated by Validate; unexported so callers cannot
	// bypass validation by setting it directly.
	upstreamURL *url.URL
}

// UpstreamURL returns the parsed upstream. Only valid after Validate().
func (r *Route) UpstreamURL() *url.URL { return r.upstreamURL }

// Config is the root configuration object.
type Config struct {
	Server Server  `json:"server"`
	Routes []Route `json:"routes"`
}

// Load reads, parses, applies defaults to, and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	// Unknown fields are almost always typos ("upstrem"). Failing loudly at
	// startup is far cheaper than debugging a silently ignored setting in prod.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	s := &c.Server
	if s.Listen == "" {
		s.Listen = ":8080"
	}
	if s.ReadHeaderTimeout.Duration == 0 {
		s.ReadHeaderTimeout.Duration = 5 * time.Second
	}
	if s.ReadTimeout.Duration == 0 {
		s.ReadTimeout.Duration = 30 * time.Second
	}
	if s.WriteTimeout.Duration == 0 {
		s.WriteTimeout.Duration = 30 * time.Second
	}
	if s.IdleTimeout.Duration == 0 {
		s.IdleTimeout.Duration = 120 * time.Second
	}
	if s.ShutdownTimeout.Duration == 0 {
		s.ShutdownTimeout.Duration = 15 * time.Second
	}
}

// Validate checks the configuration and fills derived fields.
// It collects every problem instead of stopping at the first one, so an
// operator sees the whole list in a single run.
func (c *Config) Validate() error {
	var errs []error

	if len(c.Routes) == 0 {
		errs = append(errs, errors.New("at least one route is required"))
	}

	seenPrefix := make(map[string]string) // prefix -> route name
	for i := range c.Routes {
		r := &c.Routes[i]
		where := fmt.Sprintf("routes[%d] (%q)", i, r.Name)

		if r.Name == "" {
			errs = append(errs, fmt.Errorf("%s: name is required", where))
		}
		if !strings.HasPrefix(r.PathPrefix, "/") {
			errs = append(errs, fmt.Errorf("%s: path_prefix must start with '/'", where))
		}
		if prev, dup := seenPrefix[r.PathPrefix]; dup {
			errs = append(errs, fmt.Errorf("%s: path_prefix %q already used by route %q", where, r.PathPrefix, prev))
		}
		seenPrefix[r.PathPrefix] = r.Name

		u, err := url.Parse(r.Upstream)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: invalid upstream: %w", where, err))
		case u.Scheme != "http" && u.Scheme != "https":
			errs = append(errs, fmt.Errorf("%s: upstream scheme must be http or https, got %q", where, u.Scheme))
		case u.Host == "":
			errs = append(errs, fmt.Errorf("%s: upstream must include a host", where))
		default:
			r.upstreamURL = u
		}
	}

	return errors.Join(errs...)
}
