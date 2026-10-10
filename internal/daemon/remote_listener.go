package daemon

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const DefaultRemoteListenAddr = "tcp:127.0.0.1:7331"

// RemoteListenerConfig has no identity-mode override: remote is always enforce.
type RemoteListenerConfig struct {
	Enabled        bool
	ListenAddr     string
	AllowedHosts   []string
	AllowedOrigins []string
}

func (c RemoteListenerConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if !strings.HasPrefix(c.ListenAddr, "tcp:") {
		return fmt.Errorf("daemon.remote_listener.listen_addr requires loopback TCP")
	}
	host, port, err := net.SplitHostPort(strings.TrimPrefix(c.ListenAddr, "tcp:"))
	ip := net.ParseIP(host)
	_, portErr := strconv.ParseUint(port, 10, 16)
	if err != nil || portErr != nil || ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("daemon.remote_listener.listen_addr requires a literal loopback IP and numeric port")
	}
	for _, host := range c.AllowedHosts {
		if !validRemoteHost(host) {
			return fmt.Errorf("daemon.remote_listener.allowed_hosts requires exact HTTP authorities")
		}
	}
	for _, origin := range c.AllowedOrigins {
		if !validRemoteOrigin(origin) {
			return fmt.Errorf("daemon.remote_listener.allowed_origins requires exact HTTP(S) origins")
		}
	}
	return nil
}

func validRemoteHost(host string) bool {
	if host == "" || strings.ContainsAny(host, "*/\\?#@, \t\r\n") {
		return false
	}
	u, err := url.Parse("http://" + host)
	if err != nil || u.Host != host || u.Hostname() == "" || u.User != nil || u.Path != "" {
		return false
	}
	// Require brackets for IPv6 and valid explicit port syntax when present.
	if strings.Contains(host, ":") {
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			return net.ParseIP(u.Hostname()) != nil
		}
		_, port, err := net.SplitHostPort(host)
		if err != nil || port == "" {
			return false
		}
		_, err = strconv.ParseUint(port, 10, 16)
		return err == nil
	}
	return true
}

func validRemoteOrigin(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") &&
		u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery &&
		u.Fragment == "" && validRemoteHost(u.Host) && origin == u.Scheme+"://"+u.Host
}

// RemoteHandler uses the same service router with remote authentication and
// without MCP/proxy mounts. boundHost is the listener's actual authority,
// including its allocated port when listen_addr uses port zero.
func (s *Server) RemoteHandler(boundHost string) http.Handler {
	remote := s.handler(true)
	c := s.Config.RemoteListener
	hosts := append([]string(nil), c.AllowedHosts...)
	if len(hosts) == 0 {
		hosts = []string{boundHost}
	}
	origins := append([]string(nil), c.AllowedOrigins...)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !containsExact(hosts, r.Host) {
			http.Error(w, "remote Host refused", http.StatusForbidden)
			return
		}
		if values := r.Header.Values("Origin"); len(values) != 0 {
			allowed := len(values) == 1 && validRemoteOrigin(values[0])
			if len(origins) != 0 {
				allowed = allowed && containsExact(origins, values[0])
			} else {
				allowed = allowed && values[0] == "http://"+r.Host
			}
			if !allowed {
				http.Error(w, "remote Origin refused", http.StatusForbidden)
				return
			}
		}
		remote.ServeHTTP(w, r)
	})
}

func containsExact(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
