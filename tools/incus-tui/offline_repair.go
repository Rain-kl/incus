package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	agentNetworkDir  = "/var/lib/incus/agent/network"
	systemdSystemDir = "/etc/systemd/system"
	upgradeRunDir    = "/run/incus-upgrade"
)

var (
	knownEgressUnits = []string{
		"incus-xray.service",
		"incus-network.service",
		"incus-package-guard.service",
		"incus-egress-v2.service",
	}
	egressGroupLinkPattern = regexp.MustCompile(`^ipgh([1-9]|[1-5][0-9]|6[0-3])$`)
	egressGroupKeyPattern  = regexp.MustCompile(`^p[1-9][0-9]{0,9}$`)
)

func (b *backend) hostPath(abs string) string {
	clean := filepath.Clean(abs)
	if b == nil || b.rootDir == "" {
		return clean
	}
	return filepath.Join(b.rootDir, strings.TrimPrefix(clean, string(filepath.Separator)))
}

func (b *backend) incusDataDir() string {
	if dir := os.Getenv("INCUS_DIR"); dir != "" {
		return filepath.Clean(dir)
	}
	return b.hostPath("/var/lib/incus")
}

func (b *backend) localSocketExists() bool {
	_, err := os.Stat(filepath.Join(b.incusDataDir(), "unix.socket"))
	return err == nil
}

type upgradeCheckpoint struct {
	exists bool
	phase  string
	active []string
}

func (b *backend) readUpgradeState() upgradeCheckpoint {
	dir := b.hostPath(upgradeRunDir)
	if _, err := os.Stat(dir); err != nil {
		return upgradeCheckpoint{}
	}
	cp := upgradeCheckpoint{exists: true}
	if body, err := os.ReadFile(filepath.Join(dir, "phase")); err == nil {
		cp.phase = strings.TrimSpace(string(body))
	}
	if body, err := os.ReadFile(filepath.Join(dir, "active")); err == nil {
		for _, line := range strings.Split(string(body), "\n") {
			unit := strings.TrimSpace(line)
			if unit != "" {
				cp.active = append(cp.active, unit)
			}
		}
	}
	return cp
}

func (b *backend) systemdUnitPath(unit string) string {
	return b.hostPath(filepath.Join(systemdSystemDir, unit))
}

func (b *backend) discoverEgressUnits() []string {
	seen := map[string]bool{}
	units := make([]string, 0, len(knownEgressUnits)+4)
	for _, unit := range knownEgressUnits {
		seen[unit] = true
		units = append(units, unit)
	}
	entries, err := os.ReadDir(b.hostPath(systemdSystemDir))
	if err != nil {
		return units
	}
	var dynamic []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "incus-egress-") && strings.HasSuffix(name, ".service") && !seen[name] {
			seen[name] = true
			dynamic = append(dynamic, name)
		}
	}
	sort.Strings(dynamic)
	return append(units, dynamic...)
}

type systemdDropIn struct {
	hostPath string
	label    string
}

func (b *backend) discoverIncusDropIns() ([]systemdDropIn, error) {
	var found []systemdDropIn
	var errs []error
	for _, sub := range []string{"incus.service.d", "incus-containers.service.d"} {
		dir := b.hostPath(filepath.Join(systemdSystemDir, sub))
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !strings.HasSuffix(name, ".conf") || !strings.Contains(name, "incus") {
				continue
			}
			found = append(found, systemdDropIn{
				hostPath: filepath.Join(dir, name),
				label:    filepath.Join(sub, name),
			})
		}
	}
	return found, errors.Join(errs...)
}

func (b *backend) hasEgressDropIn(subDir, confName string) bool {
	_, err := os.Stat(b.hostPath(filepath.Join(systemdSystemDir, subDir, confName)))
	return err == nil
}

type unitProbe struct {
	Name          string
	Exists        bool
	LoadState     string
	ActiveState   string
	SubState      string
	Result        string
	UnitFileState string
	JournalTail   []string
}

func (b *backend) probeUnit(ctx context.Context, unit string, withJournal bool) unitProbe {
	probe := unitProbe{Name: unit}
	if _, err := os.Stat(b.hostPath(filepath.Join(systemdSystemDir, unit))); err == nil {
		probe.Exists = true
	}
	out, err := b.run(ctx, "systemctl", "show", unit, "--property=LoadState,ActiveState,SubState,Result,UnitFileState", "--no-pager")
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok {
				continue
			}
			switch k {
			case "LoadState":
				probe.LoadState = v
			case "ActiveState":
				probe.ActiveState = v
			case "SubState":
				probe.SubState = v
			case "Result":
				probe.Result = v
			case "UnitFileState":
				probe.UnitFileState = v
			}
		}
	}
	if withJournal && (probe.ActiveState == "failed" || probe.Result == "exit-code" || probe.Result == "dependency" || (probe.Exists && probe.ActiveState != "active" && probe.ActiveState != "")) {
		if jout, jerr := b.run(ctx, "journalctl", "-u", unit, "-n", "12", "--no-pager", "-o", "cat"); jerr == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(jout)), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					probe.JournalTail = append(probe.JournalTail, line)
				}
			}
		}
	}
	return probe
}

type savedNetworkHeader struct {
	Instruction struct {
		SchemaVersion int `json:"schemaVersion"`
		Revision      int `json:"revision"`
		Egress        struct {
			Mode string `json:"mode"`
		} `json:"egress"`
		Firewall struct {
			Enabled bool `json:"enabled"`
		} `json:"firewall"`
		Groups   []json.RawMessage `json:"groups"`
		Bindings []json.RawMessage `json:"bindings"`
	} `json:"instruction"`
}

func (h savedNetworkHeader) needsRestore() bool {
	return h.Instruction.Egress.Mode != "off" || h.Instruction.Firewall.Enabled || len(h.Instruction.Groups) > 0 || len(h.Instruction.Bindings) > 0
}

type groupRegistryEntry struct {
	Key  string `json:"key"`
	Slot int    `json:"slot"`
}

