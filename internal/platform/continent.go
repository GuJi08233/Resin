package platform

import (
	"fmt"
	"strings"
)

// Standard two-letter lowercase continent codes (matching GeoNames / MaxMind GeoLite2).
const (
	ContinentAsia         = "as"
	ContinentEurope       = "eu"
	ContinentNorthAmerica = "na"
	ContinentSouthAmerica = "sa"
	ContinentOceania      = "oc"
	ContinentAfrica       = "af"
	ContinentAntarctica   = "an"
)

// AllContinentCodes lists every supported continent code in canonical display order.
var AllContinentCodes = []string{
	ContinentAsia,
	ContinentEurope,
	ContinentNorthAmerica,
	ContinentSouthAmerica,
	ContinentOceania,
	ContinentAfrica,
	ContinentAntarctica,
}

var validContinentSet = map[string]bool{
	ContinentAsia:         true,
	ContinentEurope:       true,
	ContinentNorthAmerica: true,
	ContinentSouthAmerica: true,
	ContinentOceania:      true,
	ContinentAfrica:       true,
	ContinentAntarctica:   true,
}

// ValidContinentCode reports whether code is a recognized lowercase continent code.
func ValidContinentCode(code string) bool {
	return validContinentSet[code]
}

// ParseContinentFilterEntry parses a single continent filter entry, accepting
// case-insensitive input and an optional leading "!" negation prefix.
func ParseContinentFilterEntry(entry string) (code string, exclude bool, err error) {
	value := strings.TrimSpace(entry)
	if value == "" {
		return "", false, fmt.Errorf("must not be empty")
	}
	if value[0] == '!' {
		exclude = true
		value = strings.TrimSpace(value[1:])
	}
	value = strings.ToLower(value)
	if !validContinentSet[value] {
		return "", false, fmt.Errorf(
			"must be a valid continent code (as, eu, na, sa, oc, af, an) or negation (e.g. !eu)",
		)
	}
	return value, exclude, nil
}

// NormalizeContinentFilters validates, canonicalizes (lowercase, trimmed),
// and deduplicates a list of continent filter rules.
func NormalizeContinentFilters(filters []string) ([]string, error) {
	if len(filters) == 0 {
		return []string{}, nil
	}
	out := make([]string, 0, len(filters))
	seen := make(map[string]bool, len(filters))
	for i, raw := range filters {
		code, exclude, err := ParseContinentFilterEntry(raw)
		if err != nil {
			return nil, fmt.Errorf("continent_filters[%d]: %s", i, err.Error())
		}
		canonical := code
		if exclude {
			canonical = "!" + code
		}
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		out = append(out, canonical)
	}
	return out, nil
}

// ValidateContinentFilters validates a list of continent filter rules.
func ValidateContinentFilters(filters []string) error {
	_, err := NormalizeContinentFilters(filters)
	return err
}

// ContinentForCountry resolves an ISO 3166-1 alpha-2 country code (case-insensitive)
// to its lowercase continent code. Returns "" for unknown or empty inputs.
func ContinentForCountry(countryCode string) string {
	code := strings.ToLower(strings.TrimSpace(countryCode))
	if code == "" {
		return ""
	}
	return countryToContinent[code]
}

// MatchContinentFilter applies include/exclude continent filters against a
// node's ISO 3166-1 alpha-2 country code.
// Positive entries (eu) build an include set; negative entries (!eu) build an exclude set.
// Unknown countries/continents never match when continent filters are configured.
func MatchContinentFilter(countryCode string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	continent := ContinentForCountry(countryCode)
	return MatchRegionFilter(continent, filters)
}

