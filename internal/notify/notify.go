package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/containrrr/shoutrrr"
	"github.com/containrrr/shoutrrr/pkg/types"
)

type NotificationSender interface {
	Validate(string) error
	Send(context.Context, string, string, string) error
}

type ShoutrrrSender struct{}

func (ShoutrrrSender) Validate(serviceURL string) error {
	if strings.TrimSpace(serviceURL) == "" {
		return errors.New("notification URL is required")
	}
	_, err := shoutrrr.CreateSender(serviceURL)
	return err
}

func (ShoutrrrSender) Send(ctx context.Context, serviceURL, title, body string) error {
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
