package subscription

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Resinat/Resin/internal/node"
)

func mieruTestClashNode() map[string]any {
	return map[string]any{
		"name": "test-mieru", "type": "mieru", "server": "198.51.100.1", "port": 2012,
		"transport": "TCP", "handshake-mode": "HANDSHAKE_NO_WAIT",
		"username": "example-user", "password": "example-secret", "multiplexing": "MULTIPLEXING_HIGH",
	}
}

func TestMieruClashYAMLListsAndBase64(t *testing.T) {
	var list strings.Builder
	for index := 1; index <= 8; index++ {
		fmt.Fprintf(&list, "- { name: 测试节点%d, type: mieru, server: 198.51.100.%d, port: 2012, transport: TCP, handshake-mode: HANDSHAKE_NO_WAIT, username: 'example-user', password: example-secret, multiplexing: MULTIPLEXING_HIGH }\n", index, index)
	}
	bare := list.String()
	wrapped := "proxies:\n" + bare
	for _, input := range []struct{ name, value string }{
		{"wrapped", wrapped},
		{"bare", bare},
		{"bare-indented", "    " + strings.ReplaceAll(strings.TrimSuffix(bare, "\n"), "\n", "\n    ") + "\n"},
		{"bare-with-comments", "# exported nodes\n---\n" + bare},
		{"base64-wrapped", base64.StdEncoding.EncodeToString([]byte(wrapped))},
		{"base64-bare", base64.StdEncoding.EncodeToString([]byte(bare))},
	} {
		t.Run(input.name, func(t *testing.T) {
			nodes, err := ParseGeneralSubscription([]byte(input.value))
			if err != nil || len(nodes) != 8 {
				t.Fatalf("expected eight parsed Mieru nodes, got %d, err=%v", len(nodes), err)
			}
			seen := make(map[node.Hash]bool)
			for index, parsed := range nodes {
				raw := parseNodeRaw(t, parsed.RawOptions)
				if raw["type"] != "mieru" || raw["transport"] != "tcp" || raw["handshake_mode"] != "HANDSHAKE_NO_WAIT" ||
					raw["multiplexing"] != "MULTIPLEXING_HIGH" || raw["username"] != "example-user" || raw["password"] != "example-secret" ||
					raw["server_port"] != float64(2012) || parsed.Tag != fmt.Sprintf("测试节点%d", index+1) {
					t.Fatalf("Mieru protocol settings were changed or lost: %+v", raw)
				}
				hash := node.HashFromRawOptions(parsed.RawOptions)
				if seen[hash] {
					t.Fatal("different Mieru servers collapsed into the same identity")
				}
				seen[hash] = true
			}
		})
	}
}

func TestMieruIndentedBareBlockList(t *testing.T) {
	data := `    - name: indented-block
      type: mieru
      server: 198.51.100.1
      port: 2012
      transport: TCP
      username: example-user
      password: example-secret
      handshake-mode: HANDSHAKE_NO_WAIT
      multiplexing: MULTIPLEXING_HIGH
`
	nodes, err := ParseGeneralSubscription([]byte(data))
	if err != nil || len(nodes) != 1 || nodes[0].Tag != "indented-block" {
		t.Fatalf("uniformly indented block list was not imported: nodes=%+v, err=%v", nodes, err)
	}
	raw := parseNodeRaw(t, nodes[0].RawOptions)
	if raw["transport"] != "tcp" || raw["handshake_mode"] != "HANDSHAKE_NO_WAIT" || raw["multiplexing"] != "MULTIPLEXING_HIGH" {
		t.Fatalf("indented block list lost Mieru options: %+v", raw)
	}
}

func TestMieruClashJSONAndNumericCredentials(t *testing.T) {
	proxy := mieruTestClashNode()
	proxy["username"], proxy["password"] = 123456, 987654
	proxy["transport"] = "uDp"
	proxy["handshake-mode"] = "handshake_standard"
	proxy["multiplexing"] = "multiplexing_middle"
	data, err := json.Marshal(map[string]any{"proxies": []any{proxy}})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := ParseGeneralSubscription(data)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("parse JSON Mieru: len=%d err=%v", len(nodes), err)
	}
	raw := parseNodeRaw(t, nodes[0].RawOptions)
	if raw["username"] != "123456" || raw["password"] != "987654" || raw["transport"] != "udp" ||
		raw["handshake_mode"] != "HANDSHAKE_STANDARD" || raw["multiplexing"] != "MULTIPLEXING_MIDDLE" {
		t.Fatalf("JSON values not normalized: %+v", raw)
	}

	nodes, err = ParseGeneralSubscription([]byte("proxies:\n- {type: mieru, name: number-user, server: 198.51.100.2, port: 2012, username: 123456, password: 987654}\n"))
	if err != nil || len(nodes) != 1 {
		t.Fatalf("parse YAML numeric credentials: len=%d err=%v", len(nodes), err)
	}
	raw = parseNodeRaw(t, nodes[0].RawOptions)
	if raw["username"] != "123456" || raw["password"] != "987654" {
		t.Fatalf("YAML numeric credentials were not stringified: %+v", raw)
	}
}

