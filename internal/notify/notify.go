package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/containrrr/shoutrrr"
	"github.com/containrrr/shoutrrr/pkg/types"
)

type NotificationSender interface {
	Validate(string) error
	Send(context.Context, string, string, string) error
}

type ShoutrrrSender struct {
	// AllowPrivateTargets is an explicit opt-in for self-hosted notification
	// services on a trusted LAN. It is disabled by default to keep a member
	// supplied destination from turning the application into an SSRF proxy.
	AllowPrivateTargets bool
}

func (s ShoutrrrSender) Validate(serviceURL string) error {
	if strings.TrimSpace(serviceURL) == "" {
		return errors.New("notification URL is required")
	}
	if err := validateOutboundTarget(context.Background(), serviceURL, s.AllowPrivateTargets, false); err != nil {
		return err
	}
	_, err := shoutrrr.CreateSender(serviceURL)
	return err
}

func (s ShoutrrrSender) Send(ctx context.Context, serviceURL, title, body string) error {
	if err := validateOutboundTarget(ctx, serviceURL, s.AllowPrivateTargets, true); err != nil {
		return err
	}
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
	return ctx.Err()
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
	ips, err := net.DefaultResolver.LookupIP(lookupCtx, "ip", host)
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