type agentNetworkReport struct {
	AppliedExists    bool
	AppliedValid     bool
	AppliedErr       string
	NeedsRestore     bool
	SchemaVersion    int
	Revision         int
	EgressMode       string
	FirewallEnabled  bool
	GroupCount       int
	BindingCount     int
	DesiredExists    bool
	DesiredValid     bool
	XrayExists       bool
	XrayValid        bool
	OwnerState       string
	GroupsOwnerState string
	RegistryExists   bool
	RegistryValid    bool
	RegistryErr      string
	OrphanGroupOwner bool
	WorkerExists     bool
	WorkerExecutable bool
	ContainerCount   int
}

func checkJSONFile(path string, target any) (exists, valid bool, errText string) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, false, ""
		}
		return true, false, err.Error()
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return true, false, "文件为空"
	}
	if target == nil {
		var raw json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			return true, false, err.Error()
		}
		return true, true, ""
	}
	if err := json.Unmarshal(body, target); err != nil {
		return true, false, err.Error()
	}
	return true, true, ""
}

func checkOwnerFile(path, expected string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "missing"
		}
		return "unreadable"
	}
	if string(body) == expected {
		return "valid"
	}
	return "mismatch"
}

func (b *backend) inspectAgentNetworkState() agentNetworkReport {
	dir := b.hostPath(agentNetworkDir)
	rep := agentNetworkReport{
		OwnerState:       checkOwnerFile(filepath.Join(dir, "owner"), "incus-network-v1\n"),
		GroupsOwnerState: checkOwnerFile(filepath.Join(dir, "groups", "owner"), "incus-package-egress-v2\n"),
	}

	var saved savedNetworkHeader
	rep.AppliedExists, rep.AppliedValid, rep.AppliedErr = checkJSONFile(filepath.Join(dir, "applied.json"), &saved)
	if rep.AppliedValid {
		rep.SchemaVersion = saved.Instruction.SchemaVersion
		rep.Revision = saved.Instruction.Revision
		rep.EgressMode = saved.Instruction.Egress.Mode
		rep.FirewallEnabled = saved.Instruction.Firewall.Enabled
		rep.GroupCount = len(saved.Instruction.Groups)
		rep.BindingCount = len(saved.Instruction.Bindings)
		rep.NeedsRestore = saved.needsRestore()
	}

	rep.DesiredExists, rep.DesiredValid, _ = checkJSONFile(filepath.Join(dir, "desired.json"), nil)
	rep.XrayExists, rep.XrayValid, _ = checkJSONFile(filepath.Join(dir, "xray.json"), nil)

	var reg map[string]groupRegistryEntry
	rep.RegistryExists, rep.RegistryValid, rep.RegistryErr = checkJSONFile(filepath.Join(dir, "groups", "registry.json"), &reg)
	if rep.RegistryValid {
		used := map[int]bool{}
		for key, entry := range reg {
			if !egressGroupKeyPattern.MatchString(key) || entry.Key != key || entry.Slot < 1 || entry.Slot > 63 || used[entry.Slot] {
				rep.RegistryValid = false
				rep.RegistryErr = "slot/key 映射损坏"
				break
			}
			used[entry.Slot] = true
		}
	}

	if rep.GroupsOwnerState == "mismatch" || (rep.GroupsOwnerState != "missing" && !rep.RegistryExists) {
		rep.OrphanGroupOwner = true
	}

	workerPath := filepath.Join(dir, "network-worker")
	if info, err := os.Stat(workerPath); err == nil {
		rep.WorkerExists = true
		rep.WorkerExecutable = info.Mode()&0o111 != 0
	}

	if entries, err := os.ReadDir(filepath.Join(b.incusDataDir(), "containers")); err == nil {
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".") {
				rep.ContainerCount++
			}
		}
	}

	return rep
}

func isEgressTableID(table string) bool {
	if table == "7420" {
		return true
	}
	n, err := strconv.Atoi(table)
	return err == nil && n >= 8001 && n <= 8063
}

func isEgressRulePriority(prio int) bool {
	return prio == 1100 || prio == 17420 || (prio >= 1001 && prio <= 1063) || (prio >= 17001 && prio <= 17063)
}

func isEgressFwmark(token string) bool {
	if token == "" {
		return false
	}
	base, _, _ := strings.Cut(strings.TrimSpace(token), "/")
	val, err := strconv.ParseUint(base, 0, 32)
	if err != nil {
		return false
	}
	hi := val & 0x00ff0000
	return (hi == 0x00420000 || hi == 0x00440000) && (val&0xff0000ff) == hi
}

type parsedIPRule struct {
	Family   string
	Priority int
	Fwmark   string
	Table    string
	Raw      string
}

func parseIPRuleLine(family, line string) (parsedIPRule, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return parsedIPRule{}, false
	}
	fields := strings.Fields(trimmed)
	if len(fields) < 2 || !strings.HasSuffix(fields[0], ":") {
		return parsedIPRule{}, false
	}
	prio, err := strconv.Atoi(strings.TrimSuffix(fields[0], ":"))
	if err != nil {
		return parsedIPRule{}, false
	}
	rule := parsedIPRule{Family: family, Priority: prio, Raw: trimmed}
	for i := 1; i+1 < len(fields); i++ {
		switch fields[i] {
		case "fwmark":
			rule.Fwmark = fields[i+1]
		case "lookup", "table":
			rule.Table = fields[i+1]
		}
	}
	return rule, true
}

func isEgressPolicyRule(r parsedIPRule) bool {
	if isEgressFwmark(r.Fwmark) && (isEgressTableID(r.Table) || isEgressRulePriority(r.Priority)) {
		return true
	}
	return isEgressTableID(r.Table) && isEgressRulePriority(r.Priority)
}

func splitEgressRouteTables(output string) map[string][]string {
	tables := map[string][]string{}
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		fields := strings.Fields(trimmed)
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == "table" && isEgressTableID(fields[i+1]) {
				tables[fields[i+1]] = append(tables[fields[i+1]], trimmed)
				break
			}
		}
	}
	return tables
}

