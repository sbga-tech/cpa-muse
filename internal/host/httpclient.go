package host

import (
	"context"
	"io"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// HTTPClient restores the request-scoped host client at the dynamic ABI boundary.
// An empty callback id yields a plugin-scoped client whose traffic the host does
// not attribute to, or record in the logs of, any downstream request.
// In-process executors instead receive the host's client directly.
func HTTPClient(callbackID string) pluginapi.HostHTTPClient {
	return callbackHTTPClient{client: NewClient(callbackID)}
}

type callbackHTTPClient struct{ client Client }

func (c callbackHTTPClient) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	resp, err := c.client.Do(ctx, Request{Method: req.Method, URL: req.URL, Headers: req.Headers, Body: req.Body, WireProfile: req.WireProfile})
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	return pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: resp.Body}, nil
}

func (c callbackHTTPClient) DoStream(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	resp, err := c.client.DoStream(ctx, Request{Method: req.Method, URL: req.URL, Headers: req.Headers, Body: req.Body, WireProfile: req.WireProfile})
	if err != nil {
		return pluginapi.HTTPStreamResponse{}, err
	}
	chunks := make(chan pluginapi.HTTPStreamChunk)
	go func() {
		defer close(chunks)
		defer resp.Body.Close()
		stop := context.AfterFunc(ctx, func() { resp.Body.Close() })
		defer stop()
		for {
			buf := make([]byte, 32*1024)
			n, err := resp.Body.Read(buf)
			if n > 0 || (err != nil && err != io.EOF) {
				chunk := pluginapi.HTTPStreamChunk{Payload: buf[:n]}
				if err != io.EOF {
					chunk.Err = err
				}
				select {
				case chunks <- chunk:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return pluginapi.HTTPStreamResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Chunks: chunks}, nil
}

type channelBody struct {
	ctx     context.Context
	cancel  context.CancelFunc
	chunks  <-chan pluginapi.HTTPStreamChunk
	pending []byte
	err     error
	once    sync.Once
}

func (b *channelBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if len(b.pending) > 0 {
			n := copy(p, b.pending)
			b.pending = b.pending[n:]
			return n, nil
		}
		if b.err != nil {
			return 0, b.err
		}
		select {
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		case chunk, ok := <-b.chunks:
			if !ok {
				b.err = io.EOF
				continue
			}
			b.pending, b.err = chunk.Payload, chunk.Err
		}
	}
}

func (b *channelBody) Close() error { b.once.Do(b.cancel); return nil }
