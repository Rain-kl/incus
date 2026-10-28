package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Constants define protocol identifiers and timing limits.
const (
	namespace       = "incudal-egress"
	stateDir        = "/var/lib/incudal-agent/network"
	transparentPort = 15490
	apiPort         = 15491
	socksBasePort   = 21000
)

type coreValidationError struct{ reason string }

func (e *coreValidationError) Error() string { return e.reason }
func validateCore(ctx context.Context, binary, path string) error {
	output, err := exec.CommandContext(ctx, binary, "run", "-test", "-format", "json", "-config", path).CombinedOutput()
	if err == nil {
		return nil
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Xray can include credentials in configuration errors. Report only known
	// categories; never forward its raw stdout/stderr to the Panel or journal.
	message := strings.ToLower(string(output))
	reason := "config_invalid"
	for _, item := range []struct{ pattern, reason string }{
		{`invalid "publickey"`, "reality_key_invalid"},
		{`invalid "password"`, "reality_key_invalid"},
		{`invalid "shortid"`, "reality_id_invalid"},
		{"fingerprint", "tls_config_invalid"},
		{"permission denied", "permission_denied"},
		{"unknown transport", "unsupported_transport"},
	} {
		if strings.Contains(message, item.pattern) {
			reason = item.reason
			break
		}
	}

	return &coreValidationError{reason: reason}
}

func nodeTag(id int) string { return fmt.Sprintf("proxy-%d-", id) } // Delimiter prevents prefix matches with another ID.
func xrayConfig(in Instruction, probePorts []int, transparent bool) ([]byte, error) {
	outbounds := []any{map[string]any{"tag": "blocked", "protocol": "blackhole", "settings": map[string]any{}}, map[string]any{"tag": "direct", "protocol": "freedom", "settings": map[string]any{}}}
	rules := []any{}
	inbounds := []any{}
	tags := []string{}
	for i, node := range in.Nodes {
		var outbound map[string]any
		{
			err := json.Unmarshal(node.Outbound, &outbound)
			if err != nil {
				return nil, fmt.Errorf("invalid outbound")
			}
		}

		outbound["tag"] = nodeTag(node.ID)
		outbounds = append(outbounds, outbound)
		tags = append(tags, nodeTag(node.ID))
		port := socksBasePort + i
		if len(probePorts) > i {
			port = probePorts[i]
		}

		tag := fmt.Sprintf("probe-%d", i)
		inbounds = append(inbounds, map[string]any{"tag": tag, "listen": "127.0.0.1", "port": port, "protocol": "socks", "settings": map[string]any{"auth": "noauth", "udp": true}})
		rules = append(rules, map[string]any{"type": "field", "inboundTag": []string{tag}, "outboundTag": nodeTag(node.ID)})
	}

	config := map[string]any{"log": map[string]any{"loglevel": "none"}, "inbounds": inbounds, "outbounds": outbounds}
	if transparent {
		inbounds = append(inbounds, map[string]any{"tag": "dns-forward", "listen": "169.254.242.2", "port": 53, "protocol": "dokodemo-door", "settings": map[string]any{"address": "1.1.1.1", "port": 53, "network": "tcp,udp"}})
		inbounds = append(inbounds, map[string]any{"tag": "transparent", "listen": "::", "port": transparentPort, "protocol": "dokodemo-door", "settings": map[string]any{"network": "tcp,udp", "followRedirect": true}, "streamSettings": map[string]any{"sockopt": map[string]any{"tproxy": "tproxy"}}})
		// Pinned dokodemo Unix listeners need a non-zero target port even for virtual API routing.
		inbounds = append(inbounds, map[string]any{"tag": "api-in", "listen": coreAPIPath(stateDir), "protocol": "dokodemo-door", "settings": map[string]any{"address": "127.0.0.1", "port": apiPort, "network": "unix"}})
		config["api"] = map[string]any{"tag": "api", "services": []string{"RoutingService", "HandlerService"}}
		rules = append(rules, map[string]any{"type": "field", "inboundTag": []string{"api-in"}, "outboundTag": "api"})
		route := map[string]any{"type": "field", "inboundTag": []string{"transparent", "dns-forward"}, "outboundTag": "direct"}
		if in.Egress.Mode == "proxy" {
			delete(route, "outboundTag")
			route["balancerTag"] = "egress"
			strategy := "roundRobin"
			if in.Egress.Strategy == "leastPing" {
				strategy = "leastPing"
			}

			config["routing"] = map[string]any{"domainStrategy": "AsIs", "balancers": []any{map[string]any{"tag": "egress", "selector": tags, "fallbackTag": "blocked", "strategy": map[string]any{"type": strategy}}}}
			config["observatory"] = map[string]any{"subjectSelector": tags, "probeURL": in.Egress.ProbeURL, "probeInterval": fmt.Sprintf("%ds", in.Egress.ProbeIntervalSeconds), "enableConcurrency": true}
		}

		rules = append(rules, route)
	}

	config["inbounds"] = inbounds
	routing, ok := config["routing"].(map[string]any)
	if !ok {
		routing = map[string]any{"domainStrategy": "AsIs"}
	}

	routing["rules"] = rules
	config["routing"] = routing
	return json.MarshalIndent(config, "", "  ")
}
