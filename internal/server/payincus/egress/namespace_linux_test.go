package egress

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in destructive integration fixture: run only inside the disposable test
// container, under an actual systemd service with the installed Agent's sandbox.
// The sentinel is created by the fixture runner, never by an ordinary test run.
func TestSystemdNamespaceStartup(t *testing.T) {
	mode := os.Getenv("INCUDAL_SYSTEMD_NAMESPACE_TEST")
	if mode == "" {
		t.Skip("disposable systemd test container required")
	}

	if os.Geteuid() != 0 {
		t.Fatal("fixture requires container root")
	}
	{
		_, err := os.Stat("/run/incudal-systemd-test-authorized")
		if err != nil {
			t.Fatal("fixture sentinel missing")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := func(name string, args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("fixture %s failed: %v\n%s", name, err, out)
		}

		return string(out)
	}

	if mode == "shadow" {
		command("mount", "-t", "tmpfs", "tmpfs", "/run/netns")
		{
			err := ensureHostNamespace(ctx)
			if err != nil {
				t.Fatal(err)
			}
		}
		{
			err := run(ctx, "ip", "link", "add", "igw-test-host", "type", "veth", "peer", "name", "igw-test-ns", "netns", namespace)
			if err != nil {
				t.Fatal(err)
			}
		}
		{
			err := run(ctx, "ip", "-n", namespace, "link", "show", "igw-test-ns")
			if err != nil {
				t.Fatal(err)
			}
		}
		{
			err := run(ctx, "ip", "link", "delete", "igw-test-host")
			if err != nil {
				t.Fatal(err)
			}
		}

		return
	}

	if mode == "reuse" {
		{
			err := ensureHostNamespace(ctx)
			if err != nil {
				t.Fatal(err)
			}
		}

		previous, err := unitReference(ctx, "incudal-xray.service")
		if err != nil {
			t.Fatal(err)
		}
		_ = exec.CommandContext(ctx, "systemctl", "reset-failed", "incudal-xray.service").Run()
		command("systemctl", "restart", "incudal-xray.service")
		time.Sleep(300 * time.Millisecond)
		command("systemctl", "is-active", "--quiet", "incudal-xray.service")
		current, err := unitReference(ctx, "incudal-xray.service")
		if err != nil || current == previous {
			t.Fatal("restarted core did not expose a fresh invocation identity")
		}
		out, err := networkCommand(ctx, "ip", "netns", "exec", namespace, "ss", "-H", "-lnt").CombinedOutput()
		if err != nil || !strings.Contains(string(out), ":15490") || coreReady(ctx, coreAPIPath(stateDir), nil) != nil {
			t.Fatalf("restarted core has no listeners: %s %v", out, err)
		}

		return
	}

	if mode == "occupied" {
		{
			err := os.MkdirAll("/run/netns", 0o755)
			if err != nil {
				t.Fatal(err)
			}
		}
		{
			err := os.WriteFile(namespacePath, []byte("unrelated"), 0o600)
			if err != nil {
				t.Fatal(err)
			}
		}
		{
			err := ensureHostNamespace(ctx)
			if err == nil {
				t.Fatal("non-empty path was accepted")
			}
		}

		data, err := os.ReadFile("/proc/1/root" + namespacePath)
		if err != nil || string(data) != "unrelated" {
			t.Fatal("unrelated file was changed")
		}
		{
			err := os.Remove("/proc/1/root" + namespacePath)
			if err != nil {
				t.Fatal(err)
			}
		}

		return
	}

	var originalNamespace os.FileInfo
	if mode == "legacy" {
		command("ip", "netns", "add", namespace) // Reproduce v1.2.6's private mount.
		command("ip", "netns", "exec", namespace, "true")
		var err error
		originalNamespace, err = os.Stat(namespacePath)
		if err != nil {
			t.Fatal(err)
		}

		if networkCommand(ctx, "ip", "netns", "exec", namespace, "true").Run() == nil {
			t.Fatal("fixture did not isolate the mount")
		}
	}

	if mode == "stale" {
		{
			err := os.MkdirAll("/run/netns", 0o755)
			if err != nil {
				t.Fatal(err)
			}
		}
		{
			err := os.WriteFile(namespacePath, nil, 0o600)
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	dir := "/var/lib/incudal-agent/network"
	{
		err := os.MkdirAll(dir, 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}

	binary, err := extractBinary(dir)
	if err != nil {
		t.Fatal(err)
	}

	in := testInstruction()
	in.Egress.Mode = "direct"
	in.Nodes = nil
	body, err := xrayConfig(in, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "xray.json")
	{
		err := atomicFile(path+".next", body, 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		err := validateCore(ctx, binary, path+".next")
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		err := os.Rename(path+".next", path)
		if err != nil {
			t.Fatal(err)
		}
	}

	unit := gatewayUnit(binary, path, false)
	{
		err := os.WriteFile("/etc/systemd/system/incudal-xray.service", []byte(unit), 0o644)
		if err != nil {
			t.Fatal(err)
		}
	}

	command("systemctl", "daemon-reload")
	// Host hooks need their real nftables readiness set, even though this kernel
	// lacks nft_tproxy. No parser mocks or stand-in core commands are used here.
	nft := exec.CommandContext(ctx, "nft", "-f", "-")
	nft.Stdin = strings.NewReader("table inet incudal_egress {\n set ready { type mark; }\n}\n")
	{
		out, err := nft.CombinedOutput()
		if err != nil {
			t.Fatalf("readiness fixture: %s %v", out, err)
		}
	}

	if mode == "legacy" {
		_ = exec.CommandContext(ctx, "systemctl", "start", "incudal-xray.service").Run()
		time.Sleep(200 * time.Millisecond)
		code := strings.TrimSpace(command("systemctl", "show", "--value", "-p", "ExecMainStatus", "incudal-xray.service"))
		command("systemctl", "stop", "incudal-xray.service")
		if code != "255" {
			t.Fatalf("expected exact reported startup failure 255, got %s", code)
		}
	}
	{
		err := ensureHostNamespace(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}

	if originalNamespace != nil {
		published, err := os.Stat("/proc/1/root" + namespacePath)
		if err != nil || !os.SameFile(originalNamespace, published) {
			t.Fatal("upgrade recreated the namespace instead of retaining its veth/route state")
		}
	}
	// Address/loopback setup uses the same canonical command builder as production.
	for _, args := range [][]string{{"link", "set", "lo", "up"}, {"addr", "replace", "169.254.242.2/32", "dev", "lo"}} {
		{
			err := run(ctx, "ip", append([]string{"netns", "exec", namespace, "ip"}, args...)...)
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	command("systemctl", "start", "incudal-xray.service")
	time.Sleep(300 * time.Millisecond)
	command("systemctl", "is-active", "--quiet", "incudal-xray.service")
	sockets, err := networkCommand(ctx, "ip", "netns", "exec", namespace, "ss", "-H", "-lntup").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}

	for _, required := range []string{":53", ":15490"} {
		if !strings.Contains(string(sockets), required) {
			t.Fatalf("missing native Xray socket %s: %s", required, sockets)
		}
	}

	{
		err := coreReady(ctx, coreAPIPath(stateDir), []string{"direct"})
		if err != nil {
			t.Fatal(err)
		}
	}
	set := command("nft", "list", "set", "inet", "incudal_egress", "ready")
	if strings.Contains(set, "0x00420000") || strings.Contains(set, "0x420000") {
		t.Fatalf("API startup alone opened the gateway: %s", set)
	}

	t.Logf("real systemd/Xray direct startup succeeded after %s namespace recovery", mode)
	if mode != "legacy" {
		return
	}
	// Exercise the actual ip-netns bind mount: replacing its source file does
	// not update a running core's resolver. A restart must pick up the new file.
	oldResolver := []byte("nameserver 192.0.2.53\n")
	newResolver := []byte("nameserver 203.0.113.53\n")
	{
		err := atomicFile(namespaceResolverPath, oldResolver, 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	command("systemctl", "reset-failed", "incudal-xray.service")
	command("systemctl", "restart", "incudal-xray.service")
	resolverInsideCore := func() string {
		t.Helper()
		pid := strings.TrimSpace(command("systemctl", "show", "--value", "-p", "MainPID", "incudal-xray.service"))
		body, err := os.ReadFile("/proc/" + pid + "/root/etc/resolv.conf")
		if err != nil {
			t.Fatal(err)
		}

		return string(body)
	}
	{
		err := atomicFile(namespaceResolverPath, newResolver, 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	if resolverInsideCore() != string(oldResolver) {
		t.Fatal("fixture did not reproduce a stale resolver bind mount")
	}

	command("nft", "add", "element", "inet", "incudal_egress", "ready", "{", mark, "}")
	command("systemctl", "reset-failed", "incudal-xray.service")
	command("systemctl", "restart", "incudal-xray.service")
	if resolverInsideCore() != string(newResolver) {
		t.Fatal("core restart did not pick up new bootstrap DNS")
	}
	// A restore job is ordered before Xray. Its restart must be queued, since
	// waiting synchronously would deadlock both systemd jobs. Use the real core
	// and readiness hooks to verify the queued job finishes after restore exits.
	const orderUnit = "incudal-restore-order-fixture.service"
	orderBody := "[Unit]\nBefore=incudal-xray.service\n[Service]\nType=oneshot\nExecStartPre=/usr/sbin/nft flush set inet incudal_egress ready\nExecStart=/usr/bin/systemctl --no-block restart incudal-xray.service\n"
	{
		err := os.WriteFile("/etc/systemd/system/"+orderUnit, []byte(orderBody), 0o644)
		if err != nil {
			t.Fatal(err)
		}
	}

	command("systemctl", "daemon-reload")
	command("systemctl", "reset-failed", "incudal-xray.service")
	command("systemctl", "start", orderUnit)
	for attempt := 0; attempt < 30; attempt++ {
		if coreReady(ctx, coreAPIPath(stateDir), nil) == nil && networkCommand(ctx, "systemctl", "is-active", "--quiet", "incudal-xray.service").Run() == nil {
			break
		}
		if attempt == 29 {
			t.Fatal("queued core restart never restored API readiness")
		}
		time.Sleep(100 * time.Millisecond)
	}
	set = command("nft", "list", "set", "inet", "incudal_egress", "ready")
	if strings.Contains(set, "0x00420000") || strings.Contains(set, "0x420000") {
		t.Fatal("queued restart opened traffic without data-plane checks")
	}

	// Exercise the actual service/API startup for every offered proxy strategy.
	// Closed local upstreams keep the fixture offline and must never enable direct
	// fallback. These tests leave the unit restored to zero-node direct mode.
	for _, strategy := range []string{"failover", "balance", "leastPing", "direct"} {
		candidate := testInstruction()
		candidate.Egress.Strategy = strategy
		candidate.Egress.Mode = "proxy"
		if strategy == "direct" {
			candidate.Egress.Mode = "direct"
			candidate.Egress.Strategy = "failover"
			candidate.Nodes = nil
		}

		config, e := xrayConfig(candidate, nil, true)
		if e != nil {
			t.Fatal(e)
		}
		{
			e = atomicFile(path+".next", config, 0o600)
			if e != nil {
				t.Fatal(e)
			}
		}
		{
			e = validateCore(ctx, binary, path+".next")
			if e != nil {
				t.Fatal(e)
			}
		}
		{
			e = os.Rename(path+".next", path)
			if e != nil {
				t.Fatal(e)
			}
		}
		{
			e = os.WriteFile("/etc/systemd/system/incudal-xray.service", []byte(gatewayUnit(binary, path, strategy == "failover")), 0o644)
			if e != nil {
				t.Fatal(e)
			}
		}

		command("systemctl", "daemon-reload")
		_ = exec.CommandContext(ctx, "systemctl", "reset-failed", "incudal-xray.service").Run()
		command("systemctl", "restart", "incudal-xray.service")
		time.Sleep(300 * time.Millisecond)
		command("systemctl", "is-active", "--quiet", "incudal-xray.service")
		t.Logf("real systemd/Xray startup passed strategy=%s", strategy)
	}
}

func TestNetworkNamespaceHandleType(t *testing.T) {
	for _, item := range []struct {
		path string
		want bool
	}{{"/proc/self/ns/net", true}, {"/proc/self/ns/mnt", false}} {
		file, err := os.Open(item.path)
		if err != nil {
			t.Fatal(err)
		}

		got := isNetworkNamespace(file)
		_ = file.Close()
		if got != item.want {
			t.Fatalf("namespace handle type %s: got %v", item.path, got)
		}
	}

	file, err := os.CreateTemp(t.TempDir(), "placeholder")
	if err != nil {
		t.Fatal(err)
	}

	defer file.Close()
	if isNetworkNamespace(file) {
		t.Fatal("ordinary placeholder mistaken for netns")
	}
}
