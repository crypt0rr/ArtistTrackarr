package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containrrr/shoutrrr"
	"github.com/containrrr/shoutrrr/pkg/types"
)

type NotificationSender interface {
	Validate(string) error
	Send(context.Context, string, string, string) error
}

// ErrUnsupportedTransport is returned before Shoutrrr is allowed to create a
// sender for transports whose networking cannot be brought under the
// application's resolver, redirect, and timeout policy.  Keeping this check
// at both validation and send time also protects destinations created by an
// older release.
var ErrUnsupportedTransport = errors.New("notification transport is not supported")

// ValidateTransportPolicy admits only transports for which the application
// can enforce connection-time target validation.  Discord and Telegram use
// fixed upstream endpoints; ntfy and generic HTTP(S) use the scoped client
// below.  Gotify and SMTP are intentionally rejected until transport-owned
// adapters are available.
func ValidateTransportPolicy(serviceURL string) error {
	parsed, err := url.Parse(strings.TrimSpace(serviceURL))
	if err != nil || parsed == nil {
		return ErrUnsupportedTransport
	}
	scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	switch scheme {
	case "discord", "telegram", "ntfy":
		return nil
	case "generic+http", "generic+https":
		if parsed.Host == "" {
			return ErrUnsupportedTransport
		}
		return nil
	default:
		return ErrUnsupportedTransport
	}
}

// CanonicalTransportService gives persistence a stable service label even
// when a member used the advanced raw-URL field. Generic HTTP(S) URLs are
// stored as "generic" rather than the UI's "advanced" sentinel.
func CanonicalTransportService(serviceURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(serviceURL))
	if err != nil || parsed == nil || strings.TrimSpace(parsed.Scheme) == "" {
		return "unknown"
	}
	switch strings.ToLower(parsed.Scheme) {
	case "generic+http", "generic+https":
		return "generic"
	case "discord", "telegram", "ntfy":
		return strings.ToLower(parsed.Scheme)
	default:
		return strings.ToLower(parsed.Scheme)
	}
}

type ShoutrrrSender struct {
	// AllowPrivateTargets is an explicit opt-in for self-hosted notification
	// services on a trusted LAN. It is disabled by default to keep a member
	// supplied destination from turning the application into an SSRF proxy.
	AllowPrivateTargets bool
	// SendTimeout bounds HTTP-based notification providers. Shoutrrr uses the
	// process-wide net/http client for several services, so this timeout also
	// protects those providers from hanging forever.
	SendTimeout time.Duration
	// client is owned by this sender. Shoutrrr 0.8 still uses
	// http.DefaultClient for a few services, so Send temporarily scopes this
	// client around the call while holding notificationHTTPClientMu and restores
	// the previous global immediately afterwards.
	client   *http.Client
	lookupIP func(context.Context, string, string) ([]net.IP, error)
	dial     func(context.Context, string, string) (net.Conn, error)
}

const DefaultSendTimeout = 15 * time.Second

var notificationHTTPClientMu sync.Mutex

// ConfigureHTTPClient installs the bounded client used by Shoutrrr's HTTP
// services. Redirects are checked again so a public endpoint cannot redirect
// a notification into a private network.
func ConfigureHTTPClient(timeout time.Duration, allowPrivateTargets bool) {
	if timeout <= 0 {
		timeout = DefaultSendTimeout
	}
	notificationHTTPClientMu.Lock()
	defer notificationHTTPClientMu.Unlock()
	transport := http.DefaultTransport
	if base, ok := transport.(*http.Transport); ok {
		transport = base.Clone()
	}
	http.DefaultClient = &http.Client{
		Transport: safeTransport(transport, allowPrivateTargets, net.DefaultResolver.LookupIP, (&net.Dialer{Timeout: timeout}).DialContext),
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("notification redirect limit exceeded")
			}
			if err := validateOutboundTargetWithLookup(req.Context(), req.URL.String(), allowPrivateTargets, true, net.DefaultResolver.LookupIP); err != nil {
				return err
			}
			return nil
		},
	}
}

func newHTTPClient(timeout time.Duration, allowPrivateTargets bool,
	lookup func(context.Context, string, string) ([]net.IP, error),
	dial func(context.Context, string, string) (net.Conn, error),
) *http.Client {
	if timeout <= 0 {
		timeout = DefaultSendTimeout
	}
	transport := http.DefaultTransport
	if base, ok := transport.(*http.Transport); ok {
		transport = base.Clone()
	}
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIP
	}
	if dial == nil {
		dial = (&net.Dialer{Timeout: timeout}).DialContext
	}
	return &http.Client{
		Transport: safeTransport(transport, allowPrivateTargets, lookup, dial),
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("notification redirect limit exceeded")
			}
			if err := validateOutboundTargetWithLookup(req.Context(), req.URL.String(), allowPrivateTargets, true, lookup); err != nil {
				return err
			}
			return nil
		},
	}
}

