// browser_v3 registration: runs the GuJumpgate-technique Playwright harness
// (services/codex_register/reg_v3.py) as a subprocess. This is the flow that
// successfully registers ChatGPT accounts — it uses a headless Chrome with
// ignore_default_args ["--enable-automation"] + stealth scripts + proper OTP
// time-filtering from a real Hotmail inbox.
//
// Proxy configuration is read from the admin-configured egress profile
// (configured in the frontend at /admin/egress-profiles). The proxy hostname
// is resolved at runtime to bypass fake-IP DNS (Clash/mihomo interference).
// Both HTTP and SOCKS5 proxy types are supported.
//
// On success the script prints "__CODEX_ACCOUNT__ {json}" which we parse and
// UpsertAccount — the auto-add-to-pool path.
package pipeline

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/registration/provider/proxy"
	"codex-account-pool/internal/storage"
)

// v3SMSCountries are cheap-first SMS countries that work for OpenAI on hero-sms. The proxy
// region is pinned to the chosen country so the residential exit IP and the phone number
// agree during add-phone verification. Keep in sync with
// services/codex_register/phone_verify.py COUNTRY_CFG.
//
// NOTE: the default recommended order is now BR > CO > PL (driven by the sms_preferred_countries
// setting); this list is only the fallback when no explicit/preferred country is available.
var v3SMSCountries = []string{"BR", "CO", "PL", "PH", "ID", "VN", "MY", "ZA"}

var v3CountryCounter uint64

// pickV3Country returns the SMS/region country for one registration. An explicit
// req.Country wins; otherwise it round-robins the cheap list so concurrent registrations
// spread across countries instead of draining a single country's number pool. "Rand" (the
// raw cliproxy region token) is treated as unset so we replace it with a real country.
func pickV3Country(reqCountry string) string {
	c := strings.ToUpper(strings.TrimSpace(reqCountry))
	if c != "" && c != "RAND" {
		return c
	}
	i := atomic.AddUint64(&v3CountryCounter, 1) - 1
	return v3SMSCountries[i%uint64(len(v3SMSCountries))]
}

// resolveProxyHost resolves the proxy hostname to an IP, bypassing fake-IP
// DNS (e.g. Clash/mihomo returning 198.18.0.x). Returns the original host
// if resolution fails.
func resolveProxyHost(host string) string {
	if net.ParseIP(host) != nil {
		return host // already an IP
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return host
	}
	for _, ip := range ips {
		s := ip.String()
		if !strings.HasPrefix(s, "198.18.") && !strings.HasPrefix(s, "198.19.") {
			return s
		}
	}
	return ips[0].String()
}

// parseBrowserProxy parses an egress endpoint (http://user:pass@host:port or
// socks5h://user:pass@host:port) into the component fields the v3 harness expects.
func parseBrowserProxy(endpoint string) (serverURL, user, pass string, err error) {
	ep := strings.TrimSpace(endpoint)
	if proxy.IsCliproxy(ep) {
		ep = proxy.RotateSID(ep)
	}
	u, e := url.Parse(ep)
	if e != nil || u.Host == "" {
		return "", "", "", fmt.Errorf("bad proxy URL: %q", endpoint)
	}
	host, port, _ := net.SplitHostPort(u.Host)
	if port == "" {
		port = "3010"
	}
	hostIP := resolveProxyHost(host)
	user = u.User.Username()
	pass, _ = u.User.Password()
	// The v3 harness uses HTTP proxy (Playwright's "server" + "username" + "password").
	// For socks5h endpoints, use the socks5h URL as server (Playwright supports it via
	// proxy.server = "socks5://..." since v1.40+).
	scheme := u.Scheme
	if scheme == "socks5h" {
		serverURL = fmt.Sprintf("socks5://%s:%s", hostIP, port)
	} else {
		serverURL = fmt.Sprintf("http://%s:%s", hostIP, port)
	}
	return serverURL, user, pass, nil
}

// getEmailForRegistration builds a plus-addressed Hotmail email from the
// operator-configured base email and OTP reader URL (provider_settings).
func (p *Pipeline) getEmailForRegistration(ctx context.Context) (email, otpURL, otpToken string, err error) {
	cfg, configErr := p.providerConfig(ctx, "email", "hotmail_otp")
	err = configErr
	if err != nil {
		// Fall back to env vars (for testing / non-DB setups).
		base := os.Getenv("HOTMAIL_BASE_EMAIL")
		otp := os.Getenv("HOTMAIL_OTP_URL")
		token := os.Getenv("HOTMAIL_OTP_TOKEN")
		if base == "" || otp == "" || token == "" {
			return "", "", "", fmt.Errorf("hotmail_otp email provider not configured")
		}
		return base, otp, token, nil
	}
	base, _ := cfg["base_email"].(string)
	otpURL, _ = cfg["otp_url"].(string)
	otpToken, _ = cfg["auth_token"].(string)
	if base == "" || otpURL == "" || otpToken == "" {
		return "", "", "", fmt.Errorf("hotmail_otp config missing required credentials")
	}
	return base, otpURL, otpToken, nil
}

