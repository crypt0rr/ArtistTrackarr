package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/crypt0rr/artist-tracker/internal/netpolicy"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/containrrr/shoutrrr"
	"github.com/containrrr/shoutrrr/pkg/types"
	"github.com/containrrr/shoutrrr/pkg/util/jsonclient"
)

type NotificationSender interface {
	Validate(string) error
	Send(context.Context, string, string, string) error
}

// MessageLimitError identifies a payload that cannot be delivered safely by
// the selected transport.  Delivery workers persist this error and apply the
// normal retry policy, while the explicit message avoids silently dropping
// content inside a provider library.
type MessageLimitError struct {
	Service string
	Limit   int
	Length  int
	Unit    string
}

func (e *MessageLimitError) Error() string {
	if e == nil {
		return "notification message exceeds transport limit"
	}
	return fmt.Sprintf("%s notification message exceeds the %d %s transport limit (got %d)", e.Service, e.Limit, e.Unit, e.Length)
}

const (
	telegramMessageLimit   = 4096 // Telegram counts Unicode characters.
	discordMessageLimit    = 6000 // Shoutrrr chunks Discord messages up to this total.
	notificationTitleLimit = 1024
	// Generic webhooks and ntfy do not expose one portable server-side limit.
	// Keep payloads bounded so a provider-controlled title/body cannot consume
	// unbounded memory or request bandwidth. This is above every application-
	// generated digest and release message.
	genericMessageLimitBytes = 64 << 10
)

func validateNotificationMessage(serviceURL, title, body string) error {
	scheme := strings.ToLower(parsedScheme(serviceURL))
	if length := utf8.RuneCountInString(title); length > notificationTitleLimit {
		return &MessageLimitError{Service: strings.ToUpper(scheme), Limit: notificationTitleLimit, Length: length, Unit: "title characters"}
	}
	switch scheme {
	case "telegram":
		// The final Telegram representation is assembled in sendTelegram after
		// its parse-mode options are read, so it is checked there.
		return nil
	case "discord":
		length := utf8.RuneCountInString(title) + utf8.RuneCountInString(body)
		if length > discordMessageLimit {
			return &MessageLimitError{Service: "Discord", Limit: discordMessageLimit, Length: length, Unit: "characters"}
		}
	case "ntfy", "generic+http", "generic+https":
		length := len([]byte(body))
		if length > genericMessageLimitBytes {
			return &MessageLimitError{Service: strings.ToUpper(scheme), Limit: genericMessageLimitBytes, Length: length, Unit: "bytes"}
		}
	}
	return nil
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
	// client around the call while holding notificationHTTPClientGate and restores
	// the previous global immediately afterwards.
	client   *http.Client
	lookupIP func(context.Context, string, string) ([]net.IP, error)
	dial     func(context.Context, string, string) (net.Conn, error)
}

// Shoutrrr's router uses a ten-second per-send timeout. Keeping the
// application deadline aligned prevents a caller from waiting longer than
// the compatibility layer can deliver and makes retry timing predictable.
const DefaultSendTimeout = 10 * time.Second

// notificationHTTPClientGate serialises the compatibility-global swap. It is a
// one-slot channel rather than a sync.Mutex so a waiting send can abandon the
// queue when its caller's context ends, instead of blocking uninterruptibly.
//
// Every HTTP-based send in the process passes through here, so queue wait is
// unbounded in the number of destinations. The transport budget therefore must
// not start until the slot is held; see Send.
var notificationHTTPClientGate = make(chan struct{}, 1)

// NewShoutrrrSender constructs one sender-owned HTTP client and transport for
// the application lifetime. Individual sends still use shallow client copies
// so request contexts and observers remain isolated, while the underlying
// keep-alive transport is reused across destinations.
func NewShoutrrrSender(allowPrivateTargets bool, timeout time.Duration) ShoutrrrSender {
	if timeout <= 0 {
		timeout = DefaultSendTimeout
	}
	return ShoutrrrSender{
		AllowPrivateTargets: allowPrivateTargets,
		SendTimeout:         timeout,
		client:              newHTTPClient(timeout, allowPrivateTargets, nil, nil),
	}
}

