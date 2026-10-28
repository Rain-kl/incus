package egress

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// Both DNS transports must reach the gateway; HTTPS alone cannot prove tenant DNS is operational.
func probeDNS(ctx context.Context, address, name string) error {
	if net.ParseIP(name) != nil {
		name = "example.com"
	}
	if name == "" {
		return fmt.Errorf("invalid DNS probe")
	}
	for _, network := range []string{"tcp", "udp"} {
		query := new(dns.Msg)
		query.SetQuestion(dns.Fqdn(strings.ToLower(name)), dns.TypeA)
		client := &dns.Client{Net: network, Timeout: 2 * time.Second}
		response, _, err := client.ExchangeContext(ctx, query, address)
		if err != nil {
			return fmt.Errorf("proxy DNS unavailable")
		}
		if response == nil || !response.Response || response.Id != query.Id || len(response.Question) != 1 || (response.Question[0].Qtype != query.Question[0].Qtype || response.Question[0].Qclass != query.Question[0].Qclass || !strings.EqualFold(response.Question[0].Name, query.Question[0].Name)) || (response.Rcode != dns.RcodeSuccess && response.Rcode != dns.RcodeNameError) {
			return fmt.Errorf("invalid proxy DNS response")
		}
	}
	return nil
}

func probeDNSForURL(ctx context.Context, address, target string) error {
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return fmt.Errorf("invalid probe target")
	}
	return probeDNS(ctx, address, parsed.Hostname())
}

func unitReference(ctx context.Context, unit string) (string, error) {
	output, err := networkCommand(ctx, "systemctl", "show", "--property=ActiveState", "--property=SubState", "--property=InvocationID", unit).Output()
	if err != nil {
		return "", errCoreAPI
	}
	return parseUnitReference(output)
}

func parseUnitReference(output []byte) (string, error) {
	fields := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			fields[key] = value
		}
	}
	decoded, err := hex.DecodeString(fields["InvocationID"])
	if err != nil || len(decoded) != 16 || strings.Trim(fields["InvocationID"], "0") == "" {
		return "", errCoreAPI
	}
	if fields["ActiveState"] != "active" || fields["SubState"] != "running" || len(fields["InvocationID"]) != 32 {
		return "", errCoreAPI
	}
	return fields["InvocationID"], nil
}

func gatewayDNSHealthy(ctx context.Context, address, target string) bool {
	exe, err := workerExecutable()
	if err != nil {
		return false
	}
	output, err := networkCommand(ctx, "ip", "netns", "exec", namespace, exe, "-network-dns-address", address, "-network-probe-url", target).Output()
	var result ProbeResult
	return err == nil && json.Unmarshal(output, &result) == nil && result.Success
}

func blockLegacyGateway(ctx context.Context) {
	_ = nft(ctx, "flush set inet incudal_egress ready", false)
}
