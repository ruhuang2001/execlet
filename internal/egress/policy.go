package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

type Policy struct {
	TunnelIdleSeconds int         `json:"tunnel_idle_seconds,omitzero"`
	Domains           []string    `json:"domains"`
	Credential        *Credential `json:"credential,omitempty"`
}

type Credential struct {
	URL      string   `json:"url"`
	TokenEnv string   `json:"token_env"`
	Images   []string `json:"images"`
}

var label = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (p Policy) TunnelIdleTimeout() time.Duration {
	if p.TunnelIdleSeconds == 0 {
		return 2 * time.Minute
	}
	return time.Duration(p.TunnelIdleSeconds) * time.Second
}

func (p Policy) Validate() error {
	if p.TunnelIdleSeconds < 0 || int64(p.TunnelIdleSeconds) > int64((1<<63-1)/time.Second) {
		return errors.New("invalid egress tunnel idle duration")
	}
	if len(p.Domains) == 0 {
		return errors.New("filtered egress requires exact allowed domains")
	}
	for _, host := range p.Domains {
		if len(host) > 253 || !strings.Contains(host, ".") || net.ParseIP(host) != nil {
			return errors.New("invalid egress domain")
		}
		for part := range strings.SplitSeq(host, ".") {
			if !label.MatchString(part) {
				return errors.New("invalid egress domain label")
			}
		}
	}
	if c := p.Credential; c != nil {
		u, err := url.Parse(c.URL)
		if err != nil || u.Scheme != "https" || !slices.Contains(p.Domains, u.Hostname()) || u.Port() != "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Path == "" || !envName.MatchString(c.TokenEnv) || len(c.Images) == 0 {
			return errors.New("credential requires an allowed HTTPS URL, token_env and authorized images")
		}
		if os.Getenv(c.TokenEnv) == "" {
			return errors.New("credential environment variable is unset")
		}
	}
	return nil
}

var reserved = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"), netip.MustParsePrefix("168.63.129.16/32"),
}
var globalV6 = netip.MustParsePrefix("2000::/3")

func public(ip netip.Addr) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Is4In6() || ip.Is6() && !globalV6.Contains(ip) {
		return false
	}
	for _, prefix := range reserved {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func (p Policy) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "443" {
		return nil, errors.New("port not allowed")
	}
	if !slices.Contains(p.Domains, strings.ToLower(host)) {
		return nil, errors.New("domain not allowed")
	}
	var ips []netip.Addr
	for _, family := range []string{"ip4", "ip6"} {
		found, err := net.DefaultResolver.LookupNetIP(ctx, family, host)
		if err != nil {
			_, noFamily := errors.AsType[*net.AddrError](err)
			if missing, ok := errors.AsType[*net.DNSError](err); noFamily || (ok && missing.IsNotFound) {
				continue
			}
			return nil, errors.New("egress resolution failed")
		}
		for _, ip := range found {
			if family == "ip4" {
				ip = ip.Unmap()
			}
			if !public(ip) {
				return nil, fmt.Errorf("reserved address %s", ip)
			}
			ips = append(ips, ip)
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("egress domain has no public addresses")
	}
	dialer := net.Dialer{Timeout: 8 * time.Second}
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, errors.New("egress connection failed")
}