// countryToContinent maps every ISO 3166-1 alpha-2 country/territory code
// (plus common GeoIP codes such as xk and uk) to its standard continent code
// following GeoNames / MaxMind GeoLite2 conventions.
var countryToContinent = map[string]string{
	// Asia (as)
	"ae": ContinentAsia, "af": ContinentAsia, "am": ContinentAsia, "az": ContinentAsia,
	"bd": ContinentAsia, "bh": ContinentAsia, "bn": ContinentAsia, "bt": ContinentAsia,
	"cc": ContinentAsia, "cn": ContinentAsia, "cx": ContinentAsia, "ge": ContinentAsia,
	"hk": ContinentAsia, "id": ContinentAsia, "il": ContinentAsia, "in": ContinentAsia,
	"io": ContinentAsia, "iq": ContinentAsia, "ir": ContinentAsia, "jo": ContinentAsia,
	"jp": ContinentAsia, "kg": ContinentAsia, "kh": ContinentAsia, "kp": ContinentAsia,
	"kr": ContinentAsia, "kw": ContinentAsia, "kz": ContinentAsia, "la": ContinentAsia,
	"lb": ContinentAsia, "lk": ContinentAsia, "mm": ContinentAsia, "mn": ContinentAsia,
	"mo": ContinentAsia, "mv": ContinentAsia, "my": ContinentAsia, "np": ContinentAsia,
	"om": ContinentAsia, "ph": ContinentAsia, "pk": ContinentAsia, "ps": ContinentAsia,
	"qa": ContinentAsia, "sa": ContinentAsia, "sg": ContinentAsia, "sy": ContinentAsia,
	"th": ContinentAsia, "tj": ContinentAsia, "tl": ContinentAsia, "tm": ContinentAsia,
	"tr": ContinentAsia, "tw": ContinentAsia, "uz": ContinentAsia, "vn": ContinentAsia,
	"ye": ContinentAsia,

	// Europe (eu)
	"ad": ContinentEurope, "al": ContinentEurope, "at": ContinentEurope, "ax": ContinentEurope,
	"ba": ContinentEurope, "be": ContinentEurope, "bg": ContinentEurope, "by": ContinentEurope,
	"ch": ContinentEurope, "cy": ContinentEurope, "cz": ContinentEurope, "de": ContinentEurope,
	"dk": ContinentEurope, "ee": ContinentEurope, "es": ContinentEurope, "eu": ContinentEurope,
	"fi": ContinentEurope, "fo": ContinentEurope, "fr": ContinentEurope, "gb": ContinentEurope,
	"gg": ContinentEurope, "gi": ContinentEurope, "gr": ContinentEurope, "hr": ContinentEurope,
	"hu": ContinentEurope, "ie": ContinentEurope, "im": ContinentEurope, "is": ContinentEurope,
	"it": ContinentEurope, "je": ContinentEurope, "li": ContinentEurope, "lt": ContinentEurope,
	"lu": ContinentEurope, "lv": ContinentEurope, "mc": ContinentEurope, "md": ContinentEurope,
	"me": ContinentEurope, "mk": ContinentEurope, "mt": ContinentEurope, "nl": ContinentEurope,
	"no": ContinentEurope, "pl": ContinentEurope, "pt": ContinentEurope, "ro": ContinentEurope,
	"rs": ContinentEurope, "ru": ContinentEurope, "se": ContinentEurope, "si": ContinentEurope,
	"sj": ContinentEurope, "sk": ContinentEurope, "sm": ContinentEurope, "ua": ContinentEurope,
	"uk": ContinentEurope, "va": ContinentEurope, "xk": ContinentEurope,

	// North America (na)
	"ag": ContinentNorthAmerica, "ai": ContinentNorthAmerica, "an": ContinentNorthAmerica, "aw": ContinentNorthAmerica,
	"bb": ContinentNorthAmerica, "bl": ContinentNorthAmerica, "bm": ContinentNorthAmerica, "bq": ContinentNorthAmerica,
	"bs": ContinentNorthAmerica, "bz": ContinentNorthAmerica, "ca": ContinentNorthAmerica, "cr": ContinentNorthAmerica,
	"cu": ContinentNorthAmerica, "cw": ContinentNorthAmerica, "dm": ContinentNorthAmerica, "do": ContinentNorthAmerica,
	"gd": ContinentNorthAmerica, "gl": ContinentNorthAmerica, "gp": ContinentNorthAmerica, "gt": ContinentNorthAmerica,
	"hn": ContinentNorthAmerica, "ht": ContinentNorthAmerica, "jm": ContinentNorthAmerica, "kn": ContinentNorthAmerica,
	"ky": ContinentNorthAmerica, "lc": ContinentNorthAmerica, "mf": ContinentNorthAmerica, "mq": ContinentNorthAmerica,
	"ms": ContinentNorthAmerica, "mx": ContinentNorthAmerica, "ni": ContinentNorthAmerica, "pa": ContinentNorthAmerica,
	"pm": ContinentNorthAmerica, "pr": ContinentNorthAmerica, "sv": ContinentNorthAmerica, "sx": ContinentNorthAmerica,
	"tc": ContinentNorthAmerica, "tt": ContinentNorthAmerica, "us": ContinentNorthAmerica, "vc": ContinentNorthAmerica,
	"vg": ContinentNorthAmerica, "vi": ContinentNorthAmerica,

	// South America (sa)
	"ar": ContinentSouthAmerica, "bo": ContinentSouthAmerica, "br": ContinentSouthAmerica, "cl": ContinentSouthAmerica,
	"co": ContinentSouthAmerica, "ec": ContinentSouthAmerica, "fk": ContinentSouthAmerica, "gf": ContinentSouthAmerica,
	"gy": ContinentSouthAmerica, "pe": ContinentSouthAmerica, "py": ContinentSouthAmerica, "sr": ContinentSouthAmerica,
	"uy": ContinentSouthAmerica, "ve": ContinentSouthAmerica,

	// Oceania (oc)
	"as": ContinentOceania, "au": ContinentOceania, "ck": ContinentOceania, "fj": ContinentOceania,
	"fm": ContinentOceania, "gu": ContinentOceania, "ki": ContinentOceania, "mh": ContinentOceania,
	"mp": ContinentOceania, "nc": ContinentOceania, "nf": ContinentOceania, "nr": ContinentOceania,
	"nu": ContinentOceania, "nz": ContinentOceania, "pf": ContinentOceania, "pg": ContinentOceania,
	"pn": ContinentOceania, "pw": ContinentOceania, "sb": ContinentOceania, "tk": ContinentOceania,
	"to": ContinentOceania, "tv": ContinentOceania, "um": ContinentOceania, "vu": ContinentOceania,
	"wf": ContinentOceania, "ws": ContinentOceania,

	// Africa (af)
	"ao": ContinentAfrica, "bf": ContinentAfrica, "bi": ContinentAfrica, "bj": ContinentAfrica,
	"bw": ContinentAfrica, "cd": ContinentAfrica, "cf": ContinentAfrica, "cg": ContinentAfrica,
	"ci": ContinentAfrica, "cm": ContinentAfrica, "cv": ContinentAfrica, "dj": ContinentAfrica,
	"dz": ContinentAfrica, "eg": ContinentAfrica, "eh": ContinentAfrica, "er": ContinentAfrica,
	"et": ContinentAfrica, "ga": ContinentAfrica, "gh": ContinentAfrica, "gm": ContinentAfrica,
	"gn": ContinentAfrica, "gq": ContinentAfrica, "gw": ContinentAfrica, "ke": ContinentAfrica,
	"km": ContinentAfrica, "lr": ContinentAfrica, "ls": ContinentAfrica, "ly": ContinentAfrica,
	"ma": ContinentAfrica, "mg": ContinentAfrica, "ml": ContinentAfrica, "mr": ContinentAfrica,
	"mu": ContinentAfrica, "mw": ContinentAfrica, "mz": ContinentAfrica, "na": ContinentAfrica,
	"ne": ContinentAfrica, "ng": ContinentAfrica, "re": ContinentAfrica, "rw": ContinentAfrica,
	"sc": ContinentAfrica, "sd": ContinentAfrica, "sh": ContinentAfrica, "sl": ContinentAfrica,
	"sn": ContinentAfrica, "so": ContinentAfrica, "ss": ContinentAfrica, "st": ContinentAfrica,
	"sz": ContinentAfrica, "td": ContinentAfrica, "tg": ContinentAfrica, "tn": ContinentAfrica,
	"tz": ContinentAfrica, "ug": ContinentAfrica, "yt": ContinentAfrica, "za": ContinentAfrica,
	"zm": ContinentAfrica, "zw": ContinentAfrica,

	// Antarctica (an)
	"aq": ContinentAntarctica, "bv": ContinentAntarctica, "gs": ContinentAntarctica,
	"hm": ContinentAntarctica, "tf": ContinentAntarctica,
}
