package ipinfo

import (
	"testing"
)

const sampleBody = `{
    "ip": "170.106.108.125",
    "asn": 132203,
    "asOrganization": "16 COLLYER QUAY",
    "country": "United States",
    "countryCode": "US",
    "region": "California",
    "regionCode": "CA",
    "city": "Santa Clara",
    "timezone": "America/Los_Angeles",
    "longitude": "-121.95524",
    "latitude": "37.35411",
    "postalCode": "95050",
    "fraudScore": 56,
    "isResidential": false,
    "isBroadcast": true,
    "userAgent": "curl/8.5.0"
}`

func TestParse_Sample(t *testing.T) {
	got, err := Parse([]byte(sampleBody))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.IP != "170.106.108.125" {
		t.Fatalf("ip: got %q", got.IP)
	}
	if got.ASN != 132203 {
		t.Fatalf("asn: got %d", got.ASN)
	}
	if got.ASOrganization != "16 COLLYER QUAY" {
		t.Fatalf("asOrganization: got %q", got.ASOrganization)
	}
	if got.CountryCode != "us" {
		t.Fatalf("countryCode: got %q, want lowercase", got.CountryCode)
	}
	if got.City != "Santa Clara" {
		t.Fatalf("city: got %q", got.City)
	}
	if got.Latitude != "37.35411" || got.Longitude != "-121.95524" {
		t.Fatalf("coords: got %q,%q", got.Latitude, got.Longitude)
	}
	if got.PostalCode != "95050" {
		t.Fatalf("postalCode: got %q", got.PostalCode)
	}
	if got.FraudScore != 56 {
		t.Fatalf("fraudScore: got %d", got.FraudScore)
	}
	if got.RiskLevel != RiskLevelMediumRisk {
		t.Fatalf("riskLevel: got %q", got.RiskLevel)
	}
	if got.IsResidential {
		t.Fatal("isResidential: want false")
	}
	if !got.IsBroadcast {
		t.Fatal("isBroadcast: want true")
	}
}

func TestParse_NumericFieldsAsStrings(t *testing.T) {
	body := []byte(`{"ip":"1.2.3.4","asn":"13335","fraudScore":"75","latitude":34.05,"longitude":-118.24}`)
	got, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.ASN != 13335 {
		t.Fatalf("asn from string: got %d", got.ASN)
	}
	if got.FraudScore != 75 {
		t.Fatalf("fraudScore from string: got %d", got.FraudScore)
	}
	if got.Latitude != "34.05" || got.Longitude != "-118.24" {
		t.Fatalf("numeric coords: got %q,%q", got.Latitude, got.Longitude)
	}
}

func TestParse_MissingIP(t *testing.T) {
	if _, err := Parse([]byte(`{"asn":1}`)); err == nil {
		t.Fatal("expected error for missing ip")
	}
}

func TestParse_EmptyBody(t *testing.T) {
	if _, err := Parse(nil); err == nil {
		t.Fatal("expected error for empty body")
	}
}

func TestParse_ClampsFraudScore(t *testing.T) {
	got, err := Parse([]byte(`{"ip":"1.2.3.4","fraudScore":150}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.FraudScore != 100 {
		t.Fatalf("fraudScore clamp: got %d, want 100", got.FraudScore)
	}
}

func TestRiskLevel_Boundaries(t *testing.T) {
	cases := []struct {
		score int
		want  string
	}{
		{0, RiskLevelVeryClean},
		{14, RiskLevelVeryClean},
		{15, RiskLevelClean},
		{24, RiskLevelClean},
		{25, RiskLevelNeutral},
		{39, RiskLevelNeutral},
		{40, RiskLevelLightRisk},
		{49, RiskLevelLightRisk},
		{50, RiskLevelMediumRisk},
		{69, RiskLevelMediumRisk},
		{70, RiskLevelHighRisk},
		{100, RiskLevelHighRisk},
	}
	for _, tc := range cases {
		if got := RiskLevel(tc.score); got != tc.want {
			t.Errorf("RiskLevel(%d) = %q, want %q", tc.score, got, tc.want)
		}
	}
}
