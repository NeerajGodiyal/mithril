// Package notifier delivers Alertmanager alerts to Telegram. It is delivery,
// not authorization: nothing here decides whether an alert is real, and a
// delivery failure never resolves or deletes the Alertmanager record.
package notifier

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Overclock-Validator/mithril/internal/safefile"
	toml "github.com/pelletier/go-toml/v2"
)

// DefaultConfigPath is the runtime location holding the bot token, the chat
// allowlist and the TLS material. Nothing in this file may be committed.
const DefaultConfigPath = "/etc/mithril-notifier/config.toml"

const (
	maxConfigBytes           = 64 << 10
	maxSecretBytes           = 4 << 10
	maxAllowedChatIDs        = 8
	maxSESIdentityBytes      = 254
	alertRepeatInterval      = 4 * time.Hour
	DefaultSendTimeout       = 10 * time.Second
	DefaultProbeInterval     = time.Hour
	DefaultDedupTTL          = alertRepeatInterval
	DefaultWebhookTimeout    = 30 * time.Second
	minConfiguredProbeTime   = time.Minute
	maxConfiguredProbeTime   = time.Hour
	maxConfiguredWebhookTime = DefaultWebhookTimeout
)

// Config is the notifier's runtime configuration.
//
// BotToken is a credential and AllowedChatIDs identify private destinations.
// Never format the complete Config value or place either field in logs, metric
// labels or errors.
type Config struct {
	BotToken string `toml:"bot_token"`
	// AllowedChatIDs is an allowlist of numeric chat IDs. Numeric because an
	// @username can be reassigned to another account, which would silently
	// redirect alerts to a stranger.
	AllowedChatIDs    []int64 `toml:"allowed_chat_ids"`
	TelegramAPIURL    string  `toml:"telegram_api_url"`
	SendTimeoutSec    int     `toml:"send_timeout_seconds"`
	ProbeIntervalSec  int     `toml:"probe_interval_seconds"`
	WebhookTimeoutSec int     `toml:"webhook_timeout_seconds"`

	// mTLS material. Alertmanager's generic webhook computes no body HMAC, so a
	// dedicated single-purpose client CA is the producer-verifiable boundary.
	ClientCAFile   string `toml:"client_ca_file"`
	ServerCertFile string `toml:"server_cert_file"`
	ServerKeyFile  string `toml:"server_key_file"`

	SESAddr         string `toml:"ses_addr"`
	SESUsername     string `toml:"ses_username"`
	SESPasswordFile string `toml:"ses_password_file"`
	SESFrom         string `toml:"ses_from"`
	SESCanaryTo     string `toml:"ses_canary_to"`
}

// SendTimeout bounds one delivery attempt.
func (c Config) SendTimeout() time.Duration {
	if c.SendTimeoutSec <= 0 {
		return DefaultSendTimeout
	}
	return time.Duration(c.SendTimeoutSec) * time.Second
}

// ProbeInterval returns the interval between delivery-route checks. A negative
// value disables route probing entirely; probe health then stops being
// observable, so it is opt-in rather than the default.
func (c Config) ProbeInterval() time.Duration {
	if c.ProbeIntervalSec < 0 {
		return 0
	}
	if c.ProbeIntervalSec == 0 {
		return DefaultProbeInterval
	}
	return time.Duration(c.ProbeIntervalSec) * time.Second
}

// ProbeDisabled reports whether route probing is switched off.
func (c Config) ProbeDisabled() bool { return c.ProbeIntervalSec < 0 }

// WebhookTimeout bounds all Telegram work caused by one Alertmanager request.
func (c Config) WebhookTimeout() time.Duration {
	if c.WebhookTimeoutSec <= 0 {
		return DefaultWebhookTimeout
	}
	return time.Duration(c.WebhookTimeoutSec) * time.Second
}

// APIBase returns the Telegram API root, overridable so tests never contact the
// real service.
func (c Config) APIBase() string {
	if c.TelegramAPIURL == "" {
		return "https://api.telegram.org"
	}
	return c.TelegramAPIURL
}

// SESConfigured reports whether the complete SES canary route is present.
func (c Config) SESConfigured() bool {
	return c.SESAddr != "" &&
		c.SESUsername != "" &&
		c.SESPasswordFile != "" &&
		c.SESFrom != "" &&
		c.SESCanaryTo != ""
}

