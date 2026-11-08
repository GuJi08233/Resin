package api

import (
	"cmp"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Resinat/Resin/internal/platform"
	"github.com/Resinat/Resin/internal/service"
)

func nodeTagSortKey(n service.NodeSummary) string {
	if n.DisplayTag != "" {
		return n.DisplayTag
	}
	if len(n.Tags) == 0 {
		return ""
	}
	bestCreated := int64(math.MaxInt64)
	bestTag := ""
	for _, t := range n.Tags {
		if t.SubscriptionCreatedAtNs < bestCreated {
			bestCreated = t.SubscriptionCreatedAtNs
			bestTag = t.Tag
			continue
		}
		if t.SubscriptionCreatedAtNs == bestCreated && (bestTag == "" || t.Tag < bestTag) {
			bestTag = t.Tag
		}
	}
	return bestTag
}

func compareNodeSummaries(sortBy string, a, b service.NodeSummary) int {
	order := 0
	switch sortBy {
	case "created_at":
		order = strings.Compare(a.CreatedAt, b.CreatedAt)
	case "failure_count":
		order = cmp.Compare(a.FailureCount, b.FailureCount)
	case "region":
		order = strings.Compare(a.Region, b.Region)
	case "fraud_score":
		if a.IPInfo != nil && b.IPInfo != nil {
			order = cmp.Compare(a.IPInfo.FraudScore, b.IPInfo.FraudScore)
		}
	default:
		order = strings.Compare(nodeTagSortKey(a), nodeTagSortKey(b))
	}
	if order != 0 {
		return order
	}
	return strings.Compare(a.NodeHash, b.NodeHash)
}

func sortNodeSummaries(nodes []service.NodeSummary, sorting Sorting) {
	slices.SortStableFunc(nodes, func(a, b service.NodeSummary) int {
		// Nodes without an IP profile always sort last, in both directions,
		// so they do not crowd out scored nodes.
		if sorting.SortBy == "fraud_score" {
			aHas, bHas := a.IPInfo != nil, b.IPInfo != nil
			if aHas != bHas {
				if aHas {
					return -1
				}
				return 1
			}
		}
		return applySortOrder(compareNodeSummaries(sorting.SortBy, a, b), sorting.SortOrder)
	})
}

type nodeListPageResponse struct {
	Items                  []service.NodeSummary `json:"items"`
	Total                  int                   `json:"total"`
	Limit                  int                   `json:"limit"`
	Offset                 int                   `json:"offset"`
	UniqueEgressIPs        int                   `json:"unique_egress_ips"`
	UniqueHealthyEgressIPs int                   `json:"unique_healthy_egress_ips"`
}

func countUniqueEgressIPs(nodes []service.NodeSummary) int {
	seen := make(map[string]struct{})
	for _, n := range nodes {
		if n.EgressIP == "" {
			continue
		}
		seen[n.EgressIP] = struct{}{}
	}
	return len(seen)
}

func countUniqueHealthyAndEnabledEgressIPs(nodes []service.NodeSummary) int {
	seen := make(map[string]struct{})
	for _, n := range nodes {
		if n.EgressIP == "" {
			continue
		}
		if !n.IsHealthyAndEnabled() {
			continue
		}
		seen[n.EgressIP] = struct{}{}
	}
	return len(seen)
}

// parseOptionalIntQuery parses an optional integer query parameter within
// [minValue, maxValue]. Returns nil when the parameter is absent.
func parseOptionalIntQuery(
	w http.ResponseWriter,
	r *http.Request,
	key string,
	minValue int,
	maxValue int,
) (*int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return nil, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minValue || value > maxValue {
		writeInvalidArgument(w, fmt.Sprintf("%s: must be an integer between %d and %d", key, minValue, maxValue))
		return nil, false
	}
	return &value, true
}