func tableContainsEgressRoutes(lines []string) bool {
	for _, line := range lines {
		fields := strings.Fields(line)
		hasUnreachable := len(fields) >= 2 && fields[0] == "unreachable" && fields[1] == "default"
		hasMetric42760 := false
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == "metric" && fields[i+1] == "42760" {
				hasMetric42760 = true
			}
			if fields[i] == "dev" && isEgressLinkName(fields[i+1]) {
				return true
			}
		}
		if hasUnreachable && hasMetric42760 {
			return true
		}
		if strings.Contains(line, "169.254.240.") || strings.Contains(line, "169.254.244.") || strings.Contains(line, "fd42:696e:6375::") {
			return true
		}
	}
	return false
}

func isEgressLinkName(name string) bool {
	return name == "igw-host" || egressGroupLinkPattern.MatchString(name)
}

func isEgressNetnsName(name string) bool {
	return name == "incus-egress" || strings.HasPrefix(name, "incus-eg-")
}

func isKnownEgressNFTTable(family, name string) bool {
	switch family + ":" + name {
	case "inet:incus_egress", "bridge:incus_package_ports", "bridge:incus_package_boot", "inet:incus_package_egress":
		return true
	}
	return false
}

func isCandidateEgressNFTTable(family, name string) bool {
	if isKnownEgressNFTTable(family, name) {
		return true
	}
	if family != "inet" && family != "bridge" {
		return false
	}
	return strings.HasSuffix(name, "_egress") || strings.HasSuffix(name, "_package_ports") || strings.HasSuffix(name, "_package_boot") || strings.HasSuffix(name, "_package_egress")
}

func nftBodyMatchesEgressFingerprint(body string) bool {
	if strings.Contains(body, `"ipg*"`) || strings.Contains(body, `"ipgh`) || strings.Contains(body, `"igw-host"`) {
		return true
	}
	if strings.Contains(body, "0x00420000") || strings.Contains(body, "0x420000") || strings.Contains(body, "0x00440000") {
		return true
	}
	if strings.Contains(body, "169.254.244.") || strings.Contains(body, "169.254.242.") || strings.Contains(body, "fd42:696e:6375::") {
		return true
	}
	return strings.Contains(body, "set ready") && strings.Contains(body, "type mark")
}

type routeTableRef struct {
	Family string
	Table  string
	Lines  []string
}

type nftTableRef struct {
	Family string
	Name   string
}

type xrayProcRef struct {
	PID  string
	Unit string
}

type kernelEgressSnapshot struct {
	OwnedRules            []parsedIPRule
	ForeignConflictRules  []parsedIPRule
	OwnedTables           []routeTableRef
	ForeignConflictTables []routeTableRef
	OwnedLinks            []string
	OwnedNetns            []string
	OwnedNFTTables        []nftTableRef
	NetnsDirs             []string
	StaleDnsmasqPIDs      []string
	StaleXrayPIDs         []string
	XrayRefs              []xrayProcRef
	ProbeErrors           []string
}

func (k kernelEgressSnapshot) totalOwned() int {
	return len(k.OwnedRules) + len(k.OwnedTables) + len(k.OwnedLinks) + len(k.OwnedNetns) + len(k.OwnedNFTTables) + len(k.NetnsDirs)
}

func (k kernelEgressSnapshot) totalConflicts() int {
	return len(k.ForeignConflictRules) + len(k.ForeignConflictTables)
}

func (b *backend) inspectKernelEgress(ctx context.Context) kernelEgressSnapshot {
	var snap kernelEgressSnapshot
	ownedTableKeys := map[string]bool{}

	for _, family := range []string{"-4", "-6"} {
		out, err := b.run(ctx, "ip", family, "rule", "show")
		if err != nil {
			snap.ProbeErrors = append(snap.ProbeErrors, "ip "+family+" rule: "+err.Error())
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			rule, ok := parseIPRuleLine(family, line)
			if !ok {
				continue
			}
			if isEgressPolicyRule(rule) {
				snap.OwnedRules = append(snap.OwnedRules, rule)
				if isEgressTableID(rule.Table) {
					ownedTableKeys[family+":"+rule.Table] = true
				}
			} else if isEgressTableID(rule.Table) || isEgressRulePriority(rule.Priority) {
				snap.ForeignConflictRules = append(snap.ForeignConflictRules, rule)
			}
		}
	}

	for _, family := range []string{"-4", "-6"} {
		out, err := b.run(ctx, "ip", family, "route", "show", "table", "all")
		if err != nil {
			snap.ProbeErrors = append(snap.ProbeErrors, "ip "+family+" route: "+err.Error())
			continue
		}
		tables := splitEgressRouteTables(string(out))
		keys := make([]string, 0, len(tables))
		for table := range tables {
			keys = append(keys, table)
		}
		sort.Strings(keys)
		for _, table := range keys {
			lines := tables[table]
			ref := routeTableRef{Family: family, Table: table, Lines: lines}
			if ownedTableKeys[family+":"+table] || tableContainsEgressRoutes(lines) {
				snap.OwnedTables = append(snap.OwnedTables, ref)
			} else {
				snap.ForeignConflictTables = append(snap.ForeignConflictTables, ref)
			}
		}
	}

	if out, err := b.run(ctx, "ip", "-o", "link", "show"); err != nil {
		snap.ProbeErrors = append(snap.ProbeErrors, "ip link: "+err.Error())
	} else {
		seen := map[string]bool{}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			name := strings.TrimSuffix(fields[1], ":")
			name, _, _ = strings.Cut(name, "@")
			if isEgressLinkName(name) && !seen[name] {
				seen[name] = true
				snap.OwnedLinks = append(snap.OwnedLinks, name)
			}
		}
		sort.Strings(snap.OwnedLinks)
	}

	if out, err := b.run(ctx, "ip", "netns", "list"); err != nil {
		snap.ProbeErrors = append(snap.ProbeErrors, "ip netns: "+err.Error())
	} else {
		seen := map[string]bool{}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if isEgressNetnsName(fields[0]) && !seen[fields[0]] {
				seen[fields[0]] = true
				snap.OwnedNetns = append(snap.OwnedNetns, fields[0])
			}
		}
		sort.Strings(snap.OwnedNetns)
	}

	if entries, err := os.ReadDir(b.hostPath("/etc/netns")); err == nil {
		for _, entry := range entries {
			if isEgressNetnsName(entry.Name()) {
				snap.NetnsDirs = append(snap.NetnsDirs, entry.Name())
			}
		}
		sort.Strings(snap.NetnsDirs)
	}

	if out, err := b.run(ctx, "nft", "list", "tables"); err != nil {
		snap.ProbeErrors = append(snap.ProbeErrors, "nft list tables: "+err.Error())
	} else {
		seen := map[string]bool{}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 3 || fields[0] != "table" {
				continue
			}
			family, name := fields[1], fields[2]
			key := family + ":" + name
			if seen[key] || !isCandidateEgressNFTTable(family, name) {
				continue
			}
			if isKnownEgressNFTTable(family, name) {
				seen[key] = true
				snap.OwnedNFTTables = append(snap.OwnedNFTTables, nftTableRef{Family: family, Name: name})
				continue
			}
			if body, berr := b.run(ctx, "nft", "list", "table", family, name); berr == nil && nftBodyMatchesEgressFingerprint(string(body)) {
				seen[key] = true
				snap.OwnedNFTTables = append(snap.OwnedNFTTables, nftTableRef{Family: family, Name: name})
			}
		}
	}

	if out, err := b.run(ctx, "ps", "-eo", "pid=,comm=,args="); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			pid, err := strconv.Atoi(fields[0])
			if err != nil || pid <= 0 || pid == os.Getpid() {
				continue
			}
			comm := fields[1]
			args := strings.Join(fields[2:], " ")
			if comm == "dnsmasq" && (strings.Contains(args, "--interface="+panelBridge) || strings.Contains(args, "--interface "+panelBridge)) {
				snap.StaleDnsmasqPIDs = append(snap.StaleDnsmasqPIDs, fields[0])
			} else if (strings.Contains(args, agentNetworkDir) || strings.Contains(args, b.hostPath(agentNetworkDir))) && (comm == "xray" || strings.HasPrefix(filepath.Base(fields[2]), "xray")) {
				snap.StaleXrayPIDs = append(snap.StaleXrayPIDs, fields[0])
				snap.XrayRefs = append(snap.XrayRefs, xrayProcRef{PID: fields[0], Unit: xrayOwnerUnit(args)})
			}
		}
	}

	return snap
}