// CloseIdleConnections releases pooled notification connections during a
// graceful application shutdown. It is safe to call when the sender was
// created as a zero value in tests.
func (s ShoutrrrSender) CloseIdleConnections() {
	if s.client != nil {
		s.client.CloseIdleConnections()
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
		client := *s.client
		if client.Timeout <= 0 || client.Timeout > s.sendTimeout() {
			client.Timeout = s.sendTimeout()
		}
		return &client
	}
	return newHTTPClient(s.sendTimeout(), s.AllowPrivateTargets, s.lookupIP, s.dial)
}

// observedHTTPClient returns a shallow client copy whose transport records
// connection errors. Shoutrrr's Telegram adapter can turn transport errors
// into a nil error when the response body is empty; retaining the underlying
// error lets Send report a timeout instead of incorrectly marking the delivery
// successful.
func observedHTTPClient(base *http.Client, requestCtx context.Context) (*http.Client, *observedRoundTripper) {
	if base == nil {
		base = newHTTPClient(DefaultSendTimeout, false, nil, nil)
	}
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	observer := &observedRoundTripper{base: transport, requestCtx: requestCtx}
	client := *base
	client.Transport = observer
	return &client, observer
}

type observedRoundTripper struct {
	base       http.RoundTripper
	requestCtx context.Context
	mu         sync.Mutex
	err        error
	inFlight   int
}

func (t *observedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.requestCtx != nil {
		req = req.WithContext(t.requestCtx)
	}
	t.mu.Lock()
	t.inFlight++
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.inFlight--
		t.mu.Unlock()
	}()
	response, err := t.base.RoundTrip(req)
	if err != nil {
		t.mu.Lock()
		if t.err == nil {
			t.err = err
		}
		t.mu.Unlock()
	}
	return response, err
}

func (t *observedRoundTripper) error() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

func (t *observedRoundTripper) hasInFlightRequest() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inFlight > 0
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
	if err := validateNotificationMessage(serviceURL, title, body); err != nil {
		return err
	}
	// Target validation happens before the queue and gets its own budget; it
	// performs a DNS lookup with an internal deadline of its own.
	validateCtx, cancelValidate := context.WithTimeout(ctx, s.sendTimeout())
	err := validateOutboundTargetWithLookup(validateCtx, serviceURL, s.AllowPrivateTargets, true, s.lookupIP)
	cancelValidate()
	if err != nil {
		return err
	}
	// Shoutrrr's Telegram service reaches jsonclient.DefaultClient through a
	// package-level singleton. Use the Telegram Bot API directly so this
	// transport never needs to swap process-global clients, and its request
	// carries the caller's cancellation/timeout context all the way to the
	// sender-owned HTTP client.
	if strings.EqualFold(parsedScheme(serviceURL), "telegram") {
		sendCtx, cancel := context.WithTimeout(ctx, s.sendTimeout())
		defer cancel()
		started := time.Now()
		err := s.sendTelegram(sendCtx, serviceURL, title, body)
		if observer != nil {
			observer(0, time.Since(started))
		}
		return err
	}
	// Several Shoutrrr services dereference http.DefaultClient internally.
	// Scope the sender-owned client only for this operation and restore the
	// caller's client even when Shoutrrr returns an error or panics.
	queueStarted := time.Now()
	select {
	case notificationHTTPClientGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	queueWait := time.Since(queueStarted)
	// The transport budget starts here, not when this call entered the queue.
	// Previously one context covered both, so with the gate held by another
	// send for the full send timeout, the next send in line reached the
	// transport with nothing left: it returned a deadline error having issued
	// no request at all, and the delivery layer recorded that as a real
	// failure and backed the destination off. The queue wait stays bounded
	// because the select above still honours the caller's context.
	sendCtx, cancel := context.WithTimeout(ctx, s.sendTimeout())
	defer cancel()
	previousClient := http.DefaultClient
	previousJSONClient := jsonclient.DefaultClient
	// Shoutrrr's Telegram transport uses jsonclient.DefaultClient, which is
	// initialized at package load and does not follow a later http.DefaultClient
	// swap. Install both compatibility clients around the send so Telegram and
	// the other supported transports share the same bounded, policy-enforcing
	// sender-owned client.
	scopedClient, transportObserver := observedHTTPClient(s.httpClient(), sendCtx)
	http.DefaultClient = scopedClient
	jsonclient.DefaultClient = jsonclient.NewWithHTTPClient(scopedClient)
	holdStarted := time.Now()
	defer func() {
		holdTime := time.Since(holdStarted)
		jsonclient.DefaultClient = previousJSONClient
		http.DefaultClient = previousClient
		<-notificationHTTPClientGate
		if observer != nil {
			observer(queueWait, holdTime)
		}
	}()
	router, err := shoutrrr.CreateSender(serviceURL)
	if err != nil {
		return err
	}
	// Locate the single service and invoke it directly instead of using the
	// router's asynchronous wrapper. The wrapper has its own watchdog and can
	// return while a service goroutine is still using the process-wide client;
	// direct invocation keeps the compatibility globals scoped until the
	// transport has completed or the sender-owned client timeout fires.
	service, err := router.Locate(serviceURL)
	if err != nil {
		return err
	}
	params := types.Params{}
	params.SetTitle(title)
	sendStarted := time.Now()
	if err := service.Send(body, &params); err != nil {
		return err
	}
	// Deliberately not re-checking sendCtx here. The transport has already
	// accepted the message, so reporting the deadline instead would turn a
	// delivered notification into a durable failure that is retried and sent to
	// the member twice. An expiry that matters is caught by the in-flight check
	// below, which asks whether a request is still outstanding rather than
	// whether the clock ran out after the work completed.
	if transportObserver.hasInFlightRequest() {
		return context.DeadlineExceeded
	}
	// A few Shoutrrr adapters can return just as the HTTP client's timeout
	// fires, before the shared send context observes its own deadline. Treat
	// that boundary as a failure rather than acknowledging an unconfirmed
	// notification.
	if time.Since(sendStarted) >= s.sendTimeout() {
		return context.DeadlineExceeded
	}
	if err := transportObserver.error(); err != nil {
		return err
	}
	return nil
}

