// Package ipinfo parses the public IP reputation payload returned by IPPure
// (https://my.ippure.com/v1/info). The endpoint reports the caller's own IP,
// so Resin must reach it through the node's outbound to learn that node's
// egress profile.
package ipinfo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Risk levels derived from IPPure fraudScore. IPPure's own UI groups scores
// with the boundaries [0, 15, 25, 40, 50, 70, 100].
const (
	RiskLevelVeryClean  = "very_clean"
	RiskLevelClean      = "clean"
	RiskLevelNeutral    = "neutral"
	RiskLevelLightRisk  = "light_risk"
	RiskLevelMediumRisk = "medium_risk"
	RiskLevelHighRisk   = "high_risk"
)

// Info is the normalized node egress profile.
type Info struct {
	IP             string `json:"ip"`
	ASN            int    `json:"asn"`
	ASOrganization string `json:"as_organization"`
	Country        string `json:"country"`
	CountryCode    string `json:"country_code"`
	Region         string `json:"region"`
	RegionCode     string `json:"region_code"`
	City           string `json:"city"`
	Timezone       string `json:"timezone"`
	Latitude       string `json:"latitude"`
	Longitude      string `json:"longitude"`
	PostalCode     string `json:"postal_code"`
	FraudScore     int    `json:"fraud_score"`
	RiskLevel      string `json:"risk_level"`
	// IsResidential reports whether the IP belongs to an ISP/residential pool.
	IsResidential bool `json:"is_residential"`
	// IsBroadcast reports whether the IP is a broadcast (non-native) address.
	IsBroadcast bool      `json:"is_broadcast"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// RiskLevel maps a 0-100 fraud score to a coarse level.
func RiskLevel(score int) string {
	switch {
	case score < 15:
		return RiskLevelVeryClean
	case score < 25:
		return RiskLevelClean
	case score < 40:
		return RiskLevelNeutral
	case score < 50:
		return RiskLevelLightRisk
	case score < 70:
		return RiskLevelMediumRisk
	default:
		return RiskLevelHighRisk
	}
}

type rawInfo struct {
	IP             string     `json:"ip"`
	ASN            flexInt    `json:"asn"`
	ASOrganization string     `json:"asOrganization"`
	Country        string     `json:"country"`
	CountryCode    string     `json:"countryCode"`
	Region         string     `json:"region"`
	RegionCode     string     `json:"regionCode"`
	City           string     `json:"city"`
	Timezone       string     `json:"timezone"`
	Latitude       flexString `json:"latitude"`
	Longitude      flexString `json:"longitude"`
	PostalCode     flexString `json:"postalCode"`
	FraudScore     flexInt    `json:"fraudScore"`
	IsResidential  bool       `json:"isResidential"`
	IsBroadcast    bool       `json:"isBroadcast"`
}

// Parse decodes one IPPure /v1/info response body.
func Parse(body []byte) (*Info, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, fmt.Errorf("ipinfo: empty response body")
	}

	var raw rawInfo
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("ipinfo: decode response: %w", err)
	}

	ip := strings.TrimSpace(raw.IP)
	if ip == "" {
		return nil, fmt.Errorf("ipinfo: response has no ip field")
	}

	score := int(raw.FraudScore)
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}

	return &Info{
		IP:             ip,
		ASN:            int(raw.ASN),
		ASOrganization: strings.TrimSpace(raw.ASOrganization),
		Country:        strings.TrimSpace(raw.Country),
		CountryCode:    strings.ToLower(strings.TrimSpace(raw.CountryCode)),
		Region:         strings.TrimSpace(raw.Region),
		RegionCode:     strings.TrimSpace(raw.RegionCode),
		City:           strings.TrimSpace(raw.City),
		Timezone:       strings.TrimSpace(raw.Timezone),
		Latitude:       strings.TrimSpace(string(raw.Latitude)),
		Longitude:      strings.TrimSpace(string(raw.Longitude)),
		PostalCode:     strings.TrimSpace(string(raw.PostalCode)),
		FraudScore:     score,
		RiskLevel:      RiskLevel(score),
		IsResidential:  raw.IsResidential,
		IsBroadcast:    raw.IsBroadcast,
	}, nil
}

// flexInt accepts either a JSON number or a numeric string.
type flexInt int

func (v *flexInt) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "null" {
		*v = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	if s == "" {
		*v = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return fmt.Errorf("ipinfo: invalid integer %q", s)
		}
		n = int(f)
	}
	*v = flexInt(n)
	return nil
}

// flexString accepts either a JSON string or a bare JSON number.
type flexString string

func (v *flexString) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "null" {
		*v = ""
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var out string
		if err := json.Unmarshal(data, &out); err != nil {
			return err
		}
		*v = flexString(out)
		return nil
	}
	*v = flexString(s)
	return nil
}
