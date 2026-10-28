package egress

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

func mockCoreAPI(t *testing.T, call func(context.Context, string, coreWire) (coreWire, error)) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "core-api-")
	if err != nil {
		t.Fatal(err)
	}
	path := coreAPIPath(dir)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.ForceServerCodec(coreCodec{}), grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(stream)
		var request coreWire
		{
			err := stream.RecvMsg(&request)
			if err != nil {
				return err
			}
		}
		reply, err := call(stream.Context(), method, request)
		if err != nil {
			return err
		}
		return stream.SendMsg(&reply)
	}))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { closeCoreAPI(path); server.Stop(); _ = listener.Close(); _ = os.RemoveAll(dir) })
	return path
}

func tagResponse(tags ...string) coreWire {
	var body coreWire
	for _, tag := range tags {
		message := wireString(nil, 1, tag)
		body = protowire.AppendTag(body, 1, protowire.BytesType)
		body = protowire.AppendBytes(body, message)
	}
	return body
}

func TestCoreAPIContracts(t *testing.T) {
	var lock sync.Mutex
	target := "blocked"
	path := mockCoreAPI(t, func(_ context.Context, method string, request coreWire) (coreWire, error) {
		lock.Lock()
		defer lock.Unlock()
		switch method {
		case coreAPIService + "ListInbounds":
			if string(request) != string([]byte{8, 1}) {
				t.Error("inbound request must exclude settings")
			}
			return tagResponse("api-in", "transparent", "dns-forward"), nil
		case coreAPIService + "ListOutbounds":
			if len(request) != 0 {
				t.Error("pinned ListOutbounds request must be empty")
			}
			return tagResponse("blocked", "proxy-7-"), nil
		case coreRoutingService + "OverrideBalancerTarget":
			if string(request) != string(wireString(wireString(nil, 1, "egress"), 2, "proxy-7-")) {
				t.Error("invalid balancer request")
			}
			target = "proxy-7-"
			return nil, nil
		case coreRoutingService + "GetBalancerInfo":
			if string(request) != string(wireString(nil, 1, "egress")) {
				t.Error("invalid read-back request")
			}
			override := wireString(nil, 2, target)
			balancer := protowire.AppendBytes(protowire.AppendTag(nil, 5, protowire.BytesType), override)
			return protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), balancer), nil
		default:
			return nil, status.Error(codes.Unimplemented, "unsupported")
		}
	})
	{
		err := coreReady(context.Background(), path, []string{"proxy-7-"})
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		err := overrideCore(context.Background(), path, "proxy-7-")
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		err := coreReady(context.Background(), path, []string{"proxy-99-"})
		if err == nil {
			t.Fatal("unknown outbound accepted")
		}
	}
	t.Run("unimplemented-and-secret-redaction", func(t *testing.T) {
		for _, code := range []codes.Code{codes.Unimplemented, codes.PermissionDenied} {
			socket := mockCoreAPI(t, func(context.Context, string, coreWire) (coreWire, error) {
				return nil, status.Error(code, "fixture-secret-credential")
			})
			_, err := coreTags(context.Background(), socket, false)
			if err == nil || strings.Contains(err.Error(), "fixture-secret") {
				t.Fatal("raw core error escaped")
			}
		}
	})
	t.Run("cancel-and-deadline", func(t *testing.T) {
		socket := mockCoreAPI(t, func(ctx context.Context, _ string, _ coreWire) (coreWire, error) { <-ctx.Done(); return nil, ctx.Err() })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := coreTags(ctx, socket, false)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline lost: %v", err)
		}
		ctx, stop := context.WithCancel(context.Background())
		stop()
		_, err = coreTags(ctx, socket, false)
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation lost")
		}
	})
	t.Run("malformed-and-missing-features", func(t *testing.T) {
		for _, body := range []coreWire{{0xff}, nil, tagResponse("direct"), tagResponse("blocked", "blocked")} {
			socket := mockCoreAPI(t, func(context.Context, string, coreWire) (coreWire, error) { return body, nil })
			{
				err := coreReady(context.Background(), socket, nil)
				if err == nil {
					t.Fatal("invalid response accepted")
				}
			}
		}
	})
	t.Run("write-acknowledgement-requires-readback", func(t *testing.T) {
		socket := mockCoreAPI(t, func(_ context.Context, method string, _ coreWire) (coreWire, error) {
			if method == coreRoutingService+"OverrideBalancerTarget" {
				return nil, nil
			}
			if method == coreRoutingService+"GetBalancerInfo" {
				override := wireString(nil, 2, "blocked")
				balancer := protowire.AppendBytes(protowire.AppendTag(nil, 5, protowire.BytesType), override)
				return protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), balancer), nil
			}
			return nil, status.Error(codes.Unimplemented, "unexpected")
		})
		{
			err := overrideCore(context.Background(), socket, "proxy-7-")
			if err == nil {
				t.Fatal("successful write without matching readback was accepted")
			}
		}
	})

	t.Run("socket-path-validation", func(t *testing.T) {
		{
			_, err := coreClient(filepath.Join(t.TempDir(), "missing"))
			if err == nil {
				t.Fatal("missing socket accepted")
			}
		}
		file := filepath.Join(t.TempDir(), "ordinary")
		_ = os.WriteFile(file, nil, 0o600)
		{
			_, err := coreClient(file)
			if err == nil {
				t.Fatal("ordinary file accepted as socket")
			}
		}
	})
}