func (s ShoutrrrSender) httpClient() *http.Client {
	if s.client != nil {
		return s.client
	}
	return newHTTPClient(s.sendTimeout(), s.AllowPrivateTargets, s.lookupIP, s.dial)
}

func safeTransport(base http.RoundTripper, allowPrivate bool,
	lookup func(context.Context, string, string) ([]net.IP, error),
	dial func(context.Context, string, string) (net.Conn, error),
) http.RoundTripper {
	transport, ok := base.(*http.Transport)
	if !ok {
		fallback, fallbackOK := http.DefaultTransport.(*http.Transport)
		if !fallbackOK {
			return base
		}
		transport = fallback.Clone()
	}
	transport = transport.Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialApproved(ctx, network, address, allowPrivate, lookup, dial)
	}
	return transport
}

func dialApproved(ctx context.Context, network, address string, allowPrivate bool,
	lookup func(context.Context, string, string) ([]net.IP, error),
	dial func(context.Context, string, string) (net.Conn, error),
) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("notification destination address is invalid")
	}
	if isBlockedHost(host, allowPrivate) {
		return nil, errors.New("notification destination cannot dial a local or private network")
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ips, err := lookup(lookupCtx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("notification destination host could not be resolved")
	}
	var lastErr error
	for _, ip := range ips {
		if isBlockedIP(ip, allowPrivate) {
			lastErr = errors.New("notification destination resolved to a local or private network")
			continue
		}
		conn, dialErr := dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = errors.New("notification destination could not be reached")
	}
	return nil, lastErr
}

func (s ShoutrrrSender) sendTimeout() time.Duration {
	if s.SendTimeout <= 0 {
		return DefaultSendTimeout
	}
	return s.SendTimeout
}

func (s ShoutrrrSender) Validate(serviceURL string) error {
	if strings.TrimSpace(serviceURL) == "" {
		return errors.New("notification URL is required")
	}
	if err := ValidateTransportPolicy(serviceURL); err != nil {
		return err
	}
	if err := validateOutboundTarget(context.Background(), serviceURL, s.AllowPrivateTargets, false); err != nil {
		return err
	}
	_, err := shoutrrr.CreateSender(serviceURL)
	return err
}

func (s ShoutrrrSender) Send(ctx context.Context, serviceURL, title, body string) error {
	return s.send(ctx, serviceURL, title, body, nil)
}

// send performs one notification send. observer is intentionally internal and
// is used by the benchmark to measure time waiting for and holding the
// Shoutrrr compatibility mutex without adding production-facing metrics or
// changing the NotificationSender interface.
func (s ShoutrrrSender) send(ctx context.Context, serviceURL, title, body string,
	observer func(queueWait, clientHold time.Duration),
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateTransportPolicy(serviceURL); err != nil {
		return err
	}
	sendCtx, cancel := context.WithTimeout(ctx, s.sendTimeout())
	defer cancel()
	if err := validateOutboundTargetWithLookup(sendCtx, serviceURL, s.AllowPrivateTargets, true, s.lookupIP); err != nil {
		return err
	}
	// Several Shoutrrr services dereference http.DefaultClient internally.
	// Scope the sender-owned client only for this operation and restore the
	// caller's client even when Shoutrrr returns an error or panics.
	queueStarted := time.Now()
	notificationHTTPClientMu.Lock()
	queueWait := time.Since(queueStarted)
	previousClient := http.DefaultClient
	http.DefaultClient = s.httpClient()
	holdStarted := time.Now()
	defer func() {
		holdTime := time.Since(holdStarted)
		http.DefaultClient = previousClient
		notificationHTTPClientMu.Unlock()
		if observer != nil {
			observer(queueWait, holdTime)
		}
	}()
	sender, err := shoutrrr.CreateSender(serviceURL)
	if err != nil {
		return err
	}
	params := types.Params{}
	params.SetTitle(title)
	errs := sender.Send(body, &params)
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if err := sendCtx.Err(); err != nil {
		return err
	}
	return nil
}

var (
	destinationURLPattern = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
	credentialPattern     = regexp.MustCompile(`(?i)(password|passwd|token|secret|api[_-]?key|key)=([^&\s]+)`)
)

// RedactError removes service URLs and common credential query parameters from
// provider errors before they are persisted or shown in the admin audit. The
// underlying error remains available to the caller for retry decisions, but
// destination credentials never become application-log or database content.
func RedactError(err error) string {
	if err == nil {
		return ""
	}
	message := destinationURLPattern.ReplaceAllString(err.Error(), "[redacted destination]")
	message = credentialPattern.ReplaceAllString(message, "$1=[redacted]")
	return strings.TrimSpace(message)
}

