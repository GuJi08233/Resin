package platform

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/Resinat/Resin/internal/ipinfo"
	"github.com/Resinat/Resin/internal/model"
)

// IPUsage classifies an egress IP by address pool.
type IPUsage string

const (
	IPUsageAny         IPUsage = "any"
	IPUsageResidential IPUsage = "residential"
	IPUsageHosting     IPUsage = "hosting"
)

// IPNative classifies an egress IP as native or broadcast (non-native).
type IPNative string

const (
	IPNativeAny       IPNative = "any"
	IPNativeNative    IPNative = "native"
	IPNativeBroadcast IPNative = "broadcast"
)

// IPInfoFilter constrains platform routability by the node's IPPure profile.
// A zero filter (all fields unset) is inactive. When active, nodes without a
// stored profile never match, mirroring how region filters treat unknown
// regions.
type IPInfoFilter struct {
	MinFraudScore *int
	MaxFraudScore *int
	IPType        IPUsage
	Native        IPNative
	ASNIncludes   []uint32
	ASNExcludes   []uint32
}

// Active reports whether the filter constrains anything.
func (f IPInfoFilter) Active() bool {
	return f.MinFraudScore != nil ||
		f.MaxFraudScore != nil ||
		(f.IPType != "" && f.IPType != IPUsageAny) ||
		(f.Native != "" && f.Native != IPNativeAny) ||
		len(f.ASNIncludes) > 0 ||
		len(f.ASNExcludes) > 0
}

// Match reports whether info satisfies the filter.
func (f IPInfoFilter) Match(info *ipinfo.Info) bool {
	if !f.Active() {
		return true
	}
	if info == nil {
		return false
	}
	if f.MinFraudScore != nil && info.FraudScore < *f.MinFraudScore {
		return false
	}
	if f.MaxFraudScore != nil && info.FraudScore > *f.MaxFraudScore {
		return false
	}

	switch f.IPType {
	case IPUsageResidential:
		if !info.IsResidential {
			return false
		}
	case IPUsageHosting:
		if info.IsResidential {
			return false
		}
	}

	switch f.Native {
	case IPNativeNative:
		if info.IsBroadcast {
			return false
		}
	case IPNativeBroadcast:
		if !info.IsBroadcast {
			return false
		}
	}

	asn := uint32(info.ASN)
	if len(f.ASNIncludes) > 0 && !slices.Contains(f.ASNIncludes, asn) {
		return false
	}
	if len(f.ASNExcludes) > 0 && slices.Contains(f.ASNExcludes, asn) {
		return false
	}
	return true
}

// NormalizeIPInfoFilter validates and canonicalizes a persisted filter.
// Empty ip_type/native values and "any" collapse to "" (wildcard). ASN entries
// accept an optional "AS" prefix and an optional "!" negation prefix.
func NormalizeIPInfoFilter(raw model.PlatformIPInfoFilter) (model.PlatformIPInfoFilter, error) {
	out := model.PlatformIPInfoFilter{
		IPType: "",
		Native: "",
		ASNs:   []string{},
	}

	if raw.MinFraudScore != nil {
		score := *raw.MinFraudScore
		if score < 0 || score > 100 {
			return model.PlatformIPInfoFilter{}, fmt.Errorf("ipinfo_filters.min_fraud_score: must be between 0 and 100")
		}
		out.MinFraudScore = &score
	}
	if raw.MaxFraudScore != nil {
		score := *raw.MaxFraudScore
		if score < 0 || score > 100 {
			return model.PlatformIPInfoFilter{}, fmt.Errorf("ipinfo_filters.max_fraud_score: must be between 0 and 100")
		}
		out.MaxFraudScore = &score
	}
	if out.MinFraudScore != nil && out.MaxFraudScore != nil && *out.MinFraudScore > *out.MaxFraudScore {
		return model.PlatformIPInfoFilter{}, fmt.Errorf("ipinfo_filters: min_fraud_score must be <= max_fraud_score")
	}

	ipType, err := normalizeIPUsage(raw.IPType)
	if err != nil {
		return model.PlatformIPInfoFilter{}, err
	}
	out.IPType = string(ipType)

	native, err := normalizeIPNative(raw.Native)
	if err != nil {
		return model.PlatformIPInfoFilter{}, err
	}
	out.Native = string(native)

	seenInclude := make(map[uint32]bool, len(raw.ASNs))
	seenExclude := make(map[uint32]bool, len(raw.ASNs))
	for i, entry := range raw.ASNs {
		asn, exclude, err := ParseASNFilterEntry(entry)
		if err != nil {
			return model.PlatformIPInfoFilter{}, fmt.Errorf("ipinfo_filters.asns[%d]: %s", i, err.Error())
		}
		if exclude {
			if seenInclude[asn] {
				return model.PlatformIPInfoFilter{}, fmt.Errorf(
					"ipinfo_filters.asns[%d]: AS%d both included and excluded", i, asn,
				)
			}
			if seenExclude[asn] {
				continue
			}
			seenExclude[asn] = true
			out.ASNs = append(out.ASNs, "!"+strconv.FormatUint(uint64(asn), 10))
			continue
		}
		if seenExclude[asn] {
			return model.PlatformIPInfoFilter{}, fmt.Errorf(
				"ipinfo_filters.asns[%d]: AS%d both included and excluded", i, asn,
			)
		}
		if seenInclude[asn] {
			continue
		}
		seenInclude[asn] = true
		out.ASNs = append(out.ASNs, strconv.FormatUint(uint64(asn), 10))
	}

	return out, nil
}