func (p *Pipeline) browserV3RegisterOne(ctx context.Context, req RegisterRequest) (*storage.Account, error) {
	// Resolve proxy from the egress profile
	egress, err := p.store.GetEgressProfile(ctx, req.EgressID)
	if err != nil {
		return nil, fmt.Errorf("browser_v3: egress not found: %w", err)
	}
	// Pin the residential exit to the country chosen by the hourly market table. This does
	// not reserve a number: browser_v3 purchases only if OAuth actually exposes add_phone.
	country := strings.ToUpper(strings.TrimSpace(req.Country))
	selectedPrice := 0.0
	if country == "" || country == "RAND" {
		if selected, ok := p.bestSMSCountry(ctx, "herosms"); ok && selected.CountryISO != "" {
			country = selected.CountryISO
			selectedPrice = selected.Price
		} else {
			country = pickV3Country("")
		}
	}
	if proxy.IsCliproxy(egress.Endpoint) {
		egress.Endpoint = proxy.WithRegion(egress.Endpoint, country)
	}
	server, user, pass, err := parseBrowserProxy(egress.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("browser_v3: bad proxy: %w", err)
	}

	// Use the connector-neutral loopback relay when a mailbox provider was
	// selected. Legacy authenticated plus-addressing remains compatible.
	email, otpURL, otpToken := "", "", ""
	var relay *mailboxRelay
	if strings.TrimSpace(req.MailboxProvider) != "" {
		relay, err = p.prepareMailboxRelay(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("browser_v3: mailbox relay: %w", err)
		}
		defer relay.Close(ctx)
		email, otpURL, otpToken = relay.Email, relay.URL, relay.Token
	} else {
		baseEmail, legacyOTPURL, legacyOTPToken, emailErr := p.getEmailForRegistration(ctx)
		if emailErr != nil {
			return nil, fmt.Errorf("browser_v3: email: %w", emailErr)
		}
		tag := randomHex(6)
		email = strings.Replace(baseEmail, "@", "+"+tag+"@", 1)
		otpURL, otpToken = legacyOTPURL, legacyOTPToken
	}

	// Resolve paths
	python := firstEnv("python3", "CODEX_REG_PYTHON")
	script := firstEnv("services/codex_register/reg_v3.py", "CODEX_REG_V3_SCRIPT")
	chrome := firstEnv("", "CODEX_REG_CHROME", "CHROME_PATH")
	headless := "1"
	if os.Getenv("REG_HEADLESS") == "0" {
		headless = "0"
	}

	cctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, python, "-u", script)
	minPrice, maxPrice := smsPriceBounds(ctx, p.store)
	cmd.Env = append(registrarBaseEnv(),
		"REG_PROXY_SERVER="+server,
		"REG_PROXY_USER="+user,
		"REG_PROXY_PASS="+pass,
		"REG_EMAIL="+email,
		"REG_OTP_URL="+otpURL,
		"REG_OTP_TOKEN="+otpToken,
		"REG_CHROME="+chrome,
		"REG_HEADLESS="+headless,
		// SMS country must match the proxy region so the IP and phone agree; reg_v3.py
		// only draws a hero-sms number if OpenAI demands add-phone during OAuth.
		"REG_SMS_COUNTRY="+country,
		"REG_SMS_MIN_PRICE="+strconv.FormatFloat(minPrice, 'f', -1, 64),
		"REG_SMS_MAX_PRICE="+strconv.FormatFloat(maxPrice, 'f', -1, 64),
	)
	if hk := p.herosmsKey(ctx); hk != "" {
		cmd.Env = append(cmd.Env, "HEROSMS_KEY="+hk)
	}
	out, err := p.runRegistrarCommand(cctx, cmd)
	if usedCountry, used := browserV3SMSCountry(out, country); used {
		if !strings.EqualFold(usedCountry, country) {
			selectedPrice = 0
		}
		p.recordSMSCountrySelection(ctx, req, "herosms", usedCountry, selectedPrice)
	}
	if err != nil && len(out) == 0 {
		return nil, errors.New("browser_v3: harness process failed")
	}

	// Parse the "__CODEX_ACCOUNT__ {json}" marker line.
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		i := strings.Index(line, "__CODEX_ACCOUNT__ ")
		if i < 0 {
			continue
		}
		var a codexAccountLine
		if json.Unmarshal([]byte(line[i+len("__CODEX_ACCOUNT__ "):]), &a) == nil && strings.TrimSpace(a.AccessToken) != "" {
			return p.persistVerifiedRegistration(ctx, req, registrationCredential{
				LabelPrefix:       "browser-",
				Email:             a.Email,
				UpstreamAccountID: a.AccountID,
				ChatGPTUserID:     a.UserID,
				AccessToken:       a.AccessToken,
				RefreshToken:      a.RefreshToken,
				IDToken:           a.IDToken,
				SessionToken:      a.SessionToken,
				LoginPassword:     a.LoginPassword,
			})
		}
	}
	return nil, fmt.Errorf("browser_v3: no account produced")
}

func browserV3SMSCountry(output []byte, fallback string) (string, bool) {
	text := string(output)
	if !strings.Contains(text, "add-phone step detected") {
		return "", false
	}
	const marker = "virtual number allocated country="
	if index := strings.LastIndex(text, marker); index >= 0 {
		value := text[index+len(marker):]
		if len(value) >= 2 {
			country := strings.ToUpper(value[:2])
			if country[0] >= 'A' && country[0] <= 'Z' && country[1] >= 'A' && country[1] <= 'Z' {
				return country, true
			}
		}
	}
	return strings.ToUpper(strings.TrimSpace(fallback)), true
}

func randomHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}
