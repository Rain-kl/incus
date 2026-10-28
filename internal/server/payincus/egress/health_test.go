package egress

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestGatewayDNSRequiresTCPAndUDP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	packet, err := net.ListenPacket("udp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	var refuse atomic.Bool
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		requests.Add(1)
		response := new(dns.Msg)
		response.SetReply(q)
		if refuse.Load() {
			response.Rcode = dns.RcodeServerFailure
		}
		_ = w.WriteMsg(response)
	})
	tcp := &dns.Server{Listener: listener, Handler: handler}
	udp := &dns.Server{PacketConn: packet, Handler: handler}
	go func() { _ = tcp.ActivateAndServe() }()
	go func() { _ = udp.ActivateAndServe() }()
	t.Cleanup(func() { _ = tcp.Shutdown(); _ = udp.Shutdown() })
	{
		err := probeDNS(context.Background(), listener.Addr().String(), "example.com")
		if err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 2 {
		t.Fatal("both DNS transports must be checked")
	}
	refuse.Store(true)
	{
		err := probeDNS(context.Background(), listener.Addr().String(), "example.com")
		if err == nil {
			t.Fatal("failing DNS accepted")
		}
	}
	refuse.Store(false)
	_ = udp.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	{
		err := probeDNS(ctx, listener.Addr().String(), "example.com")
		if err == nil {
			t.Fatal("TCP-only DNS accepted as ready")
		}
	}
	{
		err := probeDNSForURL(context.Background(), listener.Addr().String(), "http://example.com")
		if err == nil {
			t.Fatal("invalid health target accepted")
		}
	}
}

func TestReadinessRequiresCurrentServiceInstance(t *testing.T) {
	reference := "0123456789abcdef0123456789abcdef"
	valid := "ActiveState=active\nSubState=running\nInvocationID=" + reference + "\n"
	actual, err := parseUnitReference([]byte(valid))
	if err != nil || actual != reference {
		t.Fatal("valid systemd identity rejected")
	}
	for _, data := range []string{
		strings.Replace(valid, "active", "activating", 1), strings.Replace(valid, "running", "auto-restart", 1),
		strings.Replace(valid, reference, "", 1), strings.Replace(valid, reference, strings.Repeat("0", 32), 1), strings.Replace(valid, reference, strings.Repeat("x", 32), 1),
	} {
		{
			_, err := parseUnitReference([]byte(data))
			if err == nil {
				t.Fatal("inactive or invalid service identity accepted")
			}
		}
	}
}
