package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lxc/incus/v7/internal/server/payincus/localapi"
)

const aclName = "incudal-host-firewall"

type (
	incusClient  struct{ client *http.Client }
	incusNetwork struct {
		Name    string            `json:"name"`
		Type    string            `json:"type"`
		Managed bool              `json:"managed"`
		Config  map[string]string `json:"config"`
	}
)

type incusInstance struct {
	Name            string                       `json:"name"`
	Devices         map[string]map[string]string `json:"devices"`
	ExpandedDevices map[string]map[string]string `json:"expanded_devices"`
	Config          map[string]string            `json:"config"`
	ExpandedConfig  map[string]string            `json:"expanded_config"`
}

type inventory struct {
	Networks      []incusNetwork
	Instances     []incusInstance
	Bridges       []string
	Routed        []string
	Internal      []string
	RoutedSources []string
}

func localIncus() (*incusClient, error) {
	for _, path := range localapi.SocketCandidates() {
		{
			_, err := os.Stat(path)
			if err != nil {
				continue
			}
		}

		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}}
		return &incusClient{&http.Client{Transport: transport, Timeout: 20 * time.Second}}, nil
	}

	return nil, fmt.Errorf("Incus socket unavailable")
}

func (c *incusClient) request(ctx context.Context, method, path string, body any, target any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://incus"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return os.ErrNotExist
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Incus API status %d", resp.StatusCode)
	}

	var envelope struct {
		Type      string          `json:"type"`
		Operation string          `json:"operation"`
		Metadata  json.RawMessage `json:"metadata"`
		Error     string          `json:"error"`
	}
	{
		err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&envelope)
		if err != nil {
			return err
		}
	}

	if envelope.Type == "error" {
		return fmt.Errorf("Incus API error")
	}

	if envelope.Type != "sync" && envelope.Type != "async" {
		return fmt.Errorf("invalid Incus response type")
	}

	if envelope.Type == "async" {
		if !strings.HasPrefix(envelope.Operation, "/1.0/operations/") {
			return fmt.Errorf("invalid Incus operation URL")
		}

		var operation struct {
			StatusCode int `json:"status_code"`
		}

		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			err = c.request(ctx, http.MethodGet, envelope.Operation+"/wait?timeout=10", nil, &operation)
			if err != nil {
				return err
			}

			if operation.StatusCode == 200 {
				return nil
			}

			if operation.StatusCode >= 400 {
				return fmt.Errorf("Incus operation failed")
			}

			if operation.StatusCode != 100 && operation.StatusCode != 103 && operation.StatusCode != 104 && operation.StatusCode != 105 {
				return fmt.Errorf("invalid Incus operation status")
			}
		}
	}

	if target != nil {
		return json.Unmarshal(envelope.Metadata, target)
	}

	return nil
}

