package muse

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
)

// decodeStream unwraps a Content-Encoding the plugin asked for (profile
// Accept-Encoding: gzip,deflate,br) without buffering the stream. The host
// transport only decompresses responses whose Accept-Encoding it chose itself.
func decodeStream(headers http.Header, body io.ReadCloser) (io.ReadCloser, error) {
	encoding := strings.ToLower(strings.TrimSpace(headers.Get("Content-Encoding")))
	var reader io.Reader
	var decoder io.Closer
	switch encoding {
	case "", "identity":
		return body, nil
	case "gzip":
		value, err := gzip.NewReader(body)
		if err != nil {
			return nil, fmt.Errorf("meta: invalid gzip response: %w", err)
		}
		reader, decoder = value, value
	case "br":
		reader = brotli.NewReader(body)
	case "deflate":
		buffered := bufio.NewReader(body)
		prefix, err := buffered.Peek(2)
		if err != nil {
			return nil, fmt.Errorf("meta: invalid deflate response: %w", err)
		}
		if prefix[0]&0x0f == 8 && (int(prefix[0])*256+int(prefix[1]))%31 == 0 {
			value, errZlib := zlib.NewReader(buffered)
			if errZlib != nil {
				return nil, fmt.Errorf("meta: invalid deflate response: %w", errZlib)
			}
			reader, decoder = value, value
		} else {
			value := flate.NewReader(buffered)
			reader, decoder = value, value
		}
	default:
		return nil, fmt.Errorf("meta: unsupported response content encoding %q", encoding)
	}
	headers.Del("Content-Encoding")
	headers.Del("Content-Length")
	return &decodedBody{Reader: reader, decoder: decoder, original: body}, nil
}

// decodeBody decodes a buffered body.
func decodeBody(headers http.Header, body []byte) ([]byte, error) {
	if enc := strings.ToLower(strings.TrimSpace(headers.Get("Content-Encoding"))); enc == "" || enc == "identity" {
		return body, nil
	}
	reader, err := decodeStream(headers, io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

type decodedBody struct {
	io.Reader
	decoder  io.Closer
	original io.Closer
}

func (b *decodedBody) Close() error {
	if b.decoder != nil {
		_ = b.decoder.Close()
	}
	return b.original.Close()
}
