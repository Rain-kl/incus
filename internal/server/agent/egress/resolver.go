package egress

import (
	"fmt"
	"net"
	"os"
	"strings"
)

const namespaceResolverPath = "/etc/netns/" + namespace + "/resolv.conf"

// A loopback stub belongs to the host's network namespace. The gateway must
// use its real upstreams, not an arbitrary public resolver or the managed
// bridge's dnsmasq, which would forward straight back to Xray.
func hostBootstrapResolvers(inv inventory) ([]string, error) {
	return readBootstrapResolvers(os.ReadFile, inv)
}

func readBootstrapResolvers(read func(string) ([]byte, error), inv inventory) ([]string, error) {
	excluded := map[string]bool{"169.254.242.1": true, "169.254.242.2": true, "fd42:696e:6375::1": true, "fd42:696e:6375::2": true}
	for _, network := range inv.Networks {
		if !network.Managed || network.Type != "bridge" {
			continue
		}

		for _, key := range []string{"ipv4.address", "ipv6.address"} {
			{
				ip, _, err := net.ParseCIDR(network.Config[key])
				if err == nil {
					excluded[ip.String()] = true
				}
			}
		}
	}

	for _, path := range []string{"/etc/resolv.conf", "/run/systemd/resolve/resolv.conf", "/run/NetworkManager/no-stub-resolv.conf", "/run/NetworkManager/resolv.conf"} {
		body, err := read(path)
		if err != nil {
			continue
		}

		servers := []string{}
		seen := map[string]bool{}
		for _, line := range strings.Split(string(body), "\n") {
			fields := strings.Fields(strings.SplitN(line, "#", 2)[0])
			if len(fields) < 2 || fields[0] != "nameserver" {
				continue
			}

			ip := net.ParseIP(fields[1])
			if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || (ip.To4() == nil && ip.IsLinkLocalUnicast()) || excluded[ip.String()] || seen[ip.String()] {
				continue
			}

			seen[ip.String()] = true
			servers = append(servers, ip.String())
			// Both libc and Go support at most three resolv.conf nameservers.
			if len(servers) == 3 {
				break
			}
		}

		if len(servers) > 0 {
			return servers, nil
		}
	}

	return nil, fmt.Errorf("host DNS upstreams unavailable")
}

func namespaceResolverConfig(servers []string) []byte {
	var body strings.Builder
	for _, server := range servers {
		body.WriteString("nameserver " + server + "\n")
	}
	// A dead first upstream must not consume the entire proxy HTTPS timeout.
	body.WriteString("options timeout:1 attempts:2 rotate\n")
	return []byte(body.String())
}
