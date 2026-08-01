package janus

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

type browseCounters struct {
	listings            atomic.Uint64
	rawBypasses         atomic.Uint64
	renderAttempts      atomic.Uint64
	renderStarts        atomic.Uint64
	renderSuccesses     atomic.Uint64
	renderFailures      atomic.Uint64
	renderTimeouts      atomic.Uint64
	renderOverflows     atomic.Uint64
	renderCancellations atomic.Uint64
	renderSaturations   atomic.Uint64
}

type browseSupervisor struct {
	mu sync.Mutex

	running     int
	byRenderer  map[string]int
	byExtension map[string]int
	themes      map[string]*browseTheme
	cold        map[string]map[*App]bool
	counters    browseCounters
}

func newBrowseSupervisor() *browseSupervisor {
	return &browseSupervisor{
		byRenderer:  map[string]int{},
		byExtension: map[string]int{},
		themes:      map[string]*browseTheme{},
		cold:        map[string]map[*App]bool{},
	}
}

func (s *browseSupervisor) retainTheme(theme *browseTheme) {
	if theme == nil {
		return
	}
	s.mu.Lock()
	s.themes[theme.hash] = theme
	s.mu.Unlock()
}

func (s *browseSupervisor) theme(hash string) *browseTheme {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.themes[hash]
}

func (s *browseSupervisor) admit(renderer *browseRenderer, totalLimit int) (func(), bool) {
	s.mu.Lock()
	if s.running >= totalLimit || s.byExtension[renderer.extension] >= renderer.concurrency {
		s.mu.Unlock()
		s.counters.renderSaturations.Add(1)
		return nil, false
	}
	s.running++
	s.byRenderer[renderer.id]++
	s.byExtension[renderer.extension]++
	s.counters.renderAttempts.Add(1)
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.running--
			s.byRenderer[renderer.id]--
			s.byExtension[renderer.extension]--
			if s.byRenderer[renderer.id] == 0 {
				delete(s.byRenderer, renderer.id)
			}
			if s.byExtension[renderer.extension] == 0 {
				delete(s.byExtension, renderer.extension)
			}
			s.mu.Unlock()
		})
	}, true
}

func (s *browseSupervisor) reserveCold(app *App, hosts []string, registry *appRegistry) error {
	if registry != nil {
		registry.mu.Lock()
		defer registry.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, host := range hosts {
		if registry != nil {
			hot := registry.hostConflictLocked(host)
			if hot != "" {
				return fmt.Errorf("janus browse: cold host %q conflicts with hot claim held by app %q", host, hot)
			}
		}
		if holders := s.cold[host]; holders != nil && holders[app] {
			return fmt.Errorf("janus browse: duplicate cold host %q in one generation", host)
		}
	}
	for _, host := range hosts {
		if s.cold[host] == nil {
			s.cold[host] = map[*App]bool{}
		}
		s.cold[host][app] = true
	}
	return nil
}

func (s *browseSupervisor) releaseCold(app *App) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for host, holders := range s.cold {
		delete(holders, app)
		if len(holders) == 0 {
			delete(s.cold, host)
		}
	}
}

func (s *browseSupervisor) coldClaim(host string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cold[host]) > 0
}

func (s *browseSupervisor) coldHostUnderSuffix(suffix string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for host := range s.cold {
		if hostUnderSuffix(host, suffix) {
			return host, true
		}
	}
	return "", false
}

type browseSiteEntry struct {
	hosts   []string
	enabled bool
	handler *Handler
}

func (a *App) startBrowse() error {
	if a.state == nil {
		return nil
	}
	if err := a.buildBrowseSiteTable(); err != nil {
		return err
	}
	seen := map[string]bool{}
	var hosts []string
	for _, entry := range a.browseSites {
		if len(entry.handler.coldRoots) == 0 {
			continue
		}
		if len(entry.hosts) == 0 {
			return fmt.Errorf("janus browse: a cold-root handler must have a finite nonempty exact-host matcher")
		}
		for _, host := range entry.hosts {
			if seen[host] {
				return fmt.Errorf("janus browse: duplicate cold host %q in one generation", host)
			}
			seen[host] = true
			hosts = append(hosts, host)
		}
	}
	if err := a.state.browse.reserveCold(a, hosts, a.appsReg); err != nil {
		return err
	}
	a.coldHosts = append([]string(nil), hosts...)
	return nil
}