func xrayOwnerUnit(args string) string {
	if idx := strings.Index(args, "/groups/"); idx >= 0 {
		rest := args[idx+len("/groups/"):]
		if key, after, ok := strings.Cut(rest, "/"); ok && (after == "xray.json" || strings.HasPrefix(after, "xray.json ")) && egressGroupKeyPattern.MatchString(key) {
			return "incus-egress-" + key + ".service"
		}
	}
	return "incus-xray.service"
}

func (b *backend) processBelongsToUnit(pid, unit string, unitActive bool) bool {
	if !unitActive || unit == "" {
		return false
	}
	body, err := os.ReadFile(b.hostPath(filepath.Join("/proc", pid, "cgroup")))
	if err != nil {
		// Fake runners or hosts without /proc/<pid>/cgroup fall back to systemd unit status.
		return true
	}
	needle := "/" + unit
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, needle) || strings.Contains(trimmed, needle+"/") {
			return true
		}
	}
	return false
}

func (b *backend) classifyEgressProcesses(snap kernelEgressSnapshot, unitActive map[string]bool) (staleDnsmasq, activeDnsmasq, staleXray, activeXray []string) {
	incusActive := unitActive["incus.service"]
	for _, pid := range snap.StaleDnsmasqPIDs {
		if b.processBelongsToUnit(pid, "incus.service", incusActive) {
			activeDnsmasq = append(activeDnsmasq, pid)
		} else {
			staleDnsmasq = append(staleDnsmasq, pid)
		}
	}
	for _, ref := range snap.XrayRefs {
		if b.processBelongsToUnit(ref.PID, ref.Unit, unitActive[ref.Unit]) {
			activeXray = append(activeXray, ref.PID)
		} else {
			staleXray = append(staleXray, ref.PID)
		}
	}
	return staleDnsmasq, activeDnsmasq, staleXray, activeXray
}

