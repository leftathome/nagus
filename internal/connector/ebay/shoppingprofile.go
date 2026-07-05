package ebay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Shopping API hosts for GetUserProfile (a legacy but app-credential-friendly
// endpoint that exposes a seller's registration date and feedback history --
// data NOT available in the Browse item_summary response).
const (
	DefaultShoppingBaseURL        = "https://open.api.ebay.com/shopping"
	DefaultShoppingSandboxBaseURL = "https://open.api.sandbox.ebay.com/shopping"
	shoppingAPIVersion            = "1157"
)

// ShoppingProfileSource implements SellerProfileSource via the eBay Shopping API
// GetUserProfile call. It derives ONLY coarse, non-identifying inputs (account
// age in days, a recent-feedback count) and passes the username transiently as
// the UserID query argument -- it never stores or logs it.
//
// NOTE: the exact GetUserProfile response field mapping below is written to the
// documented shape but has NOT been validated against a live eBay response in
// this codebase. Validate it against the Sandbox (Config.Sandbox + the
// ebayintegration test) and confirm during live keyset validation (nagus-hm0)
// before enabling in production. The feature is OFF by default.
type ShoppingProfileSource struct {
	AppID      string       // eBay App ID (client id); Shopping API authenticates by appid
	BaseURL    string       // defaults to DefaultShoppingBaseURL
	SiteID     string       // eBay site id; "0" = US (default)
	HTTPClient *http.Client // defaults to http.DefaultClient
	Now        func() time.Time
}

// NewShoppingProfileSource builds a source for the given App ID, pointed at the
// production or sandbox Shopping host.
func NewShoppingProfileSource(appID string, sandbox bool, hc *http.Client) *ShoppingProfileSource {
	base := DefaultShoppingBaseURL
	if sandbox {
		base = DefaultShoppingSandboxBaseURL
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	return &ShoppingProfileSource{AppID: appID, BaseURL: base, SiteID: "0", HTTPClient: hc, Now: time.Now}
}

// shoppingUserProfileResponse is the subset of GetUserProfile we consume.
type shoppingUserProfileResponse struct {
	Ack  string `json:"Ack"`
	User *struct {
		RegistrationDate string `json:"RegistrationDate"`
		FeedbackHistory  *struct {
			PositiveFeedbackPeriodArray *struct {
				FeedbackPeriod []struct {
					PeriodInDays int `json:"PeriodInDays"`
					Count        int `json:"Count"`
				} `json:"FeedbackPeriod"`
			} `json:"PositiveFeedbackPeriodArray"`
		} `json:"FeedbackHistory"`
	} `json:"User"`
}

func (s *ShoppingProfileSource) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Profile calls GetUserProfile for username and returns coarse inputs. The
// username is used only to build the request and is never retained. token is a
// valid eBay OAuth access token, presented in the X-EBAY-API-IAF-TOKEN header --
// GetUserProfile rejects appid-only auth with error 1.33 (verified live against
// the sandbox 2026-07).
func (s *ShoppingProfileSource) Profile(ctx context.Context, username, token string) (SellerProfile, bool, error) {
	base := s.BaseURL
	if base == "" {
		base = DefaultShoppingBaseURL
	}
	site := s.SiteID
	if site == "" {
		site = "0"
	}
	q := url.Values{}
	q.Set("callname", "GetUserProfile")
	q.Set("responseencoding", "JSON")
	q.Set("siteid", site)
	q.Set("version", shoppingAPIVersion)
	q.Set("UserID", username)
	q.Set("IncludeSelector", "Details,FeedbackHistory")
	reqURL := strings.TrimRight(base, "/") + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return SellerProfile{}, false, fmt.Errorf("build profile request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-EBAY-API-IAF-TOKEN", token)
	req.Header.Set("X-EBAY-API-APP-ID", s.AppID)

	hc := s.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return SellerProfile{}, false, fmt.Errorf("profile request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return SellerProfile{}, false, fmt.Errorf("read profile response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SellerProfile{}, false, fmt.Errorf("profile endpoint status %d: %s", resp.StatusCode, truncate(data, 200))
	}

	var parsed shoppingUserProfileResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return SellerProfile{}, false, fmt.Errorf("decode profile response: %w", err)
	}
	if !strings.EqualFold(parsed.Ack, "Success") || parsed.User == nil {
		return SellerProfile{}, false, nil
	}
	return parseSellerProfile(parsed, s.now()), true, nil
}

// parseSellerProfile maps a decoded GetUserProfile response into the coarse
// inputs, marking each field resolved only when actually present.
func parseSellerProfile(r shoppingUserProfileResponse, now time.Time) SellerProfile {
	var p SellerProfile
	if reg, err := parseEbayTime(r.User.RegistrationDate); err == nil {
		days := int(now.Sub(reg).Hours() / 24)
		if days >= 0 {
			p.AccountAgeDays = days
			p.HasAccountAge = true
		}
	}
	// Recent-sales proxy: positive feedback received in the last 30 days.
	if fh := r.User.FeedbackHistory; fh != nil && fh.PositiveFeedbackPeriodArray != nil {
		for _, per := range fh.PositiveFeedbackPeriodArray.FeedbackPeriod {
			if per.PeriodInDays == 30 {
				p.RecentSales = per.Count
				p.HasRecentSales = true
				break
			}
		}
	}
	return p
}

// parseEbayTime parses eBay's ISO-8601 timestamps (e.g. "2005-03-14T08:03:14.000Z").
func parseEbayTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	for _, layout := range []string{"2006-01-02T15:04:05.000Z07:00", time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized eBay time %q", s)
}
