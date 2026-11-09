package egress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PayIncus: tenant classification must precede wg-quick/Tailscale full-tunnel rules.
const groupRuleBase = 1000
const hostRulePriority = 1100

var errPolicyRouting = errors.New("egress policy routing failed")

type policyRule struct {
	priority int
	mark     string
	table    string
	exact    bool
}

func parsePolicyRules(output string) []policyRule {
	rules := []policyRule{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		priority, err := strconv.Atoi(strings.TrimSuffix(fields[0], ":"))
		if err != nil {
			continue
		}
		rule := policyRule{priority: priority}
		// Only migrate the exact rule shape we own; selectors/actions change its meaning.
		if len(fields) == 7 && fields[1] == "from" && fields[2] == "all" && fields[3] == "fwmark" && fields[5] == "lookup" {
			rule.mark, rule.table, rule.exact = fields[4], fields[6], true
		}
		rules = append(rules, rule)
	}
	return rules
}

func policyRuleChanges(output, fwmark, table string, priority, legacy int) ([][]string, error) {
	rules := parsePolicyRules(output)
	present := false
	for _, rule := range rules {
		if rule.priority != priority {
			continue
		}
		if !rule.exact || rule.mark != fwmark || rule.table != table || present {
			return nil, fmt.Errorf("reserved routing priority already in use")
		}
		present = true
	}
	changes := [][]string{}
	if !present {
		changes = append(changes, []string{"rule", "add", "pref", strconv.Itoa(priority), "fwmark", fwmark, "lookup", table})
	}
	// Install the replacement before removing old rules, preserving fail-closed routing.
	for _, rule := range rules {
		if rule.priority == legacy && rule.exact && rule.mark == fwmark && rule.table == table {
			changes = append(changes, []string{"rule", "del", "pref", strconv.Itoa(legacy), "fwmark", fwmark, "lookup", table})
		}
	}
	return changes, nil
}

func ensurePolicyRule(ctx context.Context, family, fwmark, table string, priority, legacy int) error {
	output, err := networkCommand(ctx, "ip", family, "rule", "show").Output()
	if err != nil {
		return fmt.Errorf("%w: cannot inspect routing rules", errPolicyRouting)
	}
	changes, err := policyRuleChanges(string(output), fwmark, table, priority, legacy)
	if err != nil {
		return fmt.Errorf("%w: %v", errPolicyRouting, err)
	}
	for _, args := range changes {
		if err := run(ctx, "ip", append([]string{family}, args...)...); err != nil {
			return fmt.Errorf("%w: %v", errPolicyRouting, err)
		}
	}
	return nil
}

func removePolicyRules(ctx context.Context, family, fwmark, table string, priorities ...int) error {
	output, err := networkCommand(ctx, "ip", family, "rule", "show").Output()
	if err != nil {
		return fmt.Errorf("cannot inspect routing rules")
	}
	for _, rule := range parsePolicyRules(string(output)) {
		for _, priority := range priorities {
			if rule.priority == priority && rule.exact && rule.mark == fwmark && rule.table == table {
				if err := run(ctx, "ip", family, "rule", "del", "pref", strconv.Itoa(priority), "fwmark", fwmark, "lookup", table); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func verifyPolicyRoute(ctx context.Context, family, fwmark, device string) error {
	target := "192.0.2.1"
	if family == "-6" {
		target = "2001:db8::1"
	}
	output, err := networkCommand(ctx, "ip", family, "-j", "route", "get", target, "mark", fwmark).Output()
	if err != nil {
		return fmt.Errorf("%w: cannot verify egress routing", errPolicyRouting)
	}
	var routes []struct {
		Dev string `json:"dev"`
	}
	if json.Unmarshal(output, &routes) != nil || len(routes) != 1 || routes[0].Dev != device {
		return fmt.Errorf("%w: intercepted by another host rule", errPolicyRouting)
	}
	return nil
}