func (b *backend) cleanupProcessList(ctx context.Context, label string, pids []string) {
	for _, pidStr := range pids {
		logOperation(ctx, "检测到残留 %s 进程 (pid=%s)，尝试发送 SIGTERM", label, pidStr)
		if _, err := b.run(ctx, "kill", "-TERM", pidStr); err != nil {
			logOperation(ctx, "结束残留 %s 进程 %s 失败（可忽略）：%v", label, pidStr, err)
		}
	}
	if len(pids) > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (b *backend) cleanupStaleAgentXray(ctx context.Context, pids []string) {
	b.cleanupProcessList(ctx, "出口 Xray", pids)
}

func (b *backend) cleanupKernelEgressState(ctx context.Context) (kernelEgressSnapshot, error) {
	snap := b.inspectKernelEgress(ctx)
	var errs []error

	for _, rule := range snap.OwnedRules {
		logOperation(ctx, "清理残留策略路由规则：%s rule pref %d (%s)", rule.Family, rule.Priority, rule.Raw)
		prioStr := strconv.Itoa(rule.Priority)
		var delErr error
		if rule.Fwmark != "" && rule.Table != "" {
			_, delErr = b.run(ctx, "ip", rule.Family, "rule", "del", "priority", prioStr, "fwmark", rule.Fwmark, "lookup", rule.Table)
		} else if rule.Table != "" {
			_, delErr = b.run(ctx, "ip", rule.Family, "rule", "del", "priority", prioStr, "lookup", rule.Table)
		} else {
			_, delErr = b.run(ctx, "ip", rule.Family, "rule", "del", "priority", prioStr)
		}
		if delErr != nil {
			if _, fallbackErr := b.run(ctx, "ip", rule.Family, "rule", "del", "priority", prioStr); fallbackErr != nil {
				errs = append(errs, fmt.Errorf("删除 %s rule %d：%w", rule.Family, rule.Priority, fallbackErr))
			}
		}
	}
	for _, tbl := range snap.OwnedTables {
		logOperation(ctx, "清空残留出口路由表：%s table %s", tbl.Family, tbl.Table)
		if _, err := b.run(ctx, "ip", tbl.Family, "route", "flush", "table", tbl.Table); err != nil {
			errs = append(errs, fmt.Errorf("清空 %s table %s：%w", tbl.Family, tbl.Table, err))
		}
	}
	for _, iface := range snap.OwnedLinks {
		logOperation(ctx, "删除残留出口虚拟网卡：%s", iface)
		if _, err := b.run(ctx, "ip", "link", "del", iface); err != nil {
			errs = append(errs, fmt.Errorf("删除接口 %s：%w", iface, err))
		}
	}
	for _, ns := range snap.OwnedNetns {
		logOperation(ctx, "删除残留出口网络命名空间：%s", ns)
		if _, err := b.run(ctx, "ip", "netns", "del", ns); err != nil {
			errs = append(errs, fmt.Errorf("删除 netns %s：%w", ns, err))
		}
	}
	for _, dirName := range snap.NetnsDirs {
		if err := os.RemoveAll(b.hostPath(filepath.Join("/etc/netns", dirName))); err != nil {
			errs = append(errs, fmt.Errorf("清理 /etc/netns/%s：%w", dirName, err))
		}
	}
	for _, tbl := range snap.OwnedNFTTables {
		logOperation(ctx, "删除残留出口防火墙表：nft %s %s", tbl.Family, tbl.Name)
		if _, err := b.run(ctx, "nft", "delete", "table", tbl.Family, tbl.Name); err != nil {
			errs = append(errs, fmt.Errorf("删除 nft 表 %s %s：%w", tbl.Family, tbl.Name, err))
		}
	}

	return snap, errors.Join(errs...)
}

func (b *backend) removeEgressUnitsAndDropIns(ctx context.Context) error {
	var errs []error
	units := b.discoverEgressUnits()
	for _, unit := range units {
		_ = b.manageService(ctx, unit, "stop")
		_, _ = b.run(ctx, "systemctl", "disable", unit)
		path := b.systemdUnitPath(unit)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("删除 %s：%w", unit, err))
		}
	}
	dropIns, err := b.discoverIncusDropIns()
	if err != nil {
		errs = append(errs, err)
	}
	for _, item := range dropIns {
		if err := os.Remove(item.hostPath); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("删除 drop-in %s：%w", item.label, err))
		}
	}
	if _, err := b.run(ctx, "systemctl", "daemon-reload"); err != nil {
		errs = append(errs, fmt.Errorf("重载 systemd：%w", err))
	}
	_, _ = b.run(ctx, "systemctl", "reset-failed")
	return errors.Join(errs...)
}

func (b *backend) resumeUpgradeCheckpoint(ctx context.Context, cp upgradeCheckpoint) error {
	if !cp.exists {
		return nil
	}
	if cp.phase != "restore" {
		return fmt.Errorf("检测到未完成的升级阶段 phase=%s（并非 restore），拒绝自动清理 /run/incus-upgrade", cp.phase)
	}
	seen := map[string]bool{}
	for _, unit := range cp.active {
		if unit == "" || unit == "incus.service" || seen[unit] {
			continue
		}
		seen[unit] = true
		logOperation(ctx, "恢复升级前原运行服务：%s", unit)
		if err := b.manageService(ctx, unit, "start"); err != nil {
			return fmt.Errorf("恢复原运行服务 %s 失败：%w", unit, err)
		}
	}
	if err := os.RemoveAll(b.hostPath(upgradeRunDir)); err != nil {
		return fmt.Errorf("清理升级恢复点 %s 失败：%w", upgradeRunDir, err)
	}
	logOperation(ctx, "已恢复升级暂停点并清理 %s", upgradeRunDir)
	return nil
}

// offlineStatusDiagnostics appends compact root-cause hints to the home screen
// only when systemd/API state indicates Incus cannot respond normally.
func (b *backend) offlineStatusDiagnostics(ctx context.Context) []string {
	var hints []string
	for _, unit := range []string{"incus-network.service", "incus-package-guard.service"} {
		probe := b.probeUnit(ctx, unit, true)
		if !probe.Exists && (probe.LoadState == "" || probe.LoadState == "not-found") {
			continue
		}
		hints = append(hints, fmt.Sprintf("阻塞诊断 %s: active=%s sub=%s result=%s", unit, probe.ActiveState, probe.SubState, probe.Result))
		if len(probe.JournalTail) > 0 {
			hints = append(hints, "  最近日志: "+probe.JournalTail[len(probe.JournalTail)-1])
		}
	}
	if !b.localSocketExists() {
		hints = append(hints, "本地套接字未就绪："+filepath.Join(b.incusDataDir(), "unix.socket"))
	}
	netRep := b.inspectAgentNetworkState()
	if netRep.OrphanGroupOwner {
		hints = append(hints, "状态异常：发现孤立 groups/owner 文件（缺少 groups/registry.json）")
	}
	if netRep.AppliedExists && !netRep.AppliedValid {
		hints = append(hints, "状态异常：applied.json 损坏（"+netRep.AppliedErr+"）")
	}
	if len(hints) > 0 {
		hints = append(hints, "离线处理建议：进入“修复 → 故障诊断（离线）”查看详情，或运行“恢复被阻塞的服务（离线自救）”")
	}
	return hints
}