// LoadConfig reads and validates the runtime configuration.
//
// The file holds a bot token, so it must not be group- or other-readable.
// Validation errors identify fields, while decode errors stay generic because
// a parser error can quote the credential-bearing input.
func LoadConfig(path string) (Config, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return Config{}, errors.New("notifier config path must be a clean absolute path")
	}
	data, err := readConfigFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		// A decoder error quotes file content, which here is the bot token.
		return Config{}, errors.New("notifier config file is malformed")
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// readConfigFile validates and reads one file object. Checking a pathname and
// then reopening it would let a replacement bypass the permissions and symlink
// checks between those operations.
func readConfigFile(path string) ([]byte, error) {
	return readPrivateRegularFile(path, maxConfigBytes, "notifier config file")
}

// LoadSecretFile reads one small service-private secret without following
// symlinks or reopening the pathname after validation. Provision separate 0600
// credentials for services running as different users, even when both copies
// come from the same secret source.
func LoadSecretFile(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("notifier secret path must be a clean absolute path")
	}
	data, err := readPrivateRegularFile(path, maxSecretBytes, "notifier secret file")
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" || strings.ContainsAny(secret, "\r\n") {
		return "", errors.New("notifier secret file is invalid")
	}
	return secret, nil
}

func readPrivateRegularFile(path string, maxBytes int64, label string) ([]byte, error) {
	// O_NOFOLLOW covers only the final component, so reject symlinks in ancestor
	// directories too.
	data, err := safefile.ReadTrustedRegular(path, safefile.ReadOptions{
		MaxBytes:               maxBytes,
		ForbiddenPerm:          0o077,
		RejectAncestorSymlinks: true,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	return data, nil
}

func (c Config) validate() error {
	if !validBotToken(c.BotToken) {
		return errors.New("notifier config bot_token is invalid")
	}
	if !validTelegramAPIBase(c.TelegramAPIURL) {
		return errors.New("notifier config telegram_api_url must use Telegram or a loopback test endpoint")
	}
	if len(c.AllowedChatIDs) == 0 {
		// An empty allowlist would deliver nowhere, which looks identical to a
		// broken notifier at exactly the moment an alert matters.
		return errors.New("notifier config allowed_chat_ids must list at least one numeric chat ID")
	}
	if len(c.AllowedChatIDs) > maxAllowedChatIDs {
		return fmt.Errorf("notifier config allowed_chat_ids must contain at most %d entries", maxAllowedChatIDs)
	}
	seenChatIDs := make(map[int64]struct{}, len(c.AllowedChatIDs))
	for _, id := range c.AllowedChatIDs {
		if id == 0 {
			return errors.New("notifier config allowed_chat_ids must not contain 0")
		}
		if _, exists := seenChatIDs[id]; exists {
			return errors.New("notifier config allowed_chat_ids must not contain duplicates")
		}
		seenChatIDs[id] = struct{}{}
	}
	if c.SendTimeoutSec < 0 ||
		c.SendTimeoutSec > int(maxConfiguredWebhookTime/time.Second) {
		return errors.New("notifier config send_timeout_seconds must be between 0 and 30")
	}
	// -1 disables probing; 0 takes the default. The alert contract requires a
	// successful canary within two hours, so configured probes cannot exceed one.
	if c.ProbeIntervalSec < -1 ||
		(c.ProbeIntervalSec > 0 && c.ProbeIntervalSec < int(minConfiguredProbeTime/time.Second)) ||
		c.ProbeIntervalSec > int(maxConfiguredProbeTime/time.Second) {
		return errors.New("notifier config probe_interval_seconds must be -1 (disabled), 0 (default), or between 60 and 3600")
	}
	if c.WebhookTimeoutSec < 0 ||
		c.WebhookTimeoutSec > int(maxConfiguredWebhookTime/time.Second) {
		return errors.New("notifier config webhook_timeout_seconds must be between 0 and 30")
	}
	if c.SendTimeout() > c.WebhookTimeout() {
		return errors.New("notifier config send timeout must not exceed webhook timeout")
	}
	for _, f := range []struct{ name, value string }{
		{"client_ca_file", c.ClientCAFile},
		{"server_cert_file", c.ServerCertFile},
		{"server_key_file", c.ServerKeyFile},
	} {
		if f.value == "" {
			return fmt.Errorf("notifier config %s is required", f.name)
		}
		if !filepath.IsAbs(f.value) || filepath.Clean(f.value) != f.value {
			return fmt.Errorf("notifier config %s must be an absolute path", f.name)
		}
	}
	sesFields := []string{c.SESAddr, c.SESUsername, c.SESPasswordFile, c.SESFrom, c.SESCanaryTo}
	var sesFieldCount int
	for _, value := range sesFields {
		if value != "" {
			sesFieldCount++
		}
	}
	if sesFieldCount != 0 && sesFieldCount != len(sesFields) {
		return errors.New("notifier config SES probe fields must be configured together")
	}
	if c.SESConfigured() {
		if !validSESAddr(c.SESAddr, telegramAPIIsLoopback(c.TelegramAPIURL)) {
			return errors.New("notifier config ses_addr must be an SES STARTTLS endpoint")
		}
		if !validSESUsername(c.SESUsername) {
			return errors.New("notifier config ses_username is invalid")
		}
		if !filepath.IsAbs(c.SESPasswordFile) || filepath.Clean(c.SESPasswordFile) != c.SESPasswordFile {
			return errors.New("notifier config ses_password_file must be an absolute path")
		}
		if !validMailbox(c.SESFrom) {
			return errors.New("notifier config ses_from must be one plain mailbox address")
		}
		if !validMailbox(c.SESCanaryTo) {
			return errors.New("notifier config ses_canary_to must be one plain mailbox address")
		}
	}
	return nil
}

func validSESUsername(value string) bool {
	if value == "" || len(value) > maxSESIdentityBytes || strings.TrimSpace(value) != value {
		return false
	}
	for i := range len(value) {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func validMailbox(value string) bool {
	if value == "" || len(value) > maxSESIdentityBytes || strings.TrimSpace(value) != value {
		return false
	}
	address, err := mail.ParseAddress(value)
	if err != nil {
		return false
	}
	// Display names and parser normalization make it unclear which exact value
	// is used in the SMTP envelope versus the message header. Accept only one
	// plain mailbox whose parsed form is byte-for-byte the configured value.
	return address.Name == "" && address.Address == value
}

func validBotToken(token string) bool {
	if token == "" || len(token) > 256 || strings.TrimSpace(token) != token {
		return false
	}
	colon := strings.IndexByte(token, ':')
	if colon <= 0 || colon == len(token)-1 || colon != strings.LastIndexByte(token, ':') {
		return false
	}
	if token[0] == '0' {
		return false
	}
	for i := 0; i < colon; i++ {
		if token[i] < '0' || token[i] > '9' {
			return false
		}
	}
	for i := colon + 1; i < len(token); i++ {
		b := token[i]
		if (b >= 'a' && b <= 'z') ||
			(b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') ||
			b == '_' || b == '-' {
			continue
		}
		return false
	}
	return true
}

func validTelegramAPIBase(raw string) bool {
	if raw == "" || raw == "https://api.telegram.org" {
		return true
	}
	return telegramAPIIsLoopback(raw)
}

func telegramAPIIsLoopback(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil ||
		parsed.Scheme != "http" && parsed.Scheme != "https" ||
		parsed.User != nil ||
		parsed.Host == "" ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" ||
		parsed.Path != "" {
		return false
	}
	return isLoopbackHost(parsed.Hostname())
}

func validSESAddr(addr string, allowLoopback bool) bool {
	host, rawPort, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65_535 {
		return false
	}
	if isLoopbackHost(host) {
		return allowLoopback
	}
	if port != 25 && port != 587 && port != 2587 {
		return false
	}
	return validSESEndpoint(host)
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validSESEndpoint(host string) bool {
	host = strings.ToLower(host)
	for _, prefix := range []string{"email-smtp.", "email-smtp-fips."} {
		if !strings.HasPrefix(host, prefix) {
			continue
		}
		for _, suffix := range []string{".amazonaws.com", ".amazonaws.com.cn", ".api.aws"} {
			if strings.HasSuffix(host, suffix) {
				region := strings.TrimSuffix(strings.TrimPrefix(host, prefix), suffix)
				return validDNSLabel(region)
			}
		}
	}
	return false
}

func validDNSLabel(label string) bool {
	if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := range len(label) {
		b := label[i]
		if b < 'a' || b > 'z' {
			if b < '0' || b > '9' {
				if b != '-' {
					return false
				}
			}
		}
	}
	return true
}
