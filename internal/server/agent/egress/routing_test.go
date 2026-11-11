package egress

import (
	"reflect"
	"strings"
	"testing"
)

func TestPolicyRuleMigration(t *testing.T) {
	legacy := "17001: from all fwmark 0x420100/0xffff00 lookup 8001\n"
	wg := "0: from all lookup local\n5208: from all lookup main suppress_prefixlength 0\n5209: not from all fwmark 0xca6c lookup 51820\n"
	add := []string{"rule", "add", "pref", "1001", "fwmark", "0x420100/0xffff00", "lookup", "8001"}
	del := []string{"rule", "del", "pref", "17001", "fwmark", "0x420100/0xffff00", "lookup", "8001"}
	for _, tc := range []struct {
		name, output string
		want         [][]string
	}{
		{"new", wg, [][]string{add}},
		{"upgrade", wg + legacy, [][]string{add, del}},
		{"partial upgrade", wg + legacy + "1001: from all fwmark 0x420100/0xffff00 lookup 8001\n", [][]string{del}},
		{"idempotent", wg + "1001: from all fwmark 0x420100/0xffff00 lookup 8001\n", [][]string{}},
		{"duplicate legacy", wg + legacy + legacy, [][]string{add, del, del}},
		{"foreign legacy", wg + "17001: from all fwmark 0x42/0xff lookup 9999\n", [][]string{add}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := policyRuleChanges(tc.output, "0x420100/0xffff00", "8001", 1001, 17001)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("changes=%v err=%v want=%v", got, err, tc.want)
			}
		})
	}
}

func TestPolicyRuleRejectsConflicts(t *testing.T) {
	for _, output := range []string{
		"1001: from all lookup 9999\n",
		"1001: from 10.0.0.0/8 fwmark 0x420100/0xffff00 lookup 8001\n",
		"1001: from all fwmark 0x420100/0xffff00 lookup 8001 suppress_prefixlength 0\n",
		strings.Repeat("1001: from all fwmark 0x420100/0xffff00 lookup 8001\n", 2),
	} {
		changes, err := policyRuleChanges(output, "0x420100/0xffff00", "8001", 1001, 17001)
		if err == nil || len(changes) != 0 {
			t.Fatalf("conflict produced mutations: %v %v", changes, err)
		}
	}
}

func TestHostPolicyRuleMigration(t *testing.T) {
	for _, output := range []string{"", "17420: from all fwmark 0x420000/0xff0000 lookup 7420\n"} {
		changes, err := policyRuleChanges(output, mark+"/"+mask, "7420", hostRulePriority, 17420)
		if err != nil || len(changes) == 0 || changes[0][3] != "1100" {
			t.Fatalf("host route migration failed: %v %v", changes, err)
		}
	}
	if groupRuleBase+63 >= hostRulePriority || hostRulePriority >= 5208 {
		t.Fatal("package/host routing must precede full-tunnel rules")
	}
}