func (b *backend) diagnoseNode(ctx context.Context) ([]string, error) {
	logOperation(ctx, "开始离线只读故障诊断…")
	var lines []string
	lines = append(lines, "=== 离线故障诊断（只读，未修改系统） ===")

	var issues []string
	var suggestions []string
	addSuggestion := func(text string) {
		for _, item := range suggestions {
			if item == text {
				return
			}
		}
		suggestions = append(suggestions, text)
	}

	unitActive := map[string]bool{}
	for _, unit := range b.discoverEgressUnits() {
		probe := b.probeUnit(ctx, unit, true)
		if probe.ActiveState == "active" {
			unitActive[unit] = true
		}
		if !probe.Exists && probe.LoadState == "not-found" && probe.ActiveState != "failed" {
			continue
		}
		lines = append(lines, fmt.Sprintf("单元 %s: load=%s active=%s sub=%s result=%s", unit, probe.LoadState, probe.ActiveState, probe.SubState, probe.Result))
		if probe.ActiveState == "failed" || probe.Result == "exit-code" || probe.Result == "dependency" {
			issues = append(issues, fmt.Sprintf("%s 失败 (result=%s)", unit, probe.Result))
		}
		for _, jline := range probe.JournalTail {
			lines = append(lines, "  journal: "+jline)
		}
	}
	for _, unit := range []string{"incus.service", "incus-agent.service"} {
		probe := b.probeUnit(ctx, unit, false)
		if probe.ActiveState == "active" {
			unitActive[unit] = true
		}
		lines = append(lines, fmt.Sprintf("核心 %s: active=%s sub=%s result=%s", unit, probe.ActiveState, probe.SubState, probe.Result))
		if probe.ActiveState != "active" {
			issues = append(issues, fmt.Sprintf("%s 未运行 (%s/%s)", unit, probe.ActiveState, probe.Result))
		}
	}

	if b.localSocketExists() {
		lines = append(lines, "本地 API socket: 就绪 ("+filepath.Join(b.incusDataDir(), "unix.socket")+")")
	} else {
		lines = append(lines, "本地 API socket: 缺失 ("+filepath.Join(b.incusDataDir(), "unix.socket")+")")
	}

	dropIns, _ := b.discoverIncusDropIns()
	if len(dropIns) > 0 {
		names := make([]string, 0, len(dropIns))
		for _, d := range dropIns {
			names = append(names, d.label)
		}
		lines = append(lines, "Systemd 启动依赖 drop-in: "+strings.Join(names, ", "))
	}

	cp := b.readUpgradeState()
	if cp.exists {
		lines = append(lines, fmt.Sprintf("升级恢复点 %s: phase=%s active=[%s]", upgradeRunDir, cp.phase, strings.Join(cp.active, ", ")))
		if cp.phase == "restore" {
			issues = append(issues, "升级卡在 phase=restore 恢复阶段")
			addSuggestion("先恢复 incus-network/incus，随后自动续跑 phase=restore 恢复并清理 /run/incus-upgrade")
		} else {
			issues = append(issues, "升级停留在中间阶段 phase="+cp.phase)
			addSuggestion("请勿直接清空 " + upgradeRunDir + "，先确认 apt/dpkg 是否已结束")
		}
	}

	rep := b.inspectAgentNetworkState()
	lines = append(lines, fmt.Sprintf("本地实例目录数: %d · owner=%s · groups/owner=%s", rep.ContainerCount, rep.OwnerState, rep.GroupsOwnerState))
	if rep.AppliedExists {
		if rep.AppliedValid {
			lines = append(lines, fmt.Sprintf("applied.json: valid · schema=%d rev=%d mode=%s firewall=%t groups=%d bindings=%d needsRestore=%t",
				rep.SchemaVersion, rep.Revision, rep.EgressMode, rep.FirewallEnabled, rep.GroupCount, rep.BindingCount, rep.NeedsRestore))
			if rep.NeedsRestore && !rep.WorkerExists {
				issues = append(issues, "applied.json 需要启动恢复，但 network-worker 缺失")
				addSuggestion("使用“清除网络阻塞状态（保留实例）”解除启动依赖锁死，或补装节点包")
			}
		} else {
			lines = append(lines, "applied.json: 损坏 ("+rep.AppliedErr+")")
			issues = append(issues, "applied.json 文件损坏导致 incus-network 无法恢复")
			addSuggestion("使用“清除网络阻塞状态（保留实例）”清理损坏的出口配置并保住实例数据")
		}
	} else {
		lines = append(lines, "applied.json: 不存在")
	}
	if rep.RegistryExists && !rep.RegistryValid {
		lines = append(lines, "groups/registry.json: 损坏 ("+rep.RegistryErr+")")
		issues = append(issues, "groups/registry.json 损坏")
	}
	if rep.OrphanGroupOwner {
		lines = append(lines, "groups/owner: 异常（存在 groups/owner 但缺少有效 groups/registry.json）")
		issues = append(issues, "存在孤立或不一致的 groups/owner 声明")
	}
	if rep.DesiredExists && !rep.DesiredValid {
		lines = append(lines, "desired.json: JSON 损坏")
		issues = append(issues, "desired.json 损坏")
	}
	if rep.XrayExists && !rep.XrayValid {
		lines = append(lines, "xray.json: JSON 损坏")
		issues = append(issues, "xray.json 损坏")
	}

	snap := b.inspectKernelEgress(ctx)
	staleDnsmasq, activeDnsmasq, staleXray, activeXray := b.classifyEgressProcesses(snap, unitActive)
	kernelEgressActive := unitActive["incus-network.service"] && rep.AppliedValid && rep.NeedsRestore && !rep.OrphanGroupOwner && (!rep.RegistryExists || rep.RegistryValid)
	resourceLabel := "内核残留出口资源"
	if kernelEgressActive {
		resourceLabel = "当前活动出口资源"
	}
	procSummary := fmt.Sprintf(" · 残留进程(dnsmasq=%d, xray=%d)", len(staleDnsmasq), len(staleXray))
	if len(staleDnsmasq) == 0 && len(staleXray) == 0 && (len(activeDnsmasq) > 0 || len(activeXray) > 0) {
		procSummary = fmt.Sprintf(" · 托管进程(dnsmasq=%d, xray=%d)", len(activeDnsmasq), len(activeXray))
	}
	lines = append(lines, fmt.Sprintf("%s: ip-rule=%d route-table=%d veth=%d netns=%d nft-table=%d%s",
		resourceLabel, len(snap.OwnedRules), len(snap.OwnedTables), len(snap.OwnedLinks), len(snap.OwnedNetns), len(snap.OwnedNFTTables), procSummary))
	if !kernelEgressActive && snap.totalOwned() > 0 {
		issues = append(issues, "内核或进程中存在未清理的残留出口资源")
		addSuggestion("运行“恢复被阻塞的服务（离线自救）”清理残留内核表/规则/防火墙与孤立状态并重启服务")
	} else if len(staleDnsmasq) > 0 || len(staleXray) > 0 {
		issues = append(issues, "存在未受 systemd 托管的残留进程 (dnsmasq/xray)")
		addSuggestion("运行“恢复被阻塞的服务（离线自救）”清理残留内核表/规则/防火墙与孤立状态并重启服务")
	}
	for _, r := range snap.ForeignConflictRules {
		lines = append(lines, "非出口占用策略规则: "+r.Raw)
		issues = append(issues, fmt.Sprintf("保留优先级/路由表被非出口规则占用 (%s pref=%d table=%s)", r.Family, r.Priority, r.Table))
	}
	for _, tbl := range snap.ForeignConflictTables {
		lines = append(lines, fmt.Sprintf("非出口占用路由表: %s table %s (%d 条路由)", tbl.Family, tbl.Table, len(tbl.Lines)))
		issues = append(issues, fmt.Sprintf("保留路由表 %s table %s 被外部规则占用", tbl.Family, tbl.Table))
	}

	if len(issues) == 0 {
		lines = append(lines, "结论: 未发现明显阻塞残留；若服务仍异常，请按 l 查看日志。")
	} else {
		lines = append(lines, "发现问题:")
		for _, item := range issues {
			lines = append(lines, "  - "+item)
		}
		if len(suggestions) > 0 {
			lines = append(lines, "建议操作:")
			for _, item := range suggestions {
				lines = append(lines, "  * "+item)
			}
		} else {
			lines = append(lines, "建议操作: 优先尝试“恢复被阻塞的服务（离线自救）”；若仍失败则使用“清除网络阻塞状态（保留实例）”。")
		}
	}

	for _, line := range lines {
		logOperation(ctx, "%s", line)
	}
	return lines, nil
}