// CompileIPInfoFilter builds the runtime filter from a persisted filter.
func CompileIPInfoFilter(raw model.PlatformIPInfoFilter) (IPInfoFilter, error) {
	normalized, err := NormalizeIPInfoFilter(raw)
	if err != nil {
		return IPInfoFilter{}, err
	}

	filter := IPInfoFilter{
		MinFraudScore: normalized.MinFraudScore,
		MaxFraudScore: normalized.MaxFraudScore,
		IPType:        IPUsage(normalized.IPType),
		Native:        IPNative(normalized.Native),
	}
	if filter.IPType == "" {
		filter.IPType = IPUsageAny
	}
	if filter.Native == "" {
		filter.Native = IPNativeAny
	}
	for _, entry := range normalized.ASNs {
		exclude := strings.HasPrefix(entry, "!")
		value := strings.TrimPrefix(entry, "!")
		asn64, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return IPInfoFilter{}, fmt.Errorf("ipinfo_filters.asns: invalid entry %q", entry)
		}
		if exclude {
			filter.ASNExcludes = append(filter.ASNExcludes, uint32(asn64))
			continue
		}
		filter.ASNIncludes = append(filter.ASNIncludes, uint32(asn64))
	}
	return filter, nil
}

// ParseASN parses a positive ASN, accepting an optional "AS" prefix.
func ParseASN(raw string) (uint32, error) {
	asn, exclude, err := ParseASNFilterEntry(raw)
	if err != nil {
		return 0, err
	}
	if exclude {
		return 0, fmt.Errorf("ASN must not be negated")
	}
	return asn, nil
}

// ParseASNFilterEntry parses one ASN filter entry, which may carry a "!"
// negation prefix and an "AS" prefix. Returns the ASN and whether it is
// negated.
func ParseASNFilterEntry(entry string) (uint32, bool, error) {
	value := strings.TrimSpace(entry)
	if value == "" {
		return 0, false, fmt.Errorf("must not be empty")
	}
	exclude := false
	if value[0] == '!' {
		exclude = true
		value = strings.TrimSpace(value[1:])
	}
	if len(value) >= 2 && (value[0] == 'A' || value[0] == 'a') && (value[1] == 'S' || value[1] == 's') {
		value = value[2:]
	}
	if value == "" {
		return 0, false, fmt.Errorf("must be an ASN such as 132203 or AS132203")
	}
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return 0, false, fmt.Errorf("must be an ASN such as 132203 or AS132203")
		}
	}
	asn64, err := strconv.ParseUint(value, 10, 32)
	if err != nil || asn64 == 0 {
		return 0, false, fmt.Errorf("must be a valid ASN between 1 and 4294967295")
	}
	return uint32(asn64), exclude, nil
}

func normalizeIPUsage(raw string) (IPUsage, error) {
	switch IPUsage(strings.ToLower(strings.TrimSpace(raw))) {
	case "", IPUsageAny:
		return IPUsageAny, nil
	case IPUsageResidential:
		return IPUsageResidential, nil
	case IPUsageHosting:
		return IPUsageHosting, nil
	default:
		return "", fmt.Errorf("ipinfo_filters.ip_type: must be any, residential, or hosting")
	}
}

func normalizeIPNative(raw string) (IPNative, error) {
	switch IPNative(strings.ToLower(strings.TrimSpace(raw))) {
	case "", IPNativeAny:
		return IPNativeAny, nil
	case IPNativeNative:
		return IPNativeNative, nil
	case IPNativeBroadcast:
		return IPNativeBroadcast, nil
	default:
		return "", fmt.Errorf("ipinfo_filters.native: must be any, native, or broadcast")
	}
}