func (c *incusClient) inventory(ctx context.Context) (inventory, error) {
	var inv inventory
	var info struct {
		Extensions []string `json:"api_extensions"`
	}

	err := c.request(ctx, "GET", "/1.0", nil, &info)
	if err != nil {
		return inv, err
	}

	found := map[string]bool{}
	for _, extension := range info.Extensions {
		found[extension] = true
	}

	for _, extension := range []string{"network_acl", "network_bridge_acl_devices"} {
		if !found[extension] {
			return inv, fmt.Errorf("required Incus API extension unavailable: %s", extension)
		}
	}
	{
		err := c.request(ctx, "GET", "/1.0/networks?recursion=1", nil, &inv.Networks)
		if err != nil {
			return inv, err
		}
	}
	{
		err := c.request(ctx, "GET", "/1.0/instances?recursion=1", nil, &inv.Instances)
		if err != nil {
			return inv, err
		}
	}

	if len(inv.Instances) > 10000 {
		return inv, fmt.Errorf("inventory limit exceeded")
	}

	managed := map[string]bool{}
	for _, network := range inv.Networks {
		if network.Managed && network.Type == "bridge" {
			managed[network.Name] = true
			inv.Bridges = append(inv.Bridges, network.Name)
			for _, key := range []string{"ipv4.address", "ipv6.address"} {
				{
					_, n, err := net.ParseCIDR(network.Config[key])
					if err == nil {
						inv.Internal = append(inv.Internal, n.String())
					}
				}
			}
		}
	}

	for _, instance := range inv.Instances {
		for name, device := range instance.ExpandedDevices {
			if device["type"] != "nic" {
				continue
			}

			kind := device["nictype"]
			if kind == "" && managed[device["network"]] {
				continue
			}

			if kind == "bridged" && (managed[device["parent"]] || managed[device["network"]]) {
				continue
			}

			if kind == "routed" {
				for _, key := range []string{"ipv4.address", "ipv6.address", "ipv4.routes", "ipv6.routes"} {
					for _, value := range strings.Split(device[key], ",") {
						value = strings.TrimSpace(value)
						if value == "" {
							continue
						}

						ip := net.ParseIP(value)
						if !strings.Contains(value, "/") {
							if ip == nil {
								return inv, fmt.Errorf("unsupported-nic")
							}

							if ip.To4() != nil {
								value += "/32"
							} else {
								value += "/128"
							}
						}

						_, prefix, e := net.ParseCIDR(value)
						if e != nil {
							return inv, fmt.Errorf("unsupported-nic")
						}

						bits, _ := prefix.Mask.Size()
						if bits == 0 {
							return inv, fmt.Errorf("unsupported-nic")
						}

						inv.RoutedSources = append(inv.RoutedSources, prefix.String())
						inv.Internal = append(inv.Internal, prefix.String())
					}
				}

				hostName := device["host_name"]
				if hostName == "" {
					hostName = instance.ExpandedConfig["volatile."+name+".host_name"]
				}

				if hostName == "" {
					hostName = instance.Config["volatile."+name+".host_name"]
				}
				// Stopped routed NICs have no host interface. Newly created NICs use
				// the iceg prefix at provisioning, covered by a wildcard before start.
				if hostName != "" {
					inv.Routed = append(inv.Routed, hostName)
				}

				continue
			}

			return inv, fmt.Errorf("unsupported-nic")
		}
	}

	sort.Strings(inv.Bridges)
	sort.Strings(inv.Routed)
	sort.Strings(inv.Internal)
	sort.Strings(inv.RoutedSources)
	return inv, nil
}

func aclBody(f Firewall) map[string]any {
	result := map[string]any{"description": "Managed by Incudal; original container addresses are filtered before Xray", "config": map[string]string{"user.incudal.managed": "1"}, "ingress": []any{}, "egress": []any{}}
	for _, rule := range f.Rules {
		if !rule.Enabled {
			continue
		}

		item := map[string]string{"action": rule.Action, "state": "enabled", "description": rule.Description}
		for k, v := range map[string]string{"source": rule.Source, "destination": rule.Destination, "protocol": rule.Protocol, "source_port": rule.SourcePort, "destination_port": rule.DestinationPort} {
			if v != "" {
				item[k] = v
			}
		}

		result[rule.Direction] = append(result[rule.Direction].([]any), item)
	}

	return result
}

func aclReferences(current string, enabled bool) string {
	items := []string{}
	for _, item := range strings.Split(current, ",") {
		item = strings.TrimSpace(item)
		if item != "" && item != aclName {
			items = append(items, item)
		}
	}

	if enabled {
		items = append(items, aclName)
	}

	return strings.Join(items, ",")
}