func (b *backend) cleanupStaleBridgeAndEgressProcesses(ctx context.Context, snap kernelEgressSnapshot) {
	incusProbe := b.probeUnit(ctx, "incus.service", false)
	staleDnsmasq, _, _, _ := b.classifyEgressProcesses(snap, map[string]bool{
		"incus.service": incusProbe.ActiveState == "active",
	})
	b.cleanupProcessList(ctx, panelBridge+" dnsmasq", staleDnsmasq)
	b.cleanupStaleAgentXray(ctx, snap.StaleXrayPIDs)
}

func (b *backend) recoverBlockedServices(ctx context.Context) error {
	logOperation(ctx, "开始执行离线自救（保留实例、存储池、数据库与面板接入凭据）…")

	cp := b.readUpgradeState()
	if cp.exists && cp.phase != "" && cp.phase != "restore" {
		return fmt.Errorf("检测到升级正处于 phase=%s 阶段，为避免损坏包或数据库，拒绝在非 restore 阶段执行自救", cp.phase)
	}

	for _, unit := range b.discoverEgressUnits() {
		if err := b.manageService(ctx, unit, "stop"); err != nil {
			logOperation(ctx, "停止 %s 未完成（可忽略）：%v", unit, err)
		}
	}
	_, _ = b.run(ctx, "systemctl", "reset-failed")

	if lockPath := b.hostPath("/var/lib/incus-tui/repairs/bridge.lock"); lockPath != "" {
		if f, err := acquireRepairLock(filepath.Dir(lockPath)); err == nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}

	snap := b.inspectKernelEgress(ctx)
	b.cleanupStaleBridgeAndEgressProcesses(ctx, snap)

	cleanedSnap, cleanErr := b.cleanupKernelEgressState(ctx)
	if cleanErr != nil {
		return fmt.Errorf("清理内核残留出口状态失败：%w", cleanErr)
	}
	if cleanedSnap.totalConflicts() > 0 {
		conflictDesc := ""
		if len(cleanedSnap.ForeignConflictTables) > 0 {
			conflictDesc = cleanedSnap.ForeignConflictTables[0].Family + " table " + cleanedSnap.ForeignConflictTables[0].Table
		} else if len(cleanedSnap.ForeignConflictRules) > 0 {
			r := cleanedSnap.ForeignConflictRules[0]
			conflictDesc = fmt.Sprintf("%s pref %d", r.Family, r.Priority)
		}
		return fmt.Errorf("发现非出口规则占用了保留路由表/优先级（如 %s），拒绝自动删除他人配置；请手动清理后重试，或选择“清除网络阻塞状态（保留实例）”", conflictDesc)
	}

	rep := b.inspectAgentNetworkState()
	netDir := b.hostPath(agentNetworkDir)
	if rep.DesiredExists && !rep.DesiredValid {
		logOperation(ctx, "删除损坏的 desired.json 缓存文件")
		_ = os.Remove(filepath.Join(netDir, "desired.json"))
	}
	if rep.XrayExists && !rep.XrayValid {
		logOperation(ctx, "删除损坏的 xray.json 缓存文件")
		_ = os.Remove(filepath.Join(netDir, "xray.json"))
	}
	if rep.RegistryExists && !rep.RegistryValid {
		logOperation(ctx, "检测到 groups/registry.json 损坏（%s），清理损坏的组分配状态以便重建", rep.RegistryErr)
		_ = os.Remove(filepath.Join(netDir, "groups", "registry.json"))
		_ = os.Remove(filepath.Join(netDir, "groups", "owner"))
	} else if rep.OrphanGroupOwner {
		logOperation(ctx, "检测到孤立或冲突的 groups/owner，删除以便 network-worker 重新声明")
		_ = os.Remove(filepath.Join(netDir, "groups", "owner"))
	}
	if rep.OwnerState == "mismatch" {
		logOperation(ctx, "检测到损坏的 owner 文件，已清理以便重建")
		_ = os.Remove(filepath.Join(netDir, "owner"))
	}

	if rep.AppliedExists && !rep.AppliedValid {
		return fmt.Errorf("applied.json 已损坏（%s），无法安全按原策略恢复；请使用“清除网络阻塞状态（保留实例）”解除启动锁死", rep.AppliedErr)
	}

	if !rep.AppliedExists || !rep.NeedsRestore {
		logOperation(ctx, "未发现需要开机阻断恢复的出口策略，解除残余启动依赖")
		if err := b.removeEgressUnitsAndDropIns(ctx); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(netDir, "boot.nft")); err == nil {
			_ = os.Remove(filepath.Join(netDir, "boot.nft"))
		}
	} else {
		if !rep.WorkerExists || !rep.WorkerExecutable {
			return fmt.Errorf("applied.json 需要恢复出口策略，但 %s 缺失或不可执行；请使用“清除网络阻塞状态（保留实例）”解除阻塞", filepath.Join(agentNetworkDir, "network-worker"))
		}
		if rep.BindingCount > 0 {
			if guard := b.probeUnit(ctx, "incus-package-guard.service", false); guard.Exists {
				logOperation(ctx, "启动前置包守卫 incus-package-guard.service…")
				_, _ = b.run(ctx, "systemctl", "reset-failed", "incus-package-guard.service")
				if err := b.manageService(ctx, "incus-package-guard.service", "start"); err != nil {
					return fmt.Errorf("启动 incus-package-guard.service 失败：%w", err)
				}
			}
		} else if guard := b.probeUnit(ctx, "incus-package-guard.service", false); guard.Exists {
			// Bindings=0 means no tenant NIC needs pre-boot lock; remove stale guard drop-in if left over.
			_ = os.Remove(b.systemdUnitPath("incus-package-guard.service"))
			_ = os.Remove(b.hostPath(filepath.Join(systemdSystemDir, "incus.service.d", "incus-package-guard.conf")))
			_ = os.Remove(b.hostPath(filepath.Join(systemdSystemDir, "incus-containers.service.d", "incus-package-guard.conf")))
			_, _ = b.run(ctx, "systemctl", "daemon-reload")
		}

		netProbe := b.probeUnit(ctx, "incus-network.service", false)
		if netProbe.Exists || b.hasEgressDropIn("incus.service.d", "incus-network.conf") {
			logOperation(ctx, "恢复网络出口状态（incus-network.service）…")
			_, _ = b.run(ctx, "systemctl", "reset-failed", "incus-network.service")
			if err := b.manageService(ctx, "incus-network.service", "start"); err != nil {
				probe := b.probeUnit(ctx, "incus-network.service", true)
				reason := strings.Join(probe.JournalTail, " | ")
				if reason != "" {
					return fmt.Errorf("启动 incus-network.service 失败：%w（日志：%s）", err, reason)
				}
				return fmt.Errorf("启动 incus-network.service 失败：%w", err)
			}
		}
	}

	logOperation(ctx, "启动 incus.service…")
	_, _ = b.run(ctx, "systemctl", "reset-failed", "incus.service", "incus-agent.service")
	if err := b.manageService(ctx, "incus.service", "start"); err != nil {
		return fmt.Errorf("启动 incus.service 失败：%w", err)
	}

	if cp.exists {
		if err := b.resumeUpgradeCheckpoint(ctx, cp); err != nil {
			return err
		}
	} else if _, err := os.Stat(b.hostPath("/etc/incus/agent/config.yaml")); err == nil {
		logOperation(ctx, "启动 incus-agent.service…")
		if err := b.manageService(ctx, "incus-agent.service", "start"); err != nil {
			logOperation(ctx, "启动 incus-agent.service 未成功（可稍后检查）：%v", err)
		}
	}

	if !b.localSocketExists() {
		logOperation(ctx, "等待本地 Incus socket 就绪…")
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) && !b.localSocketExists() {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(300 * time.Millisecond):
			}
		}
	}
	if b.localSocketExists() {
		logOperation(ctx, "Incus 本地 API socket 已就绪，服务恢复完成")
	} else {
		logOperation(ctx, "incus.service 已启动，但 unix.socket 尚未出现，请检查 status / journalctl")
	}
	return nil
}

