package outbound

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/sagernet/sing-box/common/tls"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/transport/v2raywebsocket"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	"github.com/sagernet/sing/common/bufio/deadline"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	sHTTP "github.com/sagernet/sing/protocol/http"
	"github.com/sagernet/ws"
)

// Keep the upstream WebSocket framing while preserving data read together with
// HTTP 101. A newly allocated sing buffer has Len()==0; the buffered byte count
// must be captured before allocating it, rather than using that initial length.
func dialWebsocketUpgrade(ctx context.Context, dialer N.Dialer, server M.Socksaddr, options option.V2RayWebsocketOptions, tlsConfig tls.Config) (result net.Conn, err error) {
	requestURL := url.URL{Scheme: "ws", Host: server.String()}
	if tlsConfig != nil {
		requestURL.Scheme = "wss"
		if len(tlsConfig.NextProtos()) == 0 {
			tlsConfig = tlsConfig.Clone()
			tlsConfig.SetNextProtos([]string{"http/1.1"})
		}
	}
	if err := sHTTP.URLSetPath(&requestURL, options.Path); err != nil {
		return nil, fmt.Errorf("parse WebSocket path: %w", err)
	}
	if !strings.HasPrefix(requestURL.Path, "/") {
		requestURL.Path = "/" + requestURL.Path
	}
	headers := options.Headers.Build().Clone()
	if host := headers.Get("Host"); host != "" {
		headers.Del("Host")
		requestURL.Host = host
	}
	if headers.Get("User-Agent") == "" {
		headers.Set("User-Agent", "Go-http-client/1.1")
	}
	var protocols []string
	if protocol := headers.Get("Sec-WebSocket-Protocol"); protocol != "" {
		protocols = []string{protocol}
		headers.Del("Sec-WebSocket-Protocol")
	}
	ctx, cancel := context.WithTimeout(ctx, C.TCPTimeout)
	defer cancel()
	raw, err := dialer.DialContext(ctx, N.NetworkTCP, server)
	if err != nil {
		if raw != nil {
			raw.Close()
		}
		return nil, err
	}
	// Keep ownership of the raw socket throughout TLS and HTTP upgrade. After
	// success the caller owns it, independently of this short-lived context.
	interrupted := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		defer close(interrupted)
		raw.Close()
	})
	defer func() {
		if !stopCancel() {
			<-interrupted
		}
		if ctx.Err() != nil {
			result, err = nil, ctx.Err()
		}
		if err != nil {
			raw.Close()
		}
	}()
	until, _ := ctx.Deadline()
	_ = raw.SetDeadline(until)
	stream := raw
	if tlsConfig != nil {
		stream, err = tls.ClientHandshake(ctx, stream, tlsConfig)
		if err != nil {
			return nil, err
		}
	}
	upgradeConn := stream
	if deadline.NeedAdditionalReadDeadline(stream) {
		upgradeConn = deadline.NewConn(stream)
	}
	_ = upgradeConn.SetDeadline(until)
	reader, _, err := (ws.Dialer{Header: ws.HandshakeHeaderHTTP(headers), Protocols: protocols}).Upgrade(upgradeConn, &requestURL)
	_ = upgradeConn.SetDeadline(time.Time{})
	_ = raw.SetDeadline(time.Time{})
	if err != nil {
		return nil, err
	}
	if reader != nil && reader.Buffered() > 0 {
		count := reader.Buffered()
		buffer := buf.NewSize(count)
		if _, err := buffer.ReadFullFrom(reader, count); err != nil {
			buffer.Release()
			return nil, err
		}
		stream = bufio.NewCachedConn(stream, buffer)
	}
	return v2raywebsocket.NewConn(stream, nil, ws.StateClientSide), nil
}