type telegramSendPayload struct {
	Text                string `json:"text"`
	ChatID              string `json:"chat_id"`
	MessageThreadID     *int   `json:"message_thread_id,omitempty"`
	ParseMode           string `json:"parse_mode,omitempty"`
	DisablePreview      bool   `json:"disable_web_page_preview"`
	DisableNotification bool   `json:"disable_notification"`
}

type telegramSendResponse struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
}

func parsedScheme(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed == nil {
		return ""
	}
	return parsed.Scheme
}

func (s ShoutrrrSender) sendTelegram(ctx context.Context, serviceURL, title, body string) error {
	parsed, err := url.Parse(strings.TrimSpace(serviceURL))
	if err != nil || parsed == nil || !strings.EqualFold(parsed.Scheme, "telegram") {
		return ErrUnsupportedTransport
	}
	if parsed.User == nil {
		return errors.New("telegram bot token is invalid")
	}
	password, hasPassword := parsed.User.Password()
	if !hasPassword {
		return errors.New("telegram bot token is invalid")
	}
	token := parsed.User.Username() + ":" + password
	if !telegramTokenPattern.MatchString(token) {
		return errors.New("telegram bot token is invalid")
	}
	query := parsed.Query()
	for _, option := range []string{"preview", "notification"} {
		value := strings.TrimSpace(query.Get(option))
		if value != "" && !telegramOptionPattern.MatchString(value) {
			return errors.New("telegram option is invalid")
		}
	}
	chats := append([]string{}, query["chats"]...)
	if len(chats) == 0 {
		chats = append(chats, query["channels"]...)
	}
	var expandedChats []string
	for _, value := range chats {
		for _, chat := range strings.Split(value, ",") {
			if chat = strings.TrimSpace(chat); chat != "" {
				expandedChats = append(expandedChats, chat)
			}
		}
	}
	if len(expandedChats) == 0 {
		return errors.New("telegram chat is required")
	}
	parseMode := strings.TrimSpace(query.Get("parsemode"))
	if strings.EqualFold(parseMode, "none") {
		parseMode = ""
	}
	if parseMode != "" && !telegramParseModePattern.MatchString(parseMode) {
		return errors.New("telegram parse mode is invalid")
	}
	switch strings.ToLower(parseMode) {
	case "markdown":
		parseMode = "Markdown"
	case "markdownv2":
		parseMode = "MarkdownV2"
	case "html":
		parseMode = "HTML"
	}
	message := body
	if strings.TrimSpace(title) != "" {
		switch parseMode {
		case "":
			parseMode = "HTML"
			message = fmt.Sprintf("<b>%s</b>\n%s", html.EscapeString(title), html.EscapeString(message))
		case "HTML":
			message = fmt.Sprintf("<b>%s</b>\n%s", html.EscapeString(title), message)
		}
	}
	if length := utf8.RuneCountInString(message); length > telegramMessageLimit {
		return &MessageLimitError{Service: "Telegram", Limit: telegramMessageLimit, Length: length, Unit: "characters"}
	}
	preview := !telegramOptionDisabled(query.Get("preview"))
	notification := !telegramOptionDisabled(query.Get("notification"))
	client := s.httpClient()
	for _, chat := range expandedChats {
		chatID, thread, hasThread := strings.Cut(chat, ":")
		if strings.TrimSpace(chatID) == "" {
			return errors.New("telegram chat is invalid")
		}
		var threadID *int
		if hasThread {
			parsedThread, parseErr := strconv.Atoi(thread)
			if parseErr != nil {
				return errors.New("telegram message thread is invalid")
			}
			threadID = &parsedThread
		}
		payload := telegramSendPayload{
			Text:                message,
			ChatID:              chatID,
			MessageThreadID:     threadID,
			ParseMode:           parseMode,
			DisablePreview:      !preview,
			DisableNotification: !notification,
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		endpoint := "https://api.telegram.org/bot" + token + "/sendMessage"
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
		if readErr != nil {
			return readErr
		}
		var result telegramSendResponse
		if err := json.Unmarshal(responseBody, &result); err != nil {
			if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
				return fmt.Errorf("telegram API returned %s", response.Status)
			}
			return fmt.Errorf("telegram API returned an invalid response")
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || !result.OK {
			if result.Description != "" {
				return fmt.Errorf("telegram API error %d: %s", result.ErrorCode, result.Description)
			}
			return fmt.Errorf("telegram API returned %s", response.Status)
		}
	}
	return nil
}

func telegramOptionDisabled(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "0", "false", "no", "off":
		return true
	default:
		return false
	}
}