func (b *backend) clearNetworkLockout(ctx context.Context) error {
	logOperation(ctx, "开始强制清除网络阻塞状态（严格保留实例、存储池、数据库、镜像与接入凭据）…")

	cp := b.readUpgradeState()
	if cp.exists && cp.phase != "" && cp.phase != "restore" {
		return fmt.Errorf("检测到升级处于 phase=%s 阶段，拒绝清除网络状态以免干扰进行中的升级", cp.phase)
	}

	if err := b.removeEgressUnitsAndDropIns(ctx); err != nil {
		return err
	}

	snap := b.inspectKernelEgress(ctx)
	b.cleanupStaleBridgeAndEgressProcesses(ctx, snap)
	if _, err := b.cleanupKernelEgressState(ctx); err != nil {
		logOperation(ctx, "清理部分内核出口残留遇到异常（已解除 systemd 锁，将继续启动 Incus）：%v", err)
	}

	netDir := b.hostPath(agentNetworkDir)
	for _, rel := range []string{"desired.json", "applied.json", "xray.json", "owner", "boot.nft"} {
		path := filepath.Join(netDir, rel)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除 %s 失败：%w", rel, err)
		}
	}
	if err := os.RemoveAll(filepath.Join(netDir, "groups")); err != nil {
		return fmt.Errorf("清理 agent/network/groups 失败：%w", err)
	}

	logOperation(ctx, "启动 incus.service…")
	if err := b.manageService(ctx, "incus.service", "start"); err != nil {
		return fmt.Errorf("启动 incus.service 失败：%w", err)
	}

	if cp.exists {
		if err := b.resumeUpgradeCheckpoint(ctx, cp); err != nil {
			return err
		}
	} else if _, err := os.Stat(b.hostPath("/etc/incus/agent/config.yaml")); err == nil {
		logOperation(ctx, "启动 incus-agent.service…")
		_ = b.manageService(ctx, "incus-agent.service", "start")
	}

	logOperation(ctx, "网络阻塞状态已清除，Incus 已启动；连接器将重新接收面板下发的策略")
	return nil
}