func (a *App) stopBrowse() {
	if a.browseCancel != nil {
		a.browseCancel()
		a.browseCancel = nil
	}
}

func (a *App) buildBrowseSiteTable() error {
	httpAppI, err := a.ctx.AppIfConfigured("http")
	if err != nil || httpAppI == nil {
		return nil
	}
	httpApp, ok := httpAppI.(*caddyhttp.App)
	if !ok {
		return fmt.Errorf("janus browse: http app is unexpected type %T", httpAppI)
	}
	var entries []browseSiteEntry
	for _, server := range httpApp.Servers {
		if err := collectBrowseRoutes(server.Routes, nil, &entries); err != nil {
			return err
		}
	}
	a.browseSites = entries
	return nil
}

func collectBrowseRoutes(routes caddyhttp.RouteList, inherited [][]string, entries *[]browseSiteEntry) error {
	for _, route := range routes {
		alternatives, err := intersectBrowseHostAlternatives(inherited, route.MatcherSets)
		if err != nil {
			return err
		}
		for _, handler := range route.Handlers {
			switch value := handler.(type) {
			case *Handler:
				var hosts []string
				if len(value.coldRoots) > 0 {
					hosts, err = exactBrowseHosts(alternatives)
					if err != nil {
						return err
					}
				} else if len(alternatives) > 0 {
					hosts = append([]string(nil), alternatives[0]...)
				}
				*entries = append(*entries, browseSiteEntry{
					hosts: hosts, enabled: value.browseEnabled, handler: value,
				})
			case *caddyhttp.Subroute:
				if err := collectBrowseRoutes(value.Routes, alternatives, entries); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func intersectBrowseHostAlternatives(inherited [][]string, sets caddyhttp.MatcherSets) ([][]string, error) {
	if len(sets) == 0 {
		return inherited, nil
	}
	var local [][]string
	for _, set := range sets {
		var current []string
		found := false
		for _, matcher := range set {
			var hosts []string
			switch value := matcher.(type) {
			case *caddyhttp.MatchHost:
				hosts = append(hosts, (*value)...)
			case caddyhttp.MatchHost:
				hosts = append(hosts, value...)
			default:
				continue
			}
			if !found {
				current = hosts
				found = true
			} else {
				current = intersectStrings(current, hosts)
			}
		}
		if found {
			local = append(local, current)
		} else {
			local = append(local, nil)
		}
	}
	if len(inherited) == 0 {
		return local, nil
	}
	var out [][]string
	for _, parent := range inherited {
		for _, child := range local {
			switch {
			case len(parent) == 0:
				out = append(out, append([]string(nil), child...))
			case len(child) == 0:
				out = append(out, append([]string(nil), parent...))
			default:
				out = append(out, intersectStrings(parent, child))
			}
		}
	}
	return out, nil
}

func intersectStrings(a, b []string) []string {
	set := map[string]bool{}
	for _, value := range a {
		set[strings.ToLower(value)] = true
	}
	var out []string
	for _, value := range b {
		value = strings.ToLower(value)
		if set[value] {
			out = append(out, value)
		}
	}
	return out
}

func exactBrowseHosts(alternatives [][]string) ([]string, error) {
	if len(alternatives) == 0 {
		return nil, fmt.Errorf("janus browse: a cold-root handler must have an exact host matcher")
	}
	normalized := make([][]string, 0, len(alternatives))
	for _, alternative := range alternatives {
		seen := map[string]bool{}
		var hosts []string
		for _, raw := range alternative {
			host := strings.ToLower(strings.TrimSuffix(raw, "."))
			if strings.ContainsAny(host, "*{}") || net.ParseIP(host) != nil || validateHostname(host) != nil {
				return nil, fmt.Errorf("janus browse: cold host matcher %q is not an exact DNS name", raw)
			}
			if seen[host] {
				return nil, fmt.Errorf("janus browse: duplicate cold host %q in one matcher alternative", host)
			}
			seen[host] = true
			hosts = append(hosts, host)
		}
		if len(hosts) == 0 {
			return nil, fmt.Errorf("janus browse: cold-root host matcher alternative is empty")
		}
		sort.Strings(hosts)
		normalized = append(normalized, hosts)
	}
	first := strings.Join(normalized[0], "\x00")
	for _, hosts := range normalized[1:] {
		if strings.Join(hosts, "\x00") != first {
			return nil, fmt.Errorf("janus browse: cold-root host matcher alternatives are ambiguous")
		}
	}
	return normalized[0], nil
}

func (a *App) browseStatus() map[string]any {
	status := map[string]any{
		"enabled":    false,
		"renderers":  []any{},
		"cold_hosts": []any{},
	}
	if a.state == nil || a.state.browse == nil {
		return status
	}
	supervisor := a.state.browse
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	enabled := false
	for _, site := range a.browseSites {
		enabled = enabled || site.enabled
	}
	status["enabled"] = enabled
	if a.browseRuntime != nil && a.browseRuntime.theme != nil {
		status["theme_hash"] = a.browseRuntime.theme.hash
		themeKind := "custom"
		if a.browseRuntime.Theme == "" {
			themeKind = "embedded"
		}
		status["theme"] = themeKind
		timeout := browseDefaultTimeout
		maxOutput := browseDefaultMaxOutput
		concurrency := browseDefaultConcurrency
		if a.browseRuntime.Timeout != nil {
			timeout = time.Duration(*a.browseRuntime.Timeout)
		}
		if a.browseRuntime.MaxOutput != nil {
			maxOutput = *a.browseRuntime.MaxOutput
		}
		if a.browseRuntime.Concurrency != nil {
			concurrency = *a.browseRuntime.Concurrency
		}
		status["limits"] = map[string]any{
			"timeout": timeout.String(), "max_output": maxOutput, "concurrency": concurrency,
		}
		var renderers []map[string]any
		activeIDs := map[string]bool{}
		var extensions []string
		for extension := range a.browseRuntime.renderers {
			extensions = append(extensions, extension)
		}
		sort.Strings(extensions)
		for _, extension := range extensions {
			renderer := a.browseRuntime.renderers[extension]
			activeIDs[renderer.id] = true
			renderers = append(renderers, map[string]any{
				"extension": extension, "content_type": renderer.contentType,
				"timeout": renderer.timeout.String(), "max_output": renderer.maxOutput,
				"concurrency": renderer.concurrency, "running": supervisor.byExtension[renderer.extension],
			})
		}
		status["renderers"] = renderers
		retired := 0
		for id, running := range supervisor.byRenderer {
			if !activeIDs[id] {
				retired += running
			}
		}
		status["retired_running"] = retired
	}
	var cold []map[string]any
	for _, site := range a.browseSites {
		if len(site.handler.coldRoots) == 0 {
			continue
		}
		for _, host := range site.hosts {
			cold = append(cold, map[string]any{"host": host, "roots": len(site.handler.coldRoots)})
		}
	}
	sort.Slice(cold, func(i, j int) bool { return cold[i]["host"].(string) < cold[j]["host"].(string) })
	status["cold_hosts"] = cold
	c := &supervisor.counters
	status["counters"] = map[string]uint64{
		"listings": c.listings.Load(), "raw_bypasses": c.rawBypasses.Load(),
		"render_attempts": c.renderAttempts.Load(), "render_starts": c.renderStarts.Load(),
		"render_successes": c.renderSuccesses.Load(), "render_failures": c.renderFailures.Load(),
		"render_timeouts": c.renderTimeouts.Load(), "render_overflows": c.renderOverflows.Load(),
		"render_cancellations": c.renderCancellations.Load(), "render_saturations": c.renderSaturations.Load(),
	}
	return status
}
