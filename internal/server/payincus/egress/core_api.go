package egress

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protowire"
)

// The small wire adapter follows Xray v26.3.27's published proto contracts without linking its runtime.
const (
	coreAPIService     = "/xray.app.proxyman.command.HandlerService/"
	coreRoutingService = "/xray.app.router.command.RoutingService/"
)

var (
	errCoreAPI  = errors.New("proxy control API unavailable")
	coreClients = struct {
		sync.Mutex
		connections map[string]*grpc.ClientConn
	}{connections: map[string]*grpc.ClientConn{}}
)

type coreWire []byte

type coreCodec struct{}

// Name selects the standard protobuf content type on the wire.
func (coreCodec) Name() string { return "proto" }

// Marshal sends only the explicitly constructed control request.
func (coreCodec) Marshal(value any) ([]byte, error) {
	data, ok := value.(*coreWire)
	if !ok {
		return nil, errCoreAPI
	}
	return *data, nil
}

// Unmarshal retains a bounded response for selective tag decoding.
func (coreCodec) Unmarshal(data []byte, value any) error {
	target, ok := value.(*coreWire)
	if !ok {
		return errCoreAPI
	}
	*target = append((*target)[:0], data...)
	return nil
}

func coreAPIPath(dir string) string { return filepath.Join(dir, "api.sock") }

func closeCoreAPI(path string) {
	coreClients.Lock()
	defer coreClients.Unlock()
	conn := coreClients.connections[path]
	delete(coreClients.connections, path)
	if conn != nil {
		_ = conn.Close()
	}
}

func coreClient(path string) (*grpc.ClientConn, error) {
	if !filepath.IsAbs(path) {
		return nil, errCoreAPI
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return nil, errCoreAPI
	}
	coreClients.Lock()
	defer coreClients.Unlock()
	{
		conn := coreClients.connections[path]
		if conn != nil {
			return conn, nil
		}
	}
	// There are at most 63 package groups and one host gateway; bound stale diagnostic clients too.
	if len(coreClients.connections) >= 128 {
		return nil, errCoreAPI
	}
	conn, err := grpc.NewClient("passthrough:///xray-local", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithNoProxy(), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}), grpc.WithDefaultCallOptions(grpc.ForceCodec(coreCodec{}), grpc.MaxCallRecvMsgSize(2<<20)))
	if err != nil {
		return nil, errCoreAPI
	}
	coreClients.connections[path] = conn
	return conn, nil
}

func coreCall(ctx context.Context, path, method string, request coreWire) (coreWire, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	conn, err := coreClient(path)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var response coreWire
	err = conn.Invoke(callCtx, method, &request, &response, grpc.WaitForReady(true))
	if err != nil {
		if callCtx.Err() != nil {
			return nil, callCtx.Err()
		}
		// Remote errors may contain configuration secrets; never propagate their text.
		return nil, errCoreAPI
	}
	return response, nil
}

func wireString(data coreWire, number protowire.Number, value string) coreWire {
	data = protowire.AppendTag(data, number, protowire.BytesType)
	return protowire.AppendString(data, value)
}

func wireFields(data []byte, visit func(protowire.Number, []byte) error) error {
	for len(data) > 0 {
		number, kind, n := protowire.ConsumeTag(data)
		if n < 0 || number < 1 {
			return errCoreAPI
		}
		data = data[n:]
		if kind == protowire.BytesType {
			value, size := protowire.ConsumeBytes(data)
			if size < 0 {
				return errCoreAPI
			}
			{
				err := visit(number, value)
				if err != nil {
					return err
				}
			}
			data = data[size:]
		} else {
			size := protowire.ConsumeFieldValue(number, kind, data)
			if size < 0 {
				return errCoreAPI
			}
			data = data[size:]
		}
	}
	return nil
}

func coreTags(ctx context.Context, path string, inbound bool) (map[string]bool, error) {
	method := "ListOutbounds"
	if inbound {
		method = "ListInbounds"
	}
	// The pinned ListOutbounds request is empty; only ListInbounds supports isOnlyTags.
	// Decode tags only and never log the bounded outbound response, which can contain settings.
	var request coreWire
	if inbound {
		request = coreWire{8, 1}
	}
	body, err := coreCall(ctx, path, coreAPIService+method, request)
	if err != nil {
		return nil, err
	}
	tags := map[string]bool{}
	err = wireFields(body, func(number protowire.Number, handler []byte) error {
		if number != 1 {
			return nil
		}
		if len(tags) >= 128 {
			return errCoreAPI
		}
		return wireFields(handler, func(field protowire.Number, value []byte) error {
			if field == 1 {
				if len(value) == 0 || len(value) > 128 || tags[string(value)] {
					return errCoreAPI
				}
				tags[string(value)] = true
			}
			return nil
		})
	})
	if err != nil || len(tags) == 0 {
		return nil, errCoreAPI
	}
	return tags, nil
}

func coreReady(ctx context.Context, path string, expected []string) error {
	outbounds, err := coreTags(ctx, path, false)
	if err != nil {
		return err
	}
	for _, tag := range append([]string{"blocked"}, expected...) {
		if !outbounds[tag] {
			return errCoreAPI
		}
	}
	inbounds, err := coreTags(ctx, path, true)
	if err != nil {
		return err
	}
	if !inbounds["transparent"] || !inbounds["dns-forward"] || !inbounds["api-in"] {
		return errCoreAPI
	}
	return nil
}

func overrideCore(ctx context.Context, path, target string) error {
	request := wireString(wireString(nil, 1, "egress"), 2, target)
	_, err := coreCall(ctx, path, coreRoutingService+"OverrideBalancerTarget", request)
	if err != nil {
		return err
	}
	body, err := coreCall(ctx, path, coreRoutingService+"GetBalancerInfo", wireString(nil, 1, "egress"))
	if err != nil {
		return err
	}
	found := false
	err = wireFields(body, func(field protowire.Number, balancer []byte) error {
		if field != 1 {
			return nil
		}
		return wireFields(balancer, func(field protowire.Number, override []byte) error {
			if field != 5 {
				return nil
			}
			return wireFields(override, func(field protowire.Number, value []byte) error {
				if field == 2 {
					found = string(value) == target
				}
				return nil
			})
		})
	})
	if err != nil || !found {
		return errCoreAPI
	}
	return nil
}

// WaitCoreAPI is used by systemd before a gateway can be considered started; it never opens the firewall.
func WaitCoreAPI(ctx context.Context, path string, failover bool) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := coreReady(ctx, path, nil)
		if err == nil && failover {
			err = overrideCore(ctx, path, "blocked")
		}
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