func validateOutboundTarget(ctx context.Context, serviceURL string, allowPrivate, resolve bool) error {
	return validateOutboundTargetWithLookup(ctx, serviceURL, allowPrivate, resolve, net.DefaultResolver.LookupIP)
}

func validateOutboundTargetWithLookup(ctx context.Context, serviceURL string, allowPrivate, resolve bool,
	lookup func(context.Context, string, string) ([]net.IP, error),
) error {
	parsed, err := url.Parse(strings.TrimSpace(serviceURL))
	if err != nil {
		return errors.New("notification destination is invalid")
	}
	host := outboundHost(parsed)
	if host == "" {
		return nil
	}
	if isBlockedHost(host, allowPrivate) {
		return errors.New("notification destinations cannot target local or private networks")
	}
	if !resolve {
		return nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIP
	}
	ips, err := lookup(lookupCtx, "ip", host)
	if err != nil {
		return errors.New("notification destination host could not be resolved")
	}
	for _, ip := range ips {
		if isBlockedIP(ip, allowPrivate) {
			return errors.New("notification destination resolved to a local or private network")
		}
	}
	return nil
}

func outboundHost(parsed *url.URL) string {
	if parsed == nil {
		return ""
	}
	scheme := strings.ToLower(parsed.Scheme)
	// Discord and Telegram encode a channel/chat target in the URL host; the
	// Shoutrrr provider itself selects the fixed upstream service, so do not
	// mistake that target identifier for a network destination.
	if scheme == "discord" || scheme == "telegram" {
		return ""
	}
	return strings.TrimSpace(parsed.Hostname())
}

func isBlockedHost(host string, allowPrivate bool) bool {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return !allowPrivate
	}
	if ip := net.ParseIP(host); ip != nil {
		return isBlockedIP(ip, allowPrivate)
	}
	return false
}

func isBlockedIP(ip net.IP, allowPrivate bool) bool {
	if allowPrivate {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// Go deliberately treats shared, documentation, benchmarking, and
	// reserved address blocks as global-unicast. They are not public service
	// destinations, however, and should not be reachable through a webhook.
	for _, cidr := range []string{
		"100.64.0.0/10",   // RFC 6598 shared address space
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"240.0.0.0/4",     // reserved/future use
		"2001:db8::/32",   // IPv6 documentation
	} {
		if _, network, err := net.ParseCIDR(cidr); err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

type DestinationInput struct {
	Service  string
	RawURL   string
	Host     string
	Port     string
	Username string
	Password string
	Token    string
	Target   string
	From     string
	To       string
	Topic    string
}

func BuildURL(input DestinationInput) (string, error) {
	switch input.Service {
	case "advanced":
		return strings.TrimSpace(input.RawURL), nil
	case "discord":
		if input.Token == "" || input.Target == "" {
			return "", errors.New("Discord token and channel/webhook ID are required")
		}
		return "discord://" + url.PathEscape(input.Token) + "@" + url.PathEscape(input.Target), nil
	case "telegram":
		if input.Token == "" || input.Target == "" {
			return "", errors.New("Telegram bot token and chat are required")
		}
		q := url.Values{"chats": []string{input.Target}}
		return "telegram://" + url.PathEscape(input.Token) + "@telegram?" + q.Encode(), nil
	case "ntfy":
		host := strings.TrimSpace(input.Host)
		if host == "" {
			host = "ntfy.sh"
		}
		if input.Topic == "" {
			return "", errors.New("ntfy topic is required")
		}
		u := &url.URL{Scheme: "ntfy", Host: host, Path: "/" + input.Topic}
		if input.Username != "" {
			u.User = url.UserPassword(input.Username, input.Password)
		}
		return u.String(), nil
	case "gotify":
		if input.Host == "" || input.Token == "" {
			return "", errors.New("Gotify host and token are required")
		}
		return (&url.URL{Scheme: "gotify", Host: input.Host, Path: "/" + input.Token}).String(), nil
	case "generic":
		target, err := url.Parse(input.Target)
		if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
			return "", errors.New("webhook must be an absolute HTTP(S) URL")
		}
		return "generic+" + target.String(), nil
	case "email":
		if input.Host == "" || input.From == "" || input.To == "" {
			return "", errors.New("SMTP host, from, and recipient are required")
		}
		port := input.Port
		if port == "" {
			port = "587"
		}
		if _, err := strconv.Atoi(port); err != nil {
			return "", errors.New("SMTP port is invalid")
		}
		u := &url.URL{Scheme: "smtp", Host: net.JoinHostPort(input.Host, port), Path: "/"}
		if input.Username != "" {
			u.User = url.UserPassword(input.Username, input.Password)
		}
		u.RawQuery = url.Values{"from": []string{input.From}, "to": []string{input.To}}.Encode()
		return u.String(), nil
	default:
		return "", fmt.Errorf("unsupported destination service %q", input.Service)
	}
}