func TestMieruDefaultModesAndPasswordPreservation(t *testing.T) {
	proxy := mieruTestClashNode()
	delete(proxy, "transport")
	delete(proxy, "handshake-mode")
	delete(proxy, "multiplexing")
	proxy["password"] = " secret with spaces "
	parsed, ok := convertClashProxyToNode(proxy)
	if !ok {
		t.Fatal("optional Mieru settings should use their documented defaults")
	}
	raw := parseNodeRaw(t, parsed.RawOptions)
	if raw["transport"] != "tcp" || raw["handshake_mode"] != "HANDSHAKE_STANDARD" ||
		raw["multiplexing"] != "MULTIPLEXING_LOW" || raw["password"] != " secret with spaces " {
		t.Fatalf("incorrect defaults or altered password: %+v", raw)
	}
}

func TestMieruModesAndDialOptionsSurviveConversion(t *testing.T) {
	for _, handshake := range []string{"HANDSHAKE_DEFAULT", "HANDSHAKE_STANDARD", "HANDSHAKE_NO_WAIT"} {
		for _, multiplexing := range []string{"MULTIPLEXING_DEFAULT", "MULTIPLEXING_OFF", "MULTIPLEXING_LOW", "MULTIPLEXING_MIDDLE", "MULTIPLEXING_HIGH"} {
			proxy := mieruTestClashNode()
			delete(proxy, "handshake-mode")
			proxy["handshake_mode"], proxy["multiplexing"] = handshake, multiplexing
			proxy["dialer-proxy"], proxy["ip-version"] = "upstream", "ipv4"
			parsed, ok := convertClashProxyToNode(proxy)
			if !ok {
				t.Fatalf("valid Mieru enum pair rejected: %s/%s", handshake, multiplexing)
			}
			raw := parseNodeRaw(t, parsed.RawOptions)
			if raw["handshake_mode"] != handshake || raw["multiplexing"] != multiplexing || raw["detour"] != "upstream" || raw["domain_strategy"] != "ipv4_only" {
				t.Fatalf("Mieru options lost: %+v", raw)
			}
		}
	}
}

func TestMieruInvalidClashNodesDoNotDiscardOtherProtocols(t *testing.T) {
	cases := []struct {
		field string
		value any
	}{
		{"server", ""}, {"username", ""}, {"password", ""}, {"username", map[string]any{"bad": true}},
		{"port", 0}, {"port", -1}, {"port", 65536}, {"port", 2012.5},
		{"transport", "QUIC"}, {"transport", map[string]any{"type": "tcp"}}, {"transport", ""},
		{"handshake-mode", "HANDSHAKE_UNKNOWN"}, {"handshake-mode", nil},
		{"multiplexing", "MULTIPLEXING_UNKNOWN"}, {"multiplexing", 4}, {"multiplexing", map[string]any{"level": "MULTIPLEXING_HIGH"}},
	}
	for _, test := range cases {
		t.Run(fmt.Sprintf("%s-%v", test.field, test.value), func(t *testing.T) {
			bad := mieruTestClashNode()
			bad[test.field] = test.value
			good := map[string]any{"name": "socks-survives", "type": "socks5", "server": "198.51.100.200", "port": 1080}
			data, err := json.Marshal(map[string]any{"proxies": []any{bad, good}})
			if err != nil {
				t.Fatal(err)
			}
			nodes, err := ParseGeneralSubscription(data)
			if err != nil || len(nodes) != 1 || nodes[0].Tag != "socks-survives" {
				t.Fatalf("invalid Mieru must be skipped without affecting other nodes: %+v, err=%v", nodes, err)
			}
		})
	}
}

func TestMieruRawOutboundsPreserveProtocolOptions(t *testing.T) {
	raw := `{"type":"mieru","tag":"raw-mieru","server":"198.51.100.1","server_port":2012,"username":"example-user","password":"example-secret","transport":"tcp","handshake_mode":"HANDSHAKE_NO_WAIT","multiplexing":"MULTIPLEXING_HIGH"}`
	for _, data := range []string{"[" + raw + "]", `{"outbounds":[` + raw + "]}"} {
		nodes, err := ParseGeneralSubscription([]byte(data))
		if err != nil || len(nodes) != 1 || nodes[0].Tag != "raw-mieru" || string(nodes[0].RawOptions) != raw {
			t.Fatalf("raw Mieru was rejected or changed: %+v, err=%v", nodes, err)
		}
	}
}

func TestMieruHashIgnoresTagButTracksProtocolModes(t *testing.T) {
	proxy := mieruTestClashNode()
	first, _ := convertClashProxyToNode(proxy)
	proxy["name"] = "renamed"
	renamed, _ := convertClashProxyToNode(proxy)
	if node.HashFromRawOptions(first.RawOptions) != node.HashFromRawOptions(renamed.RawOptions) {
		t.Fatal("renaming Mieru node changed its identity")
	}
	for _, setting := range []struct{ field, value string }{{"handshake-mode", "HANDSHAKE_STANDARD"}, {"multiplexing", "MULTIPLEXING_OFF"}, {"transport", "UDP"}} {
		changedProxy := mieruTestClashNode()
		changedProxy[setting.field] = setting.value
		changed, _ := convertClashProxyToNode(changedProxy)
		if node.HashFromRawOptions(first.RawOptions) == node.HashFromRawOptions(changed.RawOptions) {
			t.Fatalf("changing %s did not change node identity", setting.field)
		}
	}
}