// parseIPInfoFilterQuery builds an IP profile filter from node list query
// parameters. An empty filter is inactive and matches every node.
func parseIPInfoFilterQuery(w http.ResponseWriter, r *http.Request) (platform.IPInfoFilter, bool) {
	var filter platform.IPInfoFilter

	minScore, ok := parseOptionalIntQuery(w, r, "min_fraud_score", 0, 100)
	if !ok {
		return platform.IPInfoFilter{}, false
	}
	maxScore, ok := parseOptionalIntQuery(w, r, "max_fraud_score", 0, 100)
	if !ok {
		return platform.IPInfoFilter{}, false
	}
	if minScore != nil && maxScore != nil && *minScore > *maxScore {
		writeInvalidArgument(w, "min_fraud_score: must be <= max_fraud_score")
		return platform.IPInfoFilter{}, false
	}
	filter.MinFraudScore = minScore
	filter.MaxFraudScore = maxScore

	if raw := strings.TrimSpace(r.URL.Query().Get("ip_type")); raw != "" {
		usage := platform.IPUsage(strings.ToLower(raw))
		switch usage {
		case platform.IPUsageAny:
		case platform.IPUsageResidential, platform.IPUsageHosting:
			filter.IPType = usage
		default:
			writeInvalidArgument(w, "ip_type: must be any, residential, or hosting")
			return platform.IPInfoFilter{}, false
		}
	}

	if raw := strings.TrimSpace(r.URL.Query().Get("native")); raw != "" {
		native := platform.IPNative(strings.ToLower(raw))
		switch native {
		case platform.IPNativeAny:
		case platform.IPNativeNative, platform.IPNativeBroadcast:
			filter.Native = native
		default:
			writeInvalidArgument(w, "native: must be any, native, or broadcast")
			return platform.IPInfoFilter{}, false
		}
	}

	// `asn` accepts a comma-separated list and may be repeated. Entries may
	// carry an optional "AS" prefix; a leading "!" excludes the ASN.
	for _, raw := range r.URL.Query()["asn"] {
		for _, entry := range strings.Split(raw, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			asn, exclude, err := platform.ParseASNFilterEntry(entry)
			if err != nil {
				writeInvalidArgument(w, "asn: "+err.Error())
				return platform.IPInfoFilter{}, false
			}
			if exclude {
				filter.ASNExcludes = append(filter.ASNExcludes, asn)
				continue
			}
			filter.ASNIncludes = append(filter.ASNIncludes, asn)
		}
	}

	return filter, true
}

// HandleListNodes returns a handler for GET /api/v1/nodes.
func HandleListNodes(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		filters := service.NodeFilters{}

		platformID, ok := parseOptionalUUIDQuery(w, r, "platform_id", "platform_id")
		if !ok {
			return
		}
		filters.PlatformID = platformID

		subscriptionID, ok := parseOptionalSubscriptionIDQuery(w, r)
		if !ok {
			return
		}
		filters.SubscriptionID = subscriptionID

		if v := q.Get("region"); v != "" {
			filters.Region = &v
		}
		if v := q.Get("egress_ip"); v != "" {
			filters.EgressIP = &v
		}
		if v := strings.TrimSpace(q.Get("tag_keyword")); v != "" {
			filters.TagKeyword = &v
		}

		circuitOpen, ok := parseBoolQueryOrWriteInvalid(w, r, "circuit_open")
		if !ok {
			return
		}
		filters.CircuitOpen = circuitOpen

		hasOutbound, ok := parseBoolQueryOrWriteInvalid(w, r, "has_outbound")
		if !ok {
			return
		}
		filters.HasOutbound = hasOutbound

		enabled, ok := parseBoolQueryOrWriteInvalid(w, r, "enabled")
		if !ok {
			return
		}
		filters.Enabled = enabled

		if v := q.Get("probed_since"); v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				writeInvalidArgument(w, "probed_since: invalid RFC3339 timestamp")
				return
			}
			filters.ProbedSince = &t
		}

		ipInfoFilters, ok := parseIPInfoFilterQuery(w, r)
		if !ok {
			return
		}
		filters.IPInfoFilters = ipInfoFilters

		hasIPInfo, ok := parseBoolQueryOrWriteInvalid(w, r, "has_ipinfo")
		if !ok {
			return
		}
		filters.HasIPInfo = hasIPInfo

		nodes, err := cp.ListNodes(filters)
		if err != nil {
			writeServiceError(w, err)
			return
		}

		sorting, ok := parseSortingOrWriteInvalid(w, r, []string{"tag", "created_at", "failure_count", "region", "fraud_score"}, "tag", "asc")
		if !ok {
			return
		}
		sortNodeSummaries(nodes, sorting)

		pg, ok := parsePaginationOrWriteInvalid(w, r)
		if !ok {
			return
		}
		WriteJSON(w, http.StatusOK, nodeListPageResponse{
			Items:                  PaginateSlice(nodes, pg),
			Total:                  len(nodes),
			Limit:                  pg.Limit,
			Offset:                 pg.Offset,
			UniqueEgressIPs:        countUniqueEgressIPs(nodes),
			UniqueHealthyEgressIPs: countUniqueHealthyAndEnabledEgressIPs(nodes),
		})
	}
}

// HandleGetNode returns a handler for GET /api/v1/nodes/{hash}.
func HandleGetNode(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hash := PathParam(r, "hash")
		n, err := cp.GetNode(hash)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, n)
	}
}

// HandleProbeEgress returns a handler for POST /api/v1/nodes/{hash}/actions/probe-egress.
func HandleProbeEgress(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hash := PathParam(r, "hash")
		result, err := cp.ProbeEgress(hash)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, result)
	}
}

// HandleProbeIPInfo returns a handler for POST /api/v1/nodes/{hash}/actions/probe-ipinfo.
func HandleProbeIPInfo(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hash := PathParam(r, "hash")
		result, err := cp.ProbeIPInfo(hash)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, result)
	}
}

// HandleProbeLatency returns a handler for POST /api/v1/nodes/{hash}/actions/probe-latency.
func HandleProbeLatency(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hash := PathParam(r, "hash")
		result, err := cp.ProbeLatency(hash)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, result)
	}
}
