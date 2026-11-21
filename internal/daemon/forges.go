package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/forge/forgejo"
	"github.com/baney75/tentmaker/internal/forge/github"
	"github.com/baney75/tentmaker/internal/forge/gitlab"
	"github.com/baney75/tentmaker/internal/secrets"
)

// ForgeFactory builds a forge client. token reads the secret store on every
// call, so a token stored by `tentmaker connect` is picked up.
type ForgeFactory func(name string, f config.Forge, token func(context.Context) (string, error), log *slog.Logger) (core.Forge, core.StatusForge, error)

// defaultHosts are the API hosts that keep the plain "<kind>-token" name.
var defaultHosts = map[string]string{"github": "api.github.com", "gitlab": "gitlab.com"}

// SecretName is the secret holding a forge's token: "<kind>-token", or
// "<kind>-<host>-token" when the forge has a URL other than the kind's
// public default (every Forgejo URL, a GitHub Enterprise or self-hosted
// GitLab URL).
func SecretName(kind, rawURL string) string {
	if kind == "" {
		kind = "github"
	}
	if rawURL == "" {
		return kind + "-token"
	}
	u, err := url.Parse(rawURL)
	host := ""
	if err == nil {
		host = strings.ToLower(u.Host)
	}
	if host == "" || host == defaultHosts[kind] {
		return kind + "-token"
	}
	host = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-' {
			return r
		}
		return '-'
	}, host)
	name := kind + "-" + host + "-token"
	if len(name) > 64 {
		name = name[:58] + "-token"
	}
	return name
}

// ForgeConfig is the config entry for forge name; an unknown name is a
// forge of that kind with its default URL.
func ForgeConfig(cfg config.Config, name string) config.Forge {
	if name == "" {
		name = "github"
	}
	f, ok := cfg.Forges[name]
	if !ok {
		f = config.Forge{Kind: name}
	}
	if f.Kind == "" {
		f.Kind = name
	}
	return f
}

// RepoForge is the forge name a repo uses.
func RepoForge(r config.Repo) string {
	if r.Forge == "" {
		return "github"
	}
	return r.Forge
}

// DefaultForge builds the real client for f.Kind.
func DefaultForge(name string, f config.Forge, token func(context.Context) (string, error), log *slog.Logger) (core.Forge, core.StatusForge, error) {
	switch f.Kind {
	case "github", "":
		c := github.New(github.Options{BaseURL: f.URL, Token: token, Log: log})
		return c, c, nil
	case "forgejo", "gitea":
		if f.URL == "" {
			return nil, nil, fmt.Errorf("forge %q has no url; run `sudo tentmaker connect forgejo --url https://your.forge`", name)
		}
		c := forgejo.New(forgejo.Options{BaseURL: f.URL, Token: token, Log: log})
		return c, c, nil
	case "gitlab":
		base := f.URL
		if base != "" && !strings.Contains(base, "/api/v4") {
			base = strings.TrimRight(base, "/") + "/api/v4"
		}
		return nil, gitlab.New(gitlab.Options{BaseURL: base, Token: token, Log: log}), nil
	default:
		return nil, nil, fmt.Errorf("forge %q has unknown kind %q (use github, forgejo or gitlab)", name, f.Kind)
	}
}

// Forges is what BuildForges found.
type Forges struct {
	Forges       map[string]core.Forge
	StatusForges map[string]core.StatusForge
	// Tokens lists the forge names that have a token, sorted.
	Tokens   []string
	Problems []core.Problem
}

// Empty reports whether no forge has a token.
func (f Forges) Empty() bool { return len(f.Forges) == 0 && len(f.StatusForges) == 0 }

// BuildForges builds a client for every forge a repo uses whose token is in
// store. A forge without a token is not a problem (the device is a worker);
// an unreadable store or a bad forge config is.
func BuildForges(cfg config.Config, store secrets.Store, factory ForgeFactory, log *slog.Logger) Forges {
	if factory == nil {
		factory = DefaultForge
	}
	out := Forges{Forges: map[string]core.Forge{}, StatusForges: map[string]core.StatusForge{}}
	names := map[string]bool{}
	for _, r := range cfg.Repos {
		names[RepoForge(r)] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		fc := ForgeConfig(cfg, name)
		secret := SecretName(fc.Kind, fc.URL)
		if store == nil {
			continue
		}
		if _, err := store.Get(secret); err != nil {
			if !errors.Is(err, secrets.ErrNotFound) {
				out.Problems = append(out.Problems, core.Problem{
					Check: "secrets", What: fmt.Sprintf("can't read the %s token: %v", name, err),
					Fix: "run `sudo tentmaker connect " + fc.Kind + "` again",
				})
			}
			continue
		}
		token := func(ctx context.Context) (string, error) {
			b, err := store.Get(secret)
			if err != nil {
				return "", fmt.Errorf("read secret %s: %w", secret, err)
			}
			return strings.TrimSpace(string(b)), nil
		}
		f, sf, err := factory(name, fc, token, log)
		if err != nil {
			out.Problems = append(out.Problems, core.Problem{Check: "forge", What: err.Error(), Fix: "fix [forges." + name + "] in the config"})
			continue
		}
		if f != nil {
			out.Forges[name] = f
		}
		if sf != nil {
			out.StatusForges[name] = sf
		}
		out.Tokens = append(out.Tokens, name)
	}
	return out
}