// Own defaults are saved in user.* metadata so disabling restores the previous
// values, including across Agent restarts. Existing unrelated ACLs are preserved.
func aclConfig(config map[string]string, f Firewall) map[string]string {
	patch := map[string]string{"security.acls": aclReferences(config["security.acls"], f.Enabled)}
	for _, direction := range []string{"ingress", "egress"} {
		key := "security.acls.default." + direction + ".action"
		saved := "user.incudal.acl." + direction
		if f.Enabled {
			{
				_, ok := config[saved]
				if !ok {
					patch[saved] = config[key]
					if patch[saved] == "" {
						patch[saved] = "unset"
					}
				}
			}

			value := f.IngressAction
			if direction == "egress" {
				value = f.EgressAction
			}

			patch[key] = value
		} else {
			previous, ok := config[saved]
			if ok {
				if previous == "unset" {
					previous = ""
				}

				patch[key] = previous
				patch[saved] = ""
			}
		}
	}

	return patch
}

func (c *incusClient) applyACL(ctx context.Context, inv inventory, f Firewall, proxyDNS bool) error {
	var existing map[string]any
	err := c.request(ctx, "GET", "/1.0/network-acls/"+aclName, nil, &existing)
	if err == nil {
		config, ok := existing["config"].(map[string]any)
		if !ok {
			return fmt.Errorf("invalid Incus ACL config response")
		}

		if config["user.incudal.managed"] != "1" {
			return fmt.Errorf("reserved ACL is not managed by Incudal")
		}
	}

	if f.Enabled {
		body := aclBody(f)
		switch err {
		case os.ErrNotExist:
			body["name"] = aclName
			err = c.request(ctx, "POST", "/1.0/network-acls", body, nil)
		case nil:
			err = c.request(ctx, "PUT", "/1.0/network-acls/"+aclName, body, nil)
		}

		if err != nil {
			return err
		}
	} else if err != nil && err != os.ErrNotExist {
		return err
	}

	for _, network := range inv.Networks {
		if !network.Managed || network.Type != "bridge" {
			continue
		}

		patch := aclConfig(network.Config, f)
		const marker = "\n# incudal-xray-dns\nserver=/#/169.254.242.2#53\n"
		raw := strings.ReplaceAll(network.Config["raw.dnsmasq"], marker, "")
		if proxyDNS {
			raw += marker
		}

		patch["raw.dnsmasq"] = raw
		changed := false
		for k, v := range patch {
			if network.Config[k] != v {
				changed = true
			}
		}

		if changed {
			{
				err = c.request(ctx, "PATCH", "/1.0/networks/"+url.PathEscape(network.Name), map[string]any{"config": patch}, nil)
				if err != nil {
					return err
				}
			}
		}
	}
	// Network-level ACLs cover new NICs immediately. Per-NIC ACLs additionally
	// enforce same-bridge traffic on Incus versions with bridge NIC ACL support.
	for _, instance := range inv.Instances {
		for name, device := range instance.ExpandedDevices {
			if device["type"] != "nic" || device["nictype"] == "routed" {
				continue
			}

			refs := aclReferences(device["security.acls"], f.Enabled)
			copyDevice := map[string]string{}
			for k, v := range device {
				copyDevice[k] = v
			}

			copyDevice["security.acls"] = refs
			configPatch := map[string]string{}
			for _, direction := range []string{"ingress", "egress"} {
				key := "security.acls.default." + direction + ".action"
				saved := "user.incudal.acl.nic." + name + "." + direction
				previous, exists := instance.Config[saved]
				if f.Enabled {
					if !exists {
						previous = device[key]
						if previous == "" {
							previous = "unset"
						}

						configPatch[saved] = previous
					}

					value := f.IngressAction
					if direction == "egress" {
						value = f.EgressAction
					}

					copyDevice[key] = value
				} else if exists {
					if previous == "unset" {
						previous = ""
					}

					copyDevice[key] = previous
					configPatch[saved] = ""
				}
			}

			changed := len(configPatch) > 0 || refs != device["security.acls"]
			for key, value := range copyDevice {
				if device[key] != value {
					changed = true
				}
			}

			if !changed {
				continue
			}
			{
				err = c.request(ctx, "PATCH", "/1.0/instances/"+url.PathEscape(instance.Name), map[string]any{"config": configPatch, "devices": map[string]any{name: copyDevice}}, nil)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}