var (
	destinationURLPattern    = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
	credentialPattern        = regexp.MustCompile(`(?i)(password|passwd|token|secret|api[_-]?key|key)=([^&\s]+)`)
	bearerPattern            = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
	telegramTokenPattern     = regexp.MustCompile(`^[0-9]+:[a-zA-Z0-9_-]+$`)
	telegramParseModePattern = regexp.MustCompile(`(?i)^(markdown|markdownv2|html)$`)
	telegramOptionPattern    = regexp.MustCompile(`(?i)^(0|1|true|false|yes|no|on|off)$`)
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
	message = bearerPattern.ReplaceAllString(message, "Bearer [redacted]")
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
	if allowPrivate {
		return false
	}
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return isBlockedIP(ip, allowPrivate)
	}
	// Some URL parsers and libc resolvers accept the historical inet_aton
	// spellings of IPv4 addresses (for example 2130706433, 0x7f000001, or
	// 0177.0.0.1). Treat those forms as IP literals before DNS resolution so
	// an obfuscated loopback/private target cannot pass the hostname check.
	if ip := parseLegacyIPv4(host); ip != nil {
		return isBlockedIP(ip, allowPrivate)
	}
	return false
}

// parseLegacyIPv4 recognizes the numeric IPv4 forms accepted by common URL
// and socket implementations. It deliberately returns nil for ordinary DNS
// names so they continue through the resolver and are checked again at dial
// time. Components use the inet_aton bases (decimal, 0x-prefixed hex, or
// leading-zero octal) and may contain one to four components.
func parseLegacyIPv4(host string) net.IP {
	host = strings.TrimSpace(host)
	if host == "" || strings.HasSuffix(host, ".") {
		return nil
	}
	parts := strings.Split(host, ".")
	if len(parts) < 1 || len(parts) > 4 {
		return nil
	}
	values := make([]uint64, len(parts))
	for i, part := range parts {
		if part == "" {
			return nil
		}
		base := 10
		digits := part
		if strings.HasPrefix(strings.ToLower(part), "0x") {
			base, digits = 16, part[2:]
		} else if len(part) > 1 && strings.HasPrefix(part, "0") {
			base = 8
		}
		if digits == "" {
			return nil
		}
		value, err := strconv.ParseUint(digits, base, 32)
		if err != nil {
			// A leading-zero component containing 8 or 9 is not valid octal,
			// but it may still be a decimal hostname label. Do not classify it
			// as an IP literal in that case.
			return nil
		}
		values[i] = value
	}
	var value uint64
	switch len(values) {
	case 1:
		value = values[0]
	case 2:
		if values[0] > 0xff || values[1] > 0xffffff {
			return nil
		}
		value = values[0]<<24 | values[1]
	case 3:
		if values[0] > 0xff || values[1] > 0xff || values[2] > 0xffff {
			return nil
		}
		value = values[0]<<24 | values[1]<<16 | values[2]
	case 4:
		for _, part := range values {
			if part > 0xff {
				return nil
			}
		}
		value = values[0]<<24 | values[1]<<16 | values[2]<<8 | values[3]
	}
	if value > 0xffffffff {
		return nil
	}
	return net.IPv4(byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
}

func isBlockedIP(ip net.IP, allowPrivate bool) bool {
	if allowPrivate {
		return false
	}
	// IPv4-mapped IPv6 addresses inherit the IPv4 policy. Without this
	// conversion, an address such as ::ffff:127.0.0.1 can bypass the
	// loopback/private checks below on platforms where net.IP reports it as
	// a 16-byte value.
	if v4 := ip.To4(); v4 != nil && len(ip) == net.IPv6len {
		return isBlockedIP(v4, false)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// Go deliberately treats shared, documentation, benchmarking, and
	// reserved address blocks as global-unicast. They are not public service
	// destinations, however, and should not be reachable through a webhook.
	// The artwork fetcher enforces the same policy from the same list.
	return netpolicy.IsReserved(ip)
}

// DestinationInput is the form a member fills in to add a destination.
//
// Port, From and To were removed with the Gotify and SMTP transports: BuildURL
// reads none of them, its accepted set is exactly the set the transport policy
// accepts, and keeping them invited a caller to supply values nothing consumes.
type DestinationInput struct {
	Service  string
	RawURL   string
	Host     string
	Username string
	Password string
	Token    string
	Target   string
	Topic    string
}

// BuildURL constructs a transport URL for a supported destination service.
//
// The set it accepts is deliberately the set ValidateTransportPolicy accepts.
// It previously also built gotify: and smtp: URLs, which that policy rejects
// unconditionally, so those branches could only ever produce a destination that
// failed validation the moment it was used - a trap for anyone extending the
// settings form, which does not offer them either. Adding a transport means
// adding it in both places.
func BuildURL(input DestinationInput) (string, error) {
	switch input.Service {
	case "advanced":
		return strings.TrimSpace(input.RawURL), nil
	case "discord":
		if input.Token == "" || input.Target == "" {
			return "", errors.New("discord token and channel/webhook ID are required")
		}
		return "discord://" + url.PathEscape(input.Token) + "@" + url.PathEscape(input.Target), nil
	case "telegram":
		if input.Token == "" || input.Target == "" {
			return "", errors.New("telegram bot token and chat are required")
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
	case "generic":
		target, err := url.Parse(input.Target)
		if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
			return "", errors.New("webhook must be an absolute HTTP(S) URL")
		}
		return "generic+" + target.String(), nil
	default:
		return "", fmt.Errorf("unsupported destination service %q", input.Service)
	}
}
